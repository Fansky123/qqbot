package tasklog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	secrets := []string{"short", "short-long-secret"}
	store := openStore(t, secrets)
	input := "short-long-secret short Authorization: bEaReR bearer-value\n" +
		"CODEX_API_KEY=codex-secret OPENAI_API_KEY=openai-secret NAPCAT_ACCESS_TOKEN=napcat-secret"
	if err := store.Append(testTaskID, "codex.stderr", []byte(input)); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(store.Root, testTaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, secret := range []string{"short", "short-long-secret", "bearer-value", "codex-secret", "openai-secret", "napcat-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log contains secret %q: %s", secret, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("record framing contains injected newline: %q", got)
	}
	if !strings.Contains(got, `[REDACTED]`) || !strings.Contains(got, `\n`) {
		t.Fatalf("log lacks redaction or escaped newline: %q", got)
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
	if strings.Contains(string(data), "split-secret") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("split secret was not redacted: %s", data)
	}
}

func TestSummaryIsUnicodeSafeBoundedAndReredacted(t *testing.T) {
	t.Parallel()
	store := openStore(t, []string{"summary-secret"})
	path := filepath.Join(store.Root, testTaskID+".log")
	if err := os.WriteFile(path, []byte(strings.Repeat("旧", 2000)+" summary-secret 尾部🙂"), 0o600); err != nil {
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

func TestSummaryRedactsSecretCrossingTailReadBoundary(t *testing.T) {
	t.Parallel()
	secret := "boundary-secret-value"
	store := openStore(t, []string{secret})
	cut := len(secret) / 2
	suffix := strings.Repeat("y", maxSummaryReadBytes-(len(secret)-cut))
	data := []byte("prefix-" + secret + suffix)
	if err := os.WriteFile(filepath.Join(store.Root, testTaskID+".log"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Summary(testTaskID, maxSummaryReadBytes+100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, secret[cut:]) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("Summary() leaked secret suffix across tail boundary")
	}
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

func openStore(t *testing.T, secrets []string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "logs"), secrets)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
