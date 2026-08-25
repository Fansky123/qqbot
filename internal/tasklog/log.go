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
)

var (
	taskIDPattern = regexp.MustCompile(`^T-[A-F0-9]{12}$`)
	streamPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	bearerPattern = regexp.MustCompile(`(?i)(\b(?:authorization[ \t]*:[ \t]*)?bearer[ \t]+)[^\s,;]+`)
	keyPattern    = regexp.MustCompile(`(?i)\b(CODEX_API_KEY|OPENAI_API_KEY|NAPCAT_ACCESS_TOKEN)=[^\s"'` + "`" + `,;]+`)
)

// Store owns append-only, redacted JSONL records beneath one private directory.
type Store struct {
	Root string

	secrets []string
	rootDev uint64
	rootIno uint64
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
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
	seenSecrets := make(map[string]struct{}, len(secretValues))
	for _, value := range secretValues {
		if len(value) > maxRedactionOverlap {
			return nil, errors.New("task log secret exceeds maximum length")
		}
		if value == "" {
			continue
		}
		if _, exists := seenSecrets[value]; exists {
			continue
		}
		seenSecrets[value] = struct{}{}
		secrets = append(secrets, value)
	}
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })

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
		Root:    canonical,
		secrets: secrets,
		rootDev: uint64(stat.Dev),
		rootIno: stat.Ino,
		locks:   make(map[string]*sync.Mutex),
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
	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	file, err := openTaskFile(rootFD, taskID, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return errors.Join(err, closeRoot(rootFD))
	}
	_, writeErr := file.Write(entry)
	syncErr := file.Sync()
	closeErr := file.Close()
	rootCloseErr := unix.Close(rootFD)
	if writeErr != nil {
		writeErr = fmt.Errorf("append task log: %w", writeErr)
	}
	if syncErr != nil {
		syncErr = fmt.Errorf("sync task log: %w", syncErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close task log: %w", closeErr)
	}
	if rootCloseErr != nil {
		rootCloseErr = fmt.Errorf("close task log root: %w", rootCloseErr)
	}
	return errors.Join(writeErr, syncErr, closeErr, rootCloseErr)
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
	return closeRoot(rootFD)
}

func (s *Store) redact(value string) string {
	for _, secret := range s.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	value = bearerPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = keyPattern.ReplaceAllString(value, `${1}=[REDACTED]`)
	return value
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
