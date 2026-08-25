package tasklog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const testTaskID = "T-ABCDEF012345"

func TestStoreRejectsInvalidTaskIDsAndStreams(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)

	for _, taskID := range []string{"", "T-abcdef012345", "T-ABCDEF01234", "../T-ABCDEF012345"} {
		if err := store.Append(taskID, "codex.events", []byte("data")); err == nil {
			t.Fatalf("Append(%q) error = nil, want invalid task ID", taskID)
		}
	}
	for _, stream := range []string{"", "codex\nevents", "../events", strings.Repeat("x", 65)} {
		if err := store.Append(testTaskID, stream, []byte("data")); err == nil {
			t.Fatalf("Append stream %q error = nil, want invalid stream", stream)
		}
	}
	if _, err := store.Summary("../"+testTaskID, 10); err == nil {
		t.Fatal("Summary() error = nil, want invalid task ID")
	}
	if err := store.Remove("../" + testTaskID); err == nil {
		t.Fatal("Remove() error = nil, want invalid task ID")
	}
}

func TestStoreUsesPrivateModesDespiteUmask(t *testing.T) {
	base := t.TempDir()
	oldUmask := unix.Umask(0o777)
	defer unix.Umask(oldUmask)
	root := filepath.Join(base, "logs")
	store, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(testTaskID, "codex.events", []byte("ok")); err != nil {
		t.Fatal(err)
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("root mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
}

func TestWriterPropagatesAppendFailure(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store.Root, testTaskID+".log")); err != nil {
		t.Fatal(err)
	}
	writer := store.Writer(testTaskID, "checks")
	if _, err := writer.Write([]byte("record\n")); err == nil {
		t.Fatal("Writer.Write() error = nil, want append error")
	}
	if _, err := writer.Write([]byte("another\n")); err == nil {
		t.Fatal("Writer.Write() second error = nil, want persistent append error")
	}
}

func TestStoreConcurrentRecordsDoNotInterleave(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	const count = 64

	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf("record-%03d-%s", i, strings.Repeat("x", 4096))
			if err := store.Append(testTaskID, "checks", []byte(payload)); err != nil {
				t.Errorf("Append() error = %v", err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != count {
		t.Fatalf("record count = %d, want %d", len(lines), count)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, `{"time":"`) || !strings.Contains(line, `"stream":"checks"`) || !strings.HasSuffix(line, `"}`) {
			t.Fatalf("interleaved or malformed record: %.120q", line)
		}
	}
}

func TestStoreRedactsSecretsAndFramesNewlines(t *testing.T) {
	t.Parallel()
	secrets := []string{"short-key", "short-key-long-secret"}
	store := openStore(t, secrets)
	input := "short-key-long-secret short-key Authorization: bEaReR bearer-value\n" +
		"CODEX_API_KEY=codex-secret OPENAI_API_KEY=openai-secret NAPCAT_ACCESS_TOKEN=napcat-secret"
	if err := store.Append(testTaskID, "codex.stderr", []byte(input)); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, secret := range []string{"short-key", "short-key-long-secret", "bearer-value", "codex-secret", "openai-secret", "napcat-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log contains secret %q: %s", secret, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("record framing contains injected newline: %q", got)
	}
	if !strings.Contains(got, store.exactMarker) || !strings.Contains(got, `\n`) {
		t.Fatalf("log lacks redaction or escaped newline: %q", got)
	}
}

func TestAppendRedactsSecretThatOccursInGenericMarker(t *testing.T) {
	t.Parallel()
	store := openStore(t, []string{"REDACTED"})
	if err := store.Append(testTaskID, "codex.events", []byte("CODEX_API_KEY=value REDACTED")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	var entry record
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"REDACTED", "value"} {
		if strings.Contains(entry.Data, secret) {
			t.Fatalf("redacted data still contains %q: %q", secret, entry.Data)
		}
	}
}

func TestConfiguredSecretsCannotDisableCredentialRedaction(t *testing.T) {
	t.Parallel()
	store := openStore(t, []string{"Authorization", "CODEX_API"})
	input := "Authorization: Bearer bearer-value CODEX_API_KEY=codex-secret"
	if err := store.Append(testTaskID, "codex.stderr", []byte(input)); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "bearer-value") || strings.Contains(string(data), "codex-secret") {
		t.Fatalf("overlapping configured secret disabled credential redaction: %s", data)
	}
}

func TestWriterImmediatelyAppendsCompleteRecordWithoutNewline(t *testing.T) {
	t.Parallel()
	store := openStore(t, []string{"split-secret"})
	writer := store.Writer(testTaskID, "checks")
	if _, err := writer.Write([]byte("prefix split-secret suffix")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "split-secret") {
		t.Fatalf("split secret was not redacted: %s", data)
	}
}

func TestRedactTextCoversPlainAndJSONEscapedSecrets(t *testing.T) {
	secret := "quote\\line\ncontrol\tvalue"
	logs, err := Open(t.TempDir(), []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(encoded[1 : len(encoded)-1])
	got := logs.RedactText("plain=" + secret + " escaped=" + escaped)
	if strings.Contains(got, secret) || strings.Contains(got, escaped) {
		t.Fatalf("RedactText leaked configured secret: %q", got)
	}
	if got != "plain="+logs.exactMarker+" escaped="+logs.exactMarker || logs.RedactText(got) != got || !utf8.ValidString(got) {
		t.Fatalf("RedactText() = %q", got)
	}
}

func TestRedactTextIsIdempotentForRepeatedCharacterSecret(t *testing.T) {
	logs, err := Open(t.TempDir(), []string{"AAAAAAAA"})
	if err != nil {
		t.Fatal(err)
	}
	once := logs.RedactText("AAAAAAAA")
	twice := logs.RedactText(once)
	if strings.Contains(once, "AAAAAAAA") || twice != once || !utf8.ValidString(once) {
		t.Fatalf("redaction is not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestRedactTextMatchesSecretsContainingMarker(t *testing.T) {
	secrets := []string{
		"prefix[REDACTED]suffix",
		"[REDACTED]suffix",
		"quote\"[REDACTED]\\suffix",
	}
	logs, err := Open(t.TempDir(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		encoded, err := json.Marshal(secret)
		if err != nil {
			t.Fatal(err)
		}
		escaped := string(encoded[1 : len(encoded)-1])
		for _, value := range []string{secret, escaped} {
			got := logs.RedactText("before " + value + " after")
			if strings.Contains(got, secret) || strings.Contains(got, escaped) || logs.RedactText(got) != got {
				t.Errorf("RedactText(%q) = %q", value, got)
			}
		}
	}
	overlapLogs, err := Open(t.TempDir(), append(secrets, "Authorization", "CODEX_API"))
	if err != nil {
		t.Fatal(err)
	}
	got := overlapLogs.RedactText(secrets[0] + " Bearer bearer-value CODEX_API_KEY=codex-secret")
	for _, leaked := range []string{secrets[0], "bearer-value", "codex-secret"} {
		if strings.Contains(got, leaked) {
			t.Errorf("overlapping exact/generic redaction leaked %q: %q", leaked, got)
		}
	}
}

func TestRedactTextNeverReturnsConfiguredSecret(t *testing.T) {
	secrets := []string{redactionMarker, "overlap-ab", "prefix" + redactionMarker + "suffix", "AAAAAAAA"}
	for _, secret := range secrets {
		logs, err := Open(t.TempDir(), []string{secret})
		if err != nil {
			t.Fatal(err)
		}
		input := secret + " aabb Bearer bearer-value CODEX_API_KEY=codex-secret " + redactionMarker
		once := logs.RedactText(input)
		twice := logs.RedactText(once)
		if strings.Contains(once, secret) || twice != once || !utf8.ValidString(once) {
			t.Errorf("secret=%q once=%q twice=%q", secret, once, twice)
		}
		if strings.Contains(once, "bearer-value") || strings.Contains(once, "codex-secret") {
			t.Errorf("generic credential leaked for secret %q: %q", secret, once)
		}
	}
	genericOnly, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := genericOnly.RedactText("Bearer token")
	if got != "Bearer "+redactionMarker || !utf8.ValidString(got) {
		t.Fatalf("generic-only redaction = %q", got)
	}
	invalid := genericOnly.RedactText(string([]byte{'x', 0xff, 'y'}))
	if !utf8.ValidString(invalid) {
		t.Fatalf("RedactText returned invalid UTF-8: %q", invalid)
	}
}

func TestOpenRejectsShortSecretBeforeFilesystemMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-exist")
	_, err := Open(root, []string{"abc123"})
	if err == nil || strings.Contains(err.Error(), "abc123") {
		t.Fatalf("Open short secret error = %v", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("root created for short secret: %v", statErr)
	}
}

func TestOpenRejectsInvalidUTF8SecretBeforeFilesystemMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-exist")
	_, err := Open(root, []string{string([]byte{0xff, 0xfe})})
	if err == nil || strings.Contains(err.Error(), "\xff") {
		t.Fatalf("Open invalid UTF-8 error = %v", err)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("root created for invalid secret: %v", statErr)
	}
}

func TestSummaryIsUnicodeSafeBoundedAndReredacted(t *testing.T) {
	t.Parallel()
	store := openStore(t, []string{"summary-secret"})
	path := filepath.Join(store.Root, testTaskID+".log")
	if err := os.WriteFile(path, rawLogRecord(t, strings.Repeat("旧", 2000)+" summary-secret 尾部🙂"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Summary(testTaskID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 8 {
		t.Fatalf("Summary() = %q (%d runes), want valid UTF-8 <= 8 runes", got, utf8.RuneCountInString(got))
	}
	if strings.Contains(got, "summary-secret") {
		t.Fatalf("Summary() leaked configured secret: %q", got)
	}
	if _, err := store.Summary(testTaskID, 0); err == nil {
		t.Fatal("Summary() error = nil, want nonpositive limit rejection")
	}
	if _, err := store.Summary(testTaskID, int(^uint(0)>>1)); err != nil {
		t.Fatalf("Summary() huge limit error = %v", err)
	}
}

func TestSummaryReredactsCompleteRecord(t *testing.T) {
	t.Parallel()
	secret := "boundary-secret-value"
	store := openStore(t, []string{secret})
	data := rawLogRecord(t, "prefix-"+secret+strings.Repeat("y", 100))
	if err := os.WriteFile(filepath.Join(store.Root, testTaskID+".log"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Summary(testTaskID, maxSummaryReadBytes+100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, secret) || !strings.Contains(got, store.exactMarker) {
		t.Fatalf("Summary() did not re-redact complete record")
	}
}

func TestOpenBoundsConfiguredSecretsByBytes(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "logs")
	tooLongASCII := strings.Repeat("s", maxRedactionOverlap+1)
	tooLongUTF8 := strings.Repeat("密", maxRedactionOverlap/len("密")+1)
	for _, secret := range []string{tooLongASCII, tooLongUTF8} {
		if len(secret) <= maxRedactionOverlap {
			t.Fatalf("test secret byte length = %d, want over limit", len(secret))
		}
		_, err := Open(root, []string{secret})
		if err == nil {
			t.Fatal("Open() error = nil, want oversized secret rejection")
		}
		if err.Error() != "task log secret exceeds maximum length" || strings.Contains(err.Error(), secret) {
			t.Fatalf("Open() error = %q, want generic error without secret", err)
		}
	}

	exactUTF8 := strings.Repeat("密", maxRedactionOverlap/len("密")) + "a"
	if len(exactUTF8) != maxRedactionOverlap {
		t.Fatalf("exact UTF-8 secret byte length = %d, want %d", len(exactUTF8), maxRedactionOverlap)
	}
	store, err := Open(root, []string{exactUTF8, exactUTF8})
	if err != nil {
		t.Fatalf("Open() rejected exact-limit secret: %v", err)
	}
	if len(store.secrets) != 1 {
		t.Fatalf("configured secrets = %d, want duplicate removed", len(store.secrets))
	}
}

func TestSummaryDiscardsPartialSensitiveRecordAtReadBoundary(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("s", maxRedactionOverlap)
	bearer := strings.Repeat("b", maxRedactionOverlap)
	tests := []struct {
		name      string
		sensitive string
		data      string
		secrets   []string
	}{
		{name: "configured secret", sensitive: secret, data: secret, secrets: []string{secret}},
		{name: "bearer token", sensitive: bearer, data: "Authorization: Bearer " + bearer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openStore(t, tt.secrets)
			data, sensitiveOffset := boundaryLog(t, tt.data, tt.sensitive)
			readWindow := maxSummaryReadBytes + maxRedactionOverlap
			if got := len(data) - readWindow; got != sensitiveOffset+1 {
				t.Fatalf("read offset = %d, want one byte after sensitive offset %d", got, sensitiveOffset)
			}
			if err := os.WriteFile(filepath.Join(store.Root, testTaskID+".log"), data, 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := store.Summary(testTaskID, readWindow+100)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, tt.sensitive[1:]) {
				t.Fatal("Summary() leaked suffix from partial sensitive record")
			}
			if !strings.Contains(got, "useful-tail") {
				t.Fatalf("Summary() discarded later valid record: %q", got[len(got)-min(len(got), 200):])
			}
		})
	}
}

func TestSummarySkipsMalformedAndTrailingPartialRecords(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	data := append([]byte("PRIVATE-MALFORMED-DATA\n"), rawLogRecord(t, "useful-tail")...)
	data = append(data, []byte(`{"time":"PRIVATE-TRAILING-DATA"`)...)
	if err := os.WriteFile(filepath.Join(store.Root, testTaskID+".log"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Summary(testTaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "PRIVATE-") || !strings.Contains(got, "useful-tail") {
		t.Fatalf("Summary() returned malformed data or lost valid record: %q", got)
	}
}

func TestSummaryReturnsEmptyWhenNoCompleteRecordRemains(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	if err := os.WriteFile(filepath.Join(store.Root, testTaskID+".log"), []byte("PRIVATE-INCOMPLETE"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Summary(testTaskID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("Summary() = %q, want empty for incomplete record", got)
	}
}

func boundaryLog(t *testing.T, firstData, sensitive string) ([]byte, int) {
	t.Helper()
	prefix := rawLogRecord(t, "prefix")
	sensitiveRecord := rawLogRecord(t, firstData)
	tail := rawLogRecord(t, "useful-tail")
	withinRecord := bytes.Index(sensitiveRecord, []byte(sensitive))
	if withinRecord < 0 {
		t.Fatal("sensitive value not found in test record")
	}
	readWindow := maxSummaryReadBytes + maxRedactionOverlap
	paddingLength := readWindow + 1 - (len(sensitiveRecord) - withinRecord) - len(tail)
	emptyPadding := rawLogRecord(t, "")
	if paddingLength < len(emptyPadding) {
		t.Fatal("invalid boundary test padding")
	}
	padding := rawLogRecord(t, strings.Repeat("y", paddingLength-len(emptyPadding)))
	if len(padding) != paddingLength {
		t.Fatalf("padding record length = %d, want %d", len(padding), paddingLength)
	}
	data := bytes.Join([][]byte{prefix, sensitiveRecord, padding, tail}, nil)
	return data, len(prefix) + withinRecord
}

func rawLogRecord(t *testing.T, data string) []byte {
	t.Helper()
	encoded, err := json.Marshal(record{
		Time:   time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		Stream: "test",
		Data:   data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func TestStoreRejectsSymlinkTaskFileAndSafeRemove(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(store.Root, testTaskID+".log")
	if err := os.Symlink(target, logPath); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(testTaskID, "checks", []byte("overwrite")); err == nil {
		t.Fatal("Append() error = nil, want symlink rejection")
	}
	if _, err := store.Summary(testTaskID, 100); err == nil {
		t.Fatal("Summary() error = nil, want symlink rejection")
	}
	if err := store.Remove(testTaskID); err == nil {
		t.Fatal("Remove() error = nil, want symlink rejection")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink target = %q, want unchanged", data)
	}
}

func TestStoreRejectsReplacedRoot(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	original := store.Root + ".old"
	if err := os.Rename(store.Root, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(testTaskID, "checks", []byte("data")); err == nil {
		t.Fatal("Append() error = nil, want replaced root rejection")
	}
}

func TestStoreRejectsWidenedRootOrFilePermissions(t *testing.T) {
	t.Parallel()
	t.Run("root", func(t *testing.T) {
		store := openStore(t, nil)
		if err := os.Chmod(store.Root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := store.Append(testTaskID, "checks", []byte("data")); err == nil {
			t.Fatal("Append() error = nil, want widened root rejection")
		}
	})
	t.Run("file", func(t *testing.T) {
		store := openStore(t, nil)
		if err := store.Append(testTaskID, "checks", []byte("data")); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Root, testTaskID+".log")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Summary(testTaskID, 100); err == nil {
			t.Fatal("Summary() error = nil, want widened file rejection")
		}
		if err := store.Remove(testTaskID); err == nil {
			t.Fatal("Remove() error = nil, want widened file rejection")
		}
	})
}

func TestRemoveDeletesOnlyRegularTaskLog(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	if err := store.Append(testTaskID, "checks", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(testTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, testTaskID+".log")); !os.IsNotExist(err) {
		t.Fatalf("task log still exists: %v", err)
	}
	if err := store.Remove(testTaskID); err != nil {
		t.Fatalf("idempotent Remove() error = %v", err)
	}
}

func TestAppendRejectsOversizedRecordWithoutLimitingTask(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	err := store.Append(testTaskID, "codex.events", bytes.Repeat([]byte{'x'}, maxRecordDataBytes+1))
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Append() error = %v, want record limit", err)
	}
	if err := store.Append(testTaskID, "codex.events", []byte("small")); err != nil {
		t.Fatalf("small Append() after oversized record error = %v", err)
	}
}

func TestAppendAcceptsBoundedCallerPayloadWithFramingMarker(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	data := bytes.Repeat([]byte{'x'}, maxRecordDataBytes-256)
	if err := store.Append(testTaskID, "codex.stderr", data); err != nil {
		t.Fatalf("Append() error = %v, want bounded caller payload accepted", err)
	}
}

func TestAppendEnforcesTaskLimitAndWritesOneMarker(t *testing.T) {
	t.Parallel()
	store := openStore(t, nil)
	store.maxTaskBytes = 8 << 10
	payload := bytes.Repeat([]byte{'x'}, 200)
	var limitErr error
	for range 1000 {
		if err := store.Append(testTaskID, "codex.events", payload); err != nil {
			limitErr = err
			break
		}
	}
	if !errors.Is(limitErr, ErrLimitExceeded) {
		t.Fatalf("Append() limit error = %v", limitErr)
	}
	path := filepath.Join(store.Root, testTaskID+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > store.maxTaskBytes {
		t.Fatalf("task log size = %d, limit = %d", len(data), store.maxTaskBytes)
	}
	if got := bytes.Count(data, taskLimitRecord); got != 1 {
		t.Fatalf("task limit marker count = %d, want 1", got)
	}

	size := len(data)
	for range 3 {
		if err := store.Append(testTaskID, "checks", []byte("later")); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("Append() after limit error = %v", err)
		}
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != size || bytes.Count(data, taskLimitRecord) != 1 {
		t.Fatalf("limited task log changed: size=%d marker count=%d", len(data), bytes.Count(data, taskLimitRecord))
	}

	reopened, err := Open(store.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened.maxTaskBytes = store.maxTaskBytes
	if err := reopened.Append(testTaskID, "checks", []byte("after restart")); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("reopened Append() error = %v, want persistent task limit", err)
	}
}

func TestAppendCountsEncodedRecordBytesAndNeverExceedsHardLimit(t *testing.T) {
	t.Parallel()
	t.Run("encoded overhead", func(t *testing.T) {
		store := openStore(t, nil)
		store.maxTaskBytes = 4 << 10
		data := bytes.Repeat([]byte{1}, 700)
		err := store.Append(testTaskID, "codex.events", data)
		if !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("Append() error = %v, want encoded-size limit", err)
		}
		info, err := os.Stat(filepath.Join(store.Root, testTaskID+".log"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > store.maxTaskBytes {
			t.Fatalf("encoded task log size = %d, limit = %d", info.Size(), store.maxTaskBytes)
		}
	})

	t.Run("hard limit", func(t *testing.T) {
		store := openStore(t, nil)
		if err := store.Append(testTaskID, "checks", []byte("seed")); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Root, testTaskID+".log")
		if err := os.Truncate(path, maxTaskLogBytes); err != nil {
			t.Fatal(err)
		}
		if err := store.Append(testTaskID, "checks", []byte("must-not-grow")); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("Append() error = %v, want hard task limit", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != maxTaskLogBytes {
			t.Fatalf("task log size = %d, want hard limit %d", info.Size(), maxTaskLogBytes)
		}
	})
}

func TestWriteAllHandlesPartialAndZeroWrites(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writer := partialWriter{writer: &output, max: 3}
	if err := writeAll(writer, []byte("complete")); err != nil {
		t.Fatal(err)
	}
	if output.String() != "complete" {
		t.Fatalf("writeAll() output = %q", output.String())
	}
	if err := writeAll(zeroWriter{}, []byte("data")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeAll() zero-write error = %v, want io.ErrShortWrite", err)
	}
	wantErr := errors.New("write failed")
	if err := writeAll(partialErrorWriter{err: wantErr}, []byte("data")); !errors.Is(err, wantErr) {
		t.Fatalf("writeAll() partial-write error = %v, want %v", err, wantErr)
	}
}

type partialWriter struct {
	writer io.Writer
	max    int
}

func (w partialWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.writer.Write(p)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

type partialErrorWriter struct{ err error }

func (w partialErrorWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

func openStore(t *testing.T, secrets []string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "logs"), secrets)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
