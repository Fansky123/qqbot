package tasklog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxSummaryReadBytes = 1 << 20
	// This is also the maximum configured secret length, measured in bytes.
	maxRedactionOverlap = 64 << 10
	minSecretBytes      = 8
	maxSecretEntries    = 256
	maxSecretBytes      = 512 << 10
	maxRecordDataBytes  = 2 << 20
	maxTaskLogBytes     = 64 << 20
	redactionMarker     = "[REDACTED]"
)

// ErrLimitExceeded reports that a record or task log reached its hard limit.
var ErrLimitExceeded = errors.New("task log limit exceeded")

var taskLimitRecord = []byte(`{"time":"1970-01-01T00:00:00Z","stream":"tasklog.limit","data":"task log output limit reached"}` + "\n")

var (
	taskIDPattern = regexp.MustCompile(`^[TQ]-[A-F0-9]{12}$`)
	streamPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	bearerPattern = regexp.MustCompile(`(?i)(\b(?:authorization[ \t]*:[ \t]*)?bearer[ \t]+)[^\s,;]+`)
	keyPattern    = regexp.MustCompile(`(?i)\b(CODEX_API_KEY|OPENAI_API_KEY|NAPCAT_ACCESS_TOKEN)=[^\s"'` + "`" + `,;]+`)
)

// Store owns append-only, redacted JSONL records beneath one private directory.
type Store struct {
	Root string

	secrets     []string
	exactMarker string
	rootDev     uint64
	rootIno     uint64
	locksMu     sync.Mutex
	locks       map[string]*sync.Mutex
	limited     map[string]bool

	maxTaskBytes int64
}

type record struct {
	Time   string `json:"time"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

func Open(root string, secretValues []string) (*Store, error) {
	if root == "" {
		return nil, errors.New("task log root is required")
	}
	secrets := make([]string, 0, len(secretValues))
	seenSecrets := make(map[string]struct{}, len(secretValues)*2)
	addSecret := func(value string) {
		if value == "" {
			return
		}
		if _, exists := seenSecrets[value]; exists {
			return
		}
		seenSecrets[value] = struct{}{}
		secrets = append(secrets, value)
	}
	for _, value := range secretValues {
		if !utf8.ValidString(value) {
			return nil, errors.New("task log secret is not valid UTF-8")
		}
		if value != "" && len(value) < minSecretBytes {
			return nil, errors.New("task log secret is shorter than minimum length")
		}
		if len(value) > maxRedactionOverlap {
			return nil, errors.New("task log secret exceeds maximum length")
		}
		addSecret(value)
		if value == "" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, errors.New("encode task log secret failed")
		}
		addSecret(string(encoded[1 : len(encoded)-1]))
	}
	totalSecretBytes := 0
	for _, secret := range secrets {
		totalSecretBytes += len(secret)
	}
	if len(secrets) > maxSecretEntries || totalSecretBytes > maxSecretBytes {
		return nil, errors.New("task log secret configuration exceeds limit")
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	exactMarker := redactionMarker
	if len(secrets) > 0 {
		exactMarker = chooseExactMarker(secrets)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("make task log root absolute: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create task log root: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("secure task log root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve task log root: %w", err)
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		return nil, fmt.Errorf("secure task log root: %w", err)
	}
	fd, stat, err := openRoot(canonical)
	if err != nil {
		return nil, err
	}
	if closeErr := unix.Close(fd); closeErr != nil {
		return nil, fmt.Errorf("close task log root: %w", closeErr)
	}

	return &Store{
		Root:        canonical,
		secrets:     secrets,
		exactMarker: exactMarker,
		rootDev:     uint64(stat.Dev),
		rootIno:     stat.Ino,
		locks:       make(map[string]*sync.Mutex),
		limited:     make(map[string]bool),

		maxTaskBytes: maxTaskLogBytes,
	}, nil
}

func (s *Store) Append(taskID, stream string, data []byte) error {
	if err := validateTaskID(taskID); err != nil {
		return err
	}
	if !streamPattern.MatchString(stream) {
		return errors.New("invalid task log stream")
	}
	if s == nil {
		return errors.New("task log store is not initialized")
	}
	if len(data) > maxRecordDataBytes {
		return fmt.Errorf("task log record data exceeded limit: %w", ErrLimitExceeded)
	}

	entry, err := json.Marshal(record{
		Time:   time.Now().UTC().Format(time.RFC3339Nano),
		Stream: stream,
		Data:   s.redact(string(data)),
	})
	if err != nil {
		return fmt.Errorf("encode task log record: %w", err)
	}
	entry = append(entry, '\n')

	lock := s.taskLock(taskID)
	lock.Lock()
	defer lock.Unlock()
	if s.taskLimited(taskID) {
		return ErrLimitExceeded
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	file, err := openTaskFile(rootFD, taskID, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return errors.Join(err, closeRoot(rootFD))
	}
	lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX)
	var writeErr error
	if lockErr == nil {
		writeErr = s.appendBounded(taskID, file, entry)
	}
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	rootCloseErr := unix.Close(rootFD)
	if lockErr != nil {
		lockErr = fmt.Errorf("lock task log: %w", lockErr)
	}
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock task log: %w", unlockErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close task log: %w", closeErr)
	}
	if rootCloseErr != nil {
		rootCloseErr = fmt.Errorf("close task log root: %w", rootCloseErr)
	}
	return errors.Join(lockErr, writeErr, unlockErr, closeErr, rootCloseErr)
}

func (s *Store) appendBounded(taskID string, file *os.File, entry []byte) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat task log before append: %w", err)
	}
	size := info.Size()
	if size < 0 || s.maxTaskBytes <= int64(len(taskLimitRecord)) {
		s.setTaskLimited(taskID, true)
		return ErrLimitExceeded
	}
	limited, err := hasTaskLimitRecord(file, size)
	if err != nil {
		return err
	}
	if limited {
		s.setTaskLimited(taskID, true)
		return ErrLimitExceeded
	}
	usable := s.maxTaskBytes - int64(len(taskLimitRecord))
	if size > usable || int64(len(entry)) > usable-size {
		s.setTaskLimited(taskID, true)
		var markerErr error
		if size <= s.maxTaskBytes-int64(len(taskLimitRecord)) {
			markerErr = appendAndSync(file, size, taskLimitRecord)
		}
		return errors.Join(ErrLimitExceeded, markerErr)
	}
	return appendAndSync(file, size, entry)
}

func hasTaskLimitRecord(file *os.File, size int64) (bool, error) {
	if size < int64(len(taskLimitRecord)) {
		return false, nil
	}
	data := make([]byte, len(taskLimitRecord))
	if _, err := file.ReadAt(data, size-int64(len(data))); err != nil {
		return false, fmt.Errorf("inspect task log limit marker: %w", err)
	}
	return bytes.Equal(data, taskLimitRecord), nil
}

func appendAndSync(file *os.File, originalSize int64, data []byte) error {
	if err := writeAll(file, data); err != nil {
		return errors.Join(fmt.Errorf("append task log: %w", err), rollbackAppend(file, originalSize))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync task log: %w", err), rollbackAppend(file, originalSize))
	}
	return nil
}

func rollbackAppend(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("roll back partial task log append: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync rolled back task log: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return errors.New("task log writer returned invalid byte count")
		}
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (s *Store) Writer(taskID, stream string) io.Writer {
	return storeWriter{store: s, taskID: taskID, stream: stream}
}

func (s *Store) Summary(taskID string, maxRunes int) (string, error) {
	if err := validateTaskID(taskID); err != nil {
		return "", err
	}
	if maxRunes <= 0 {
		return "", errors.New("summary rune limit must be positive")
	}
	if s == nil {
		return "", errors.New("task log store is not initialized")
	}

	lock := s.taskLock(taskID)
	lock.Lock()
	defer lock.Unlock()
	rootFD, err := s.openRoot()
	if err != nil {
		return "", err
	}
	file, err := openTaskFile(rootFD, taskID, unix.O_RDONLY, 0)
	if err != nil {
		return "", errors.Join(err, closeRoot(rootFD))
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return "", errors.Join(fmt.Errorf("stat task log: %w", statErr), file.Close(), closeRoot(rootFD))
	}
	readSize := info.Size()
	if readSize > maxSummaryReadBytes+maxRedactionOverlap {
		readSize = maxSummaryReadBytes + maxRedactionOverlap
	}
	data := make([]byte, int(readSize))
	offset := info.Size() - readSize
	_, readErr := file.ReadAt(data, offset)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	startsAtRecord := offset == 0
	var boundaryErr error
	if offset > 0 {
		var previous [1]byte
		_, boundaryErr = file.ReadAt(previous[:], offset-1)
		startsAtRecord = boundaryErr == nil && previous[0] == '\n'
	}
	closeErr := file.Close()
	rootCloseErr := unix.Close(rootFD)
	if err := errors.Join(readErr, boundaryErr, closeErr, rootCloseErr); err != nil {
		return "", fmt.Errorf("read task log summary: %w", err)
	}

	if !startsAtRecord {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			data = nil
		} else {
			data = data[newline+1:]
		}
	}
	if lastNewline := bytes.LastIndexByte(data, '\n'); lastNewline < 0 {
		data = nil
	} else {
		data = data[:lastNewline+1]
	}
	redacted := string(s.sanitizeRecords(data))
	if utf8.RuneCountInString(redacted) <= maxRunes {
		return redacted, nil
	}
	runes := []rune(redacted)
	return string(runes[len(runes)-maxRunes:]), nil
}

func (s *Store) sanitizeRecords(data []byte) []byte {
	var output bytes.Buffer
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			break
		}
		line := data[:newline]
		data = data[newline+1:]
		if len(line) == 0 {
			continue
		}

		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var entry record
		// Tampered records are skipped; raw bytes are never returned to callers.
		if err := decoder.Decode(&entry); err != nil {
			continue
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.Time); err != nil || !streamPattern.MatchString(entry.Stream) {
			continue
		}
		entry.Data = s.redact(entry.Data)
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		_, _ = output.Write(encoded)
		_ = output.WriteByte('\n')
	}
	return output.Bytes()
}

func (s *Store) Remove(taskID string) error {
	if err := validateTaskID(taskID); err != nil {
		return err
	}
	if s == nil {
		return errors.New("task log store is not initialized")
	}
	lock := s.taskLock(taskID)
	lock.Lock()
	defer lock.Unlock()
	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	name := taskID + ".log"
	var stat unix.Stat_t
	if err := unix.Fstatat(rootFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			s.setTaskLimited(taskID, false)
			return closeRoot(rootFD)
		}
		return errors.Join(fmt.Errorf("inspect task log: %w", err), closeRoot(rootFD))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return errors.Join(errors.New("task log is not a private regular file"), closeRoot(rootFD))
	}
	if stat.Mode&0o777 != 0o600 {
		return errors.Join(errors.New("task log permissions are not private"), closeRoot(rootFD))
	}
	if err := unix.Unlinkat(rootFD, name, 0); err != nil {
		return errors.Join(fmt.Errorf("remove task log: %w", err), closeRoot(rootFD))
	}
	s.setTaskLimited(taskID, false)
	return closeRoot(rootFD)
}

func (s *Store) redact(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = bearerPattern.ReplaceAllString(value, `${1}`+s.exactMarker)
	value = keyPattern.ReplaceAllString(value, `${1}=`+s.exactMarker)
	return s.redactExact(value)
}

func (s *Store) redactExact(value string) string {
	if len(s.secrets) == 0 {
		return value
	}
	var redacted strings.Builder
	redacted.Grow(len(value))
	for offset := 0; offset < len(value); {
		matched := false
		for _, secret := range s.secrets {
			if strings.HasPrefix(value[offset:], secret) {
				redacted.WriteString(s.exactMarker)
				offset += len(secret)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if strings.HasPrefix(value[offset:], s.exactMarker) {
			redacted.WriteString(s.exactMarker)
			offset += len(s.exactMarker)
			continue
		}
		redacted.WriteByte(value[offset])
		offset++
	}
	return redacted.String()
}

func chooseExactMarker(secrets []string) string {
	for candidate := rune(0x2588); candidate <= utf8.MaxRune; candidate++ {
		if candidate >= 0xD800 && candidate <= 0xDFFF {
			continue
		}
		marker := string(candidate)
		available := true
		for _, secret := range secrets {
			if strings.Contains(secret, marker) {
				available = false
				break
			}
		}
		if available {
			return marker
		}
	}
	panic("task log secret marker space exhausted")
}

// RedactText removes configured exact secrets and recognized credentials from text.
func (s *Store) RedactText(value string) string {
	if s == nil {
		return redactionMarker
	}
	return s.redact(value)
}

func (s *Store) taskLock(taskID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock := s.locks[taskID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[taskID] = lock
	}
	return lock
}

func (s *Store) taskLimited(taskID string) bool {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	return s.limited[taskID]
}

func (s *Store) setTaskLimited(taskID string, limited bool) {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if limited {
		s.limited[taskID] = true
	} else {
		delete(s.limited, taskID)
	}
}

func (s *Store) openRoot() (int, error) {
	fd, stat, err := openRoot(s.Root)
	if err != nil {
		return -1, err
	}
	if uint64(stat.Dev) != s.rootDev || stat.Ino != s.rootIno {
		_ = unix.Close(fd)
		return -1, errors.New("task log root was replaced")
	}
	return fd, nil
}

func openRoot(root string) (int, unix.Stat_t, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open task log root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("stat task log root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, errors.New("task log root is not a directory")
	}
	if stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, errors.New("task log root permissions are not private")
	}
	return fd, stat, nil
}

func closeRoot(fd int) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close task log root: %w", err)
	}
	return nil
}

func openTaskFile(rootFD int, taskID string, flags int, mode uint32) (*os.File, error) {
	name := taskID + ".log"
	openFlags := flags | unix.O_CLOEXEC | unix.O_NOFOLLOW
	created := false
	var fd int
	var err error
	if flags&unix.O_CREAT != 0 {
		fd, err = unix.Openat(rootFD, name, openFlags|unix.O_EXCL, mode)
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(rootFD, name, openFlags&^unix.O_CREAT, mode)
		} else if err == nil {
			created = true
		}
	} else {
		fd, err = unix.Openat(rootFD, name, openFlags, mode)
	}
	if err != nil {
		return nil, fmt.Errorf("open task log: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat task log: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, errors.New("task log is not a private regular file")
	}
	if created {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("secure task log: %w", err)
		}
	} else if stat.Mode&0o777 != 0o600 {
		_ = unix.Close(fd)
		return nil, errors.New("task log permissions are not private")
	}
	return os.NewFile(uintptr(fd), filepath.Join("tasklog", name)), nil
}

func validateTaskID(taskID string) error {
	if !taskIDPattern.MatchString(taskID) {
		return errors.New("invalid task log task ID")
	}
	return nil
}

// storeWriter treats each Write call as one complete logical record. Callers
// that receive arbitrary process chunks must capture and frame them first.
type storeWriter struct {
	store  *Store
	taskID string
	stream string
}

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.store.Append(w.taskID, w.stream, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
