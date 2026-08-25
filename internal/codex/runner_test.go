package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"qqcodex/internal/model"
)

const (
	helperEnabled = "GO_WANT_CODEX_HELPER"
	helperRecord  = "QQ_CODEX_HELPER_RECORD"
	helperFinal   = "QQ_CODEX_HELPER_FINAL"
	helperStderr  = "QQ_CODEX_HELPER_STDERR"
	helperExit    = "QQ_CODEX_HELPER_EXIT"
	helperSleep   = "QQ_CODEX_HELPER_SLEEP"
	helperLarge   = "QQ_CODEX_HELPER_LARGE"
	helperSpawn   = "QQ_CODEX_HELPER_SPAWN"
	helperPID     = "QQ_CODEX_HELPER_PID"
	helperChild   = "QQ_CODEX_HELPER_CHILD"
	helperStdout  = "QQ_CODEX_HELPER_STDOUT"
	helperDirty   = "QQ_CODEX_HELPER_DIRTY_TEMP"
)

type helperRecordData struct {
	PID           int      `json:"pid"`
	Args          []string `json:"args"`
	Env           []string `json:"env"`
	Dir           string   `json:"dir"`
	SchemaPath    string   `json:"schema_path"`
	SchemaMode    uint32   `json:"schema_mode"`
	SchemaContent string   `json:"schema_content"`
	LastPath      string   `json:"last_path"`
	LastMode      uint32   `json:"last_mode"`
}

func init() {
	if os.Getenv(helperEnabled) != "1" {
		return
	}
	if os.Getenv(helperChild) == "1" {
		pid := strconv.Itoa(os.Getpid())
		helperMust(os.WriteFile(os.Getenv(helperPID), []byte(pid), 0o600))
		for {
			time.Sleep(time.Hour)
		}
	}
	runCodexHelper()
	os.Exit(0)
}

func runCodexHelper() {
	record := helperRecordData{PID: os.Getpid(), Args: os.Args[1:], Env: os.Environ()}
	record.Dir, _ = os.Getwd()
	record.SchemaPath = flagValue(record.Args, "--output-schema")
	record.LastPath = flagValue(record.Args, "-o")
	if record.SchemaPath != "" {
		if info, err := os.Stat(record.SchemaPath); err == nil {
			record.SchemaMode = uint32(info.Mode().Perm())
		}
		data, _ := os.ReadFile(record.SchemaPath)
		record.SchemaContent = string(data)
	}
	if record.LastPath != "" {
		if info, err := os.Stat(record.LastPath); err == nil {
			record.LastMode = uint32(info.Mode().Perm())
		}
		_ = os.WriteFile(record.LastPath, []byte(envOr(helperFinal, "final answer")), 0o600)
	}
	data, err := json.Marshal(record)
	helperMust(err)
	helperMust(os.WriteFile(os.Getenv(helperRecord), data, 0o600))
	if os.Getenv(helperDirty) == "schema" {
		helperMust(os.Remove(record.SchemaPath))
		helperMust(os.Mkdir(record.SchemaPath, 0o700))
		helperMust(os.WriteFile(filepath.Join(record.SchemaPath, "retained"), []byte("private temp content"), 0o600))
	} else if os.Getenv(helperDirty) == "remove-schema" {
		helperMust(os.Remove(record.SchemaPath))
	}

	if spawn := os.Getenv(helperSpawn); spawn != "" {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), helperChild+"=1")
		if spawn == "detached" {
			cmd.Stdout = os.Stdout
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		helperMust(cmd.Start())
	}

	if output, ok := os.LookupEnv(helperStdout); ok {
		_, err = io.WriteString(os.Stdout, output)
		helperMust(err)
	} else {
		helperPrintln(`{"type":"item.completed","thread_id":"unrelated"}`)
		helperPrintln(`{"type":"thread.started","thread_id":"thread-123"}`)
		if size, _ := strconv.Atoi(os.Getenv(helperLarge)); size > 0 {
			_, err = fmt.Fprintf(os.Stdout, `{"type":"item.completed","data":"%s"}`+"\n", strings.Repeat("x", size))
			helperMust(err)
		}
		helperPrintln(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
	}
	_, _ = fmt.Fprint(os.Stderr, os.Getenv(helperStderr))
	if delay, _ := time.ParseDuration(os.Getenv(helperSleep)); delay > 0 {
		time.Sleep(delay)
	}
	if code, _ := strconv.Atoi(os.Getenv(helperExit)); code != 0 {
		os.Exit(code)
	}
}

func helperPrintln(line string) {
	_, err := fmt.Fprintln(os.Stdout, line)
	helperMust(err)
}

func helperMust(err error) {
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(98)
	}
}

func flagValue(args []string, name string) string {
	for i := range len(args) - 1 {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func TestRunnerPlanArgumentsAndTemporaryFiles(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	prompt := "--dangerously-bypass-approvals-and-sandbox literal `code` $(touch nope); 'quoted' \"double\"\nsecond line"
	req.Prompt = prompt

	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := append([]string{"exec"}, expectedPolicyArgs(runner.KeepEnv)...)
	want = append(want,
		"-C", req.WorkingDir, "--sandbox", "read-only", "--ephemeral",
		"--output-schema", record.SchemaPath, "--json", "-o", record.LastPath, "--", prompt,
	)
	if !reflect.DeepEqual(record.Args, want) {
		t.Fatalf("argv = %#v, want %#v", record.Args, want)
	}
	if record.SchemaPath == record.LastPath || record.SchemaPath == "" || record.LastPath == "" {
		t.Fatalf("temporary paths are not distinct: schema=%q last=%q", record.SchemaPath, record.LastPath)
	}
	if record.SchemaMode != 0o600 || record.LastMode != 0o600 {
		t.Fatalf("temporary modes = %o and %o, want 600", record.SchemaMode, record.LastMode)
	}
	wantSchema, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if record.SchemaContent != string(wantSchema) {
		t.Fatalf("schema content differs from embedded schema")
	}
	assertRemoved(t, record.SchemaPath)
	assertRemoved(t, record.LastPath)
	if result.SessionID != "thread-123" || result.Final != "final answer" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if strings.Count(string(result.EventsJSONL), "\n") != 3 {
		t.Fatalf("events = %q", result.EventsJSONL)
	}
}

func TestRunnerExecuteArguments(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.GitCommonDir = t.TempDir()
	req.Prompt = "--dangerously-bypass-approvals-and-sandbox"

	if _, err := runner.Execute(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := append([]string{"exec"}, expectedPolicyArgs(runner.KeepEnv)...)
	want = append(want, "-C", req.WorkingDir, "--sandbox", "workspace-write", "--add-dir", req.GitCommonDir,
		"--json", "-o", record.LastPath, "--", req.Prompt)
	if !reflect.DeepEqual(record.Args, want) {
		t.Fatalf("argv = %#v, want %#v", record.Args, want)
	}
	if got := countArg(record.Args, "--add-dir"); got != 1 {
		t.Fatalf("--add-dir count = %d, want 1", got)
	}
	assertRemoved(t, record.LastPath)
}

func TestRunnerResumeArgumentsAndWorkingDirectory(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.SessionID = "session-exact"
	req.Prompt = "--last"

	if _, err := runner.Resume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := append([]string{"exec", "resume"}, expectedPolicyArgs(runner.KeepEnv)...)
	want = append(want, "--json", "-o", record.LastPath, "--", req.SessionID, req.Prompt)
	if !reflect.DeepEqual(record.Args, want) {
		t.Fatalf("argv = %#v, want %#v", record.Args, want)
	}
	separator := slices.Index(record.Args, "--")
	if separator < 0 {
		t.Fatalf("resume argv lacks option terminator: %#v", record.Args)
	}
	for _, forbidden := range []string{"--last", "-C", "--sandbox", "--add-dir"} {
		if slices.Contains(record.Args[:separator], forbidden) {
			t.Fatalf("resume argv contains %q: %#v", forbidden, record.Args)
		}
	}
	if record.Dir != req.WorkingDir {
		t.Fatalf("working directory = %q, want %q", record.Dir, req.WorkingDir)
	}
}

func TestRunnerSanitizesEnvironment(t *testing.T) {
	setEnv(t, "CODEX_API_KEY", "codex-key")
	setEnv(t, "PATH", "/bin")
	setEnv(t, "HOME", "/private/home")
	setEnv(t, "LANG", "C.UTF-8")
	setEnv(t, "TMPDIR", "/tmp")
	unsetEnv(t, "TMP")
	unsetEnv(t, "TEMP")
	setEnv(t, "ALLOWED_CUSTOM", "kept")
	setEnv(t, "UNKNOWN_SECRET", "must-not-leak")
	runner, req, recordPath := helperRunner(t)
	runner.KeepEnv = append(runner.KeepEnv, "ALLOWED_CUSTOM", "PATH", "CODEX_API_KEY", "ALLOWED_CUSTOM")

	if _, err := runner.Execute(context.Background(), withGitCommonDir(t, req)); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := []string{
		"CODEX_API_KEY=codex-key", "PATH=/bin", "HOME=/private/home", "LANG=C.UTF-8", "TMPDIR=/tmp",
		"GO_WANT_CODEX_HELPER=1", "QQ_CODEX_HELPER_RECORD=" + recordPath, "ALLOWED_CUSTOM=kept",
	}
	if !reflect.DeepEqual(record.Env, want) {
		t.Fatalf("environment = %#v, want %#v", record.Env, want)
	}
	for _, arg := range record.Args {
		if strings.Contains(arg, "filters.CODEX_API_KEY") {
			t.Fatalf("CODEX_API_KEY exposed to tool subprocess policy: %q", arg)
		}
	}
	wantPolicy := expectedPolicyArgs(runner.KeepEnv)
	if len(record.Args) < 1+len(wantPolicy) || !slices.Equal(record.Args[1:1+len(wantPolicy)], wantPolicy) {
		t.Fatalf("argv = %#v, want policy prefix %#v", record.Args, wantPolicy)
	}
}

func TestRunnerRejectsEnvironmentAssignment(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	runner.KeepEnv = append(runner.KeepEnv, "INJECTED=value")

	_, err := runner.Plan(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("error = %v, want invalid environment variable error", err)
	}
	assertNotCreated(t, recordPath)
}

func TestRunnerCancellationKillsProcessGroup(t *testing.T) {
	runner, req, _ := helperRunner(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	setEnv(t, helperSpawn, "1")
	setEnv(t, helperPID, pidPath)
	setEnv(t, helperSleep, "10s")
	runner.KeepEnv = append(runner.KeepEnv, helperSpawn, helperPID, helperSleep)
	t.Cleanup(func() { killProcessFromFile(pidPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runner.Plan(ctx, req)
		done <- err
	}()

	pid := waitForPIDFile(t, pidPath)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("descendant process %d survived cancellation", pid)
	}
}

func TestRunnerCancellationWithDetachedStdoutReturnsContextErrorAndPartialResult(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	pidPath := filepath.Join(t.TempDir(), "detached.pid")
	setEnv(t, helperSpawn, "detached")
	setEnv(t, helperPID, pidPath)
	runner.KeepEnv = append(runner.KeepEnv, helperSpawn, helperPID)
	t.Cleanup(func() { killProcessFromFile(pidPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runner.Plan(ctx, req)
		done <- outcome{result, err}
	}()

	outerPID := waitForHelperPID(t, recordPath)
	waitForProcessExit(t, outerPID)
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Plan() error = %v, want context canceled", got.err)
		}
		if got.result.SessionID != "thread-123" || got.result.Final != "final answer" || len(got.result.EventsJSONL) == 0 {
			t.Fatalf("result = %#v, want retained events, session, and final message", got.result)
		}
	case <-time.After(2 * time.Second):
		killProcessFromFile(pidPath)
		t.Fatal("Plan did not return after context cancellation closed retained stdout")
	}
}

func TestRunnerReturnsPartialResultWithoutStderrOnFailure(t *testing.T) {
	runner, req, _ := helperRunner(t)
	setEnv(t, helperFinal, "partial final")
	setEnv(t, helperStderr, "private-secret-stderr")
	setEnv(t, helperExit, "7")
	runner.KeepEnv = append(runner.KeepEnv, helperFinal, helperStderr, helperExit)

	result, err := runner.Plan(context.Background(), req)
	if err == nil {
		t.Fatal("expected nonzero-exit error")
	}
	if result.SessionID != "thread-123" || result.Final != "partial final" || len(result.EventsJSONL) == 0 {
		t.Fatalf("partial result = %#v", result)
	}
	if strings.Contains(err.Error(), "private-secret-stderr") || strings.Contains(string(result.EventsJSONL), "private-secret-stderr") || strings.Contains(result.Final, "private-secret-stderr") {
		t.Fatalf("stderr leaked through returned data: result=%#v error=%v", result, err)
	}
	entries, readErr := os.ReadDir(runner.LogDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var found bool
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(runner.LogDir, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(data) != "private-secret-stderr" {
			continue
		}
		found = true
		info, statErr := entry.Info()
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("stderr log mode = %o, want 600", info.Mode().Perm())
		}
	}
	if !found {
		t.Fatal("private stderr log not found")
	}
	info, statErr := os.Stat(runner.LogDir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestRunnerAcceptsLargeJSONLLine(t *testing.T) {
	runner, req, _ := helperRunner(t)
	setEnv(t, helperLarge, strconv.Itoa(128<<10))
	runner.KeepEnv = append(runner.KeepEnv, helperLarge)

	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.EventsJSONL) < 128<<10 {
		t.Fatalf("events length = %d, want at least 128 KiB", len(result.EventsJSONL))
	}
}

func TestRunnerReturnsScannerErrorWithPartialResult(t *testing.T) {
	runner, req, _ := helperRunner(t)
	setEnv(t, helperLarge, strconv.Itoa(2<<20))
	runner.KeepEnv = append(runner.KeepEnv, helperLarge)

	result, err := runner.Plan(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "scan codex JSONL") {
		t.Fatalf("error = %v, want scanner error", err)
	}
	if result.SessionID != "thread-123" || result.Final != "final answer" || len(result.EventsJSONL) == 0 {
		t.Fatalf("partial result = %#v", result)
	}
}

func TestRunnerOnlyUsesThreadStartedSessionID(t *testing.T) {
	runner, req, _ := helperRunner(t)
	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "thread-123" {
		t.Fatalf("session ID = %q, want thread-123", result.SessionID)
	}
}

func TestRunnerRejectsMalformedAndNonObjectJSONLWithPartialResult(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"malformed", `{`},
		{"array", `[]`},
		{"string", `"text"`},
		{"null", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, _ := helperRunner(t)
			prefix := `{"type":"thread.started","thread_id":"session-good"}` + "\n"
			setEnv(t, helperStdout, prefix+tt.line+"\n")
			runner.KeepEnv = append(runner.KeepEnv, helperStdout)

			result, err := runner.Plan(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "scan codex JSONL") {
				t.Fatalf("error = %v, want JSONL scan error", err)
			}
			if result.SessionID != "session-good" || string(result.EventsJSONL) != prefix {
				t.Fatalf("partial result = %#v, want only valid prior event", result)
			}
		})
	}
}

func TestRunnerRejectsInvalidThreadStartedSessionID(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		wantErr string
	}{
		{"missing", `{"type":"thread.started"}`, "session ID"},
		{"empty", `{"type":"thread.started","thread_id":""}`, "session ID"},
		{"option", `{"type":"thread.started","thread_id":"--last"}`, "session ID"},
		{"whitespace", `{"type":"thread.started","thread_id":"session bad"}`, "session ID"},
		{"wrong type", `{"type":"thread.started","thread_id":7}`, "session ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, _ := helperRunner(t)
			prefix := `{"type":"item.completed"}` + "\n"
			setEnv(t, helperStdout, prefix+tt.event+"\n")
			runner.KeepEnv = append(runner.KeepEnv, helperStdout)

			result, err := runner.Plan(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if result.SessionID != "" || string(result.EventsJSONL) != prefix {
				t.Fatalf("partial result = %#v, want only prior event", result)
			}
		})
	}
}

func TestRunnerRequiresSessionIDAfterSuccessfulPersistentInvocation(t *testing.T) {
	tests := []struct {
		name string
		run  func(Runner, Request) (Result, error)
	}{
		{"execute", func(r Runner, req Request) (Result, error) {
			return r.Execute(context.Background(), withGitCommonDir(t, req))
		}},
		{"resume", func(r Runner, req Request) (Result, error) {
			req.SessionID = "existing-session"
			return r.Resume(context.Background(), req)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, _ := helperRunner(t)
			setEnv(t, helperStdout, `{"type":"item.completed"}`+"\n")
			runner.KeepEnv = append(runner.KeepEnv, helperStdout)

			result, err := tt.run(runner, req)
			if err == nil || !strings.Contains(err.Error(), "session ID") {
				t.Fatalf("result = %#v, error = %v, want missing session error", result, err)
			}
		})
	}
}

func TestRunnerRemovesTemporaryFilesAfterFailure(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	setEnv(t, helperExit, "9")
	runner.KeepEnv = append(runner.KeepEnv, helperExit)
	if _, err := runner.Plan(context.Background(), req); err == nil {
		t.Fatal("expected failure")
	}
	record := readHelperRecord(t, recordPath)
	assertRemoved(t, record.SchemaPath)
	assertRemoved(t, record.LastPath)
}

func TestRunnerRemovesTemporaryFilesWhenStartFails(t *testing.T) {
	runner, req, _ := helperRunner(t)
	runner.Binary = filepath.Join(t.TempDir(), "missing-codex")

	if _, err := runner.Plan(context.Background(), req); err == nil {
		t.Fatal("expected start failure")
	}
	entries, err := os.ReadDir(runner.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "-last-") || strings.Contains(entry.Name(), "-schema-") {
			t.Fatalf("temporary file leaked after start failure: %q", entry.Name())
		}
	}
}

func TestRunnerReportsTemporaryCleanupFailureWithoutLosingResult(t *testing.T) {
	tests := []struct {
		name     string
		exitCode string
		want     []string
	}{
		{"cleanup only", "", []string{"remove private codex temporary file"}},
		{"joined with process failure", "7", []string{"codex planning failed", "remove private codex temporary file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, _ := helperRunner(t)
			setEnv(t, helperDirty, "schema")
			runner.KeepEnv = append(runner.KeepEnv, helperDirty)
			if tt.exitCode != "" {
				setEnv(t, helperExit, tt.exitCode)
				runner.KeepEnv = append(runner.KeepEnv, helperExit)
			}

			result, err := runner.Plan(context.Background(), req)
			if err == nil {
				t.Fatal("expected checked cleanup error")
			}
			for _, fragment := range tt.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error = %v, want %q", err, fragment)
				}
			}
			if result.SessionID != "thread-123" || result.Final != "final answer" {
				t.Fatalf("result = %#v, want successful data retained", result)
			}
			if strings.Contains(err.Error(), "private temp content") {
				t.Fatalf("temporary content leaked in error: %v", err)
			}
		})
	}
}

func TestRunnerIgnoresAlreadyRemovedTemporaryFile(t *testing.T) {
	runner, req, _ := helperRunner(t)
	setEnv(t, helperDirty, "remove-schema")
	runner.KeepEnv = append(runner.KeepEnv, helperDirty)

	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "thread-123" || result.Final != "final answer" {
		t.Fatalf("result = %#v, want complete result", result)
	}
}

func TestRunnerRejectsRepoRelativeBinary(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	localBinary := filepath.Join(req.WorkingDir, "codex")
	if err := os.Symlink(os.Args[0], localBinary); err != nil {
		t.Fatal(err)
	}
	runner.Binary = "./codex"

	_, err := runner.Plan(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("error = %v, want relative binary validation error", err)
	}
	assertNotCreated(t, recordPath)
}

func TestRunnerResolvesBareBinaryBeforeChangingDirectory(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	binDir := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(binDir, "codex-helper")); err != nil {
		t.Fatal(err)
	}
	setEnv(t, "PATH", binDir)
	runner.Binary = "codex-helper"

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	if record.Dir != req.WorkingDir {
		t.Fatalf("working directory = %q, want %q", record.Dir, req.WorkingDir)
	}
}

func TestRunnerValidatesBeforeSpawning(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		mutate func(*Runner, *Request)
	}{
		{"empty binary", "plan", func(r *Runner, _ *Request) { r.Binary = "" }},
		{"empty log directory", "plan", func(r *Runner, _ *Request) { r.LogDir = "" }},
		{"relative log directory", "plan", func(r *Runner, _ *Request) { r.LogDir = "relative" }},
		{"empty task ID", "plan", func(_ *Runner, q *Request) { q.TaskID = "" }},
		{"traversal task ID", "plan", func(_ *Runner, q *Request) { q.TaskID = "../escape" }},
		{"empty working directory", "plan", func(_ *Runner, q *Request) { q.WorkingDir = "" }},
		{"relative working directory", "plan", func(_ *Runner, q *Request) { q.WorkingDir = "relative" }},
		{"empty prompt", "plan", func(_ *Runner, q *Request) { q.Prompt = "" }},
		{"zero timeout", "plan", func(_ *Runner, q *Request) { q.Timeout = 0 }},
		{"negative timeout", "plan", func(_ *Runner, q *Request) { q.Timeout = -time.Second }},
		{"empty git common directory", "execute", func(_ *Runner, q *Request) { q.GitCommonDir = "" }},
		{"relative git common directory", "execute", func(_ *Runner, q *Request) { q.GitCommonDir = "relative" }},
		{"empty session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = "" }},
		{"option session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = "--last" }},
		{"leading dash session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = "-session" }},
		{"whitespace session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = "session bad" }},
		{"slash session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = "session/bad" }},
		{"long session ID", "resume", func(_ *Runner, q *Request) { q.SessionID = strings.Repeat("a", 129) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, recordPath := helperRunner(t)
			if tt.mode == "execute" {
				req = withGitCommonDir(t, req)
			}
			if tt.mode == "resume" {
				req.SessionID = "session"
			}
			tt.mutate(&runner, &req)
			var err error
			switch tt.mode {
			case "plan":
				_, err = runner.Plan(context.Background(), req)
			case "execute":
				_, err = runner.Execute(context.Background(), req)
			case "resume":
				_, err = runner.Resume(context.Background(), req)
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			assertNotCreated(t, recordPath)
		})
	}
}

func TestSessionIDValidationBoundaries(t *testing.T) {
	valid := []string{
		"session-123",
		"019c10d9-a29a-73c2-9a48-d131fcde8d46",
		"a" + strings.Repeat("x", 127),
	}
	for _, sessionID := range valid {
		if err := validateSessionID(sessionID); err != nil {
			t.Errorf("validateSessionID(%q) error = %v", sessionID, err)
		}
	}
}

func TestParsePlan(t *testing.T) {
	valid := `{"summary":"","scope":[],"checks":["go test ./..."],"risks":[]}`
	got, err := ParsePlan(valid)
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Summary: "", Scope: []string{}, Checks: []string{"go test ./..."}, Risks: []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}

	invalid := []struct {
		name  string
		input string
	}{
		{"malformed", `{`},
		{"unknown field", `{"summary":"ok","scope":[],"checks":[],"risks":[],"extra":true}`},
		{"trailing value", `{"summary":"ok","scope":[],"checks":[],"risks":[]} {}`},
		{"missing summary", `{"scope":[],"checks":[],"risks":[]}`},
		{"missing scope", `{"summary":"ok","checks":[],"risks":[]}`},
		{"capitalized alias", `{"Summary":"ok","scope":[],"checks":[],"risks":[]}`},
		{"mixed-case alias", `{"sUmMaRy":"ok","scope":[],"checks":[],"risks":[]}`},
		{"exact and case alias", `{"summary":"ok","Summary":"override","scope":[],"checks":[],"risks":[]}`},
		{"null summary", `{"summary":null,"scope":[],"checks":[],"risks":[]}`},
		{"null scope", `{"summary":"ok","scope":null,"checks":[],"risks":[]}`},
		{"null checks", `{"summary":"ok","scope":[],"checks":null,"risks":[]}`},
		{"null risks", `{"summary":"ok","scope":[],"checks":[],"risks":null}`},
		{"wrong checks type", `{"summary":"ok","scope":[],"checks":"go test","risks":[]}`},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePlan(tt.input); err == nil {
				t.Errorf("ParsePlan(%q) succeeded, want error", tt.input)
			}
		})
	}
}

func TestPromptBuildersKeepUntrustedDataDelimited(t *testing.T) {
	requirement := "change `x`; $(touch /tmp/nope)\nquote: \"value\""
	checks := [][]string{{"go", "test", "./..."}, {"tool", "arg;still-one", "line\nfeed"}}
	planning := PlanningPrompt("orders", requirement, checks)
	for _, fragment := range []string{
		`"project_id":"orders"`,
		`"requirement":"change ` + "`x`" + `; $(touch /tmp/nope)\nquote: \"value\""`,
		`["tool","arg;still-one","line\nfeed"]`,
		"untrusted data", "read-only", "structured plan", "remote push", "deployment",
	} {
		if !strings.Contains(planning, fragment) {
			t.Errorf("planning prompt missing %q:\n%s", fragment, planning)
		}
	}

	task := model.Task{
		ID: "task-7", ProjectID: "orders", Requirement: requirement,
		Plan: `{"summary":"do it"}`, Worktree: "/srv/worktrees/task-7",
	}
	execution := ExecutionPrompt(task, checks)
	for _, fragment := range []string{
		`"task_id":"task-7"`, `"worktree":"/srv/worktrees/task-7"`,
		`"plan":"{\"summary\":\"do it\"}"`,
		`["tool","arg;still-one","line\nfeed"]`,
		"untrusted data", "Git commit", "remote push", "deployment", "project configuration",
	} {
		if !strings.Contains(execution, fragment) {
			t.Errorf("execution prompt missing %q:\n%s", fragment, execution)
		}
	}
}

func helperRunner(t *testing.T) (Runner, Request, string) {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), "record.json")
	setEnv(t, helperEnabled, "1")
	setEnv(t, helperRecord, recordPath)
	for _, name := range []string{
		helperFinal, helperStderr, helperExit, helperSleep, helperLarge, helperSpawn,
		helperPID, helperChild, helperStdout, helperDirty,
	} {
		unsetEnv(t, name)
	}
	workingDir := t.TempDir()
	logDir := filepath.Join(t.TempDir(), "logs")
	return Runner{
			Binary:  os.Args[0],
			KeepEnv: []string{helperEnabled, helperRecord},
			LogDir:  logDir,
		}, Request{
			TaskID:     "task-123",
			WorkingDir: workingDir,
			Prompt:     "test prompt",
			Timeout:    5 * time.Second,
		}, recordPath
}

func withGitCommonDir(t *testing.T, req Request) Request {
	t.Helper()
	req.GitCommonDir = t.TempDir()
	return req
}

func readHelperRecord(t *testing.T, path string) helperRecordData {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record helperRecordData
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func countArg(args []string, target string) int {
	var count int
	for _, arg := range args {
		if arg == target {
			count++
		}
	}
	return count
}

func expectedPolicyArgs(keep []string) []string {
	args := []string{
		"-c", "shell_environment_policy.inherit=all",
		"-c", "shell_environment_policy.ignore_default_excludes=false",
	}
	names := append([]string{"PATH", "HOME", "LANG", "TMPDIR", "TMP", "TEMP"}, keep...)
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "CODEX_API_KEY" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		args = append(args, "-c", `shell_environment_policy.filters.`+name+`="include"`)
	}
	return args
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file %q still exists or stat failed: %v", path, err)
	}
}

func assertNotCreated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper record %q unexpectedly exists: %v", path, err)
	}
}

func setEnv(t *testing.T, name, value string) {
	t.Helper()
	t.Setenv(name, value)
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	value, present := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(name, value)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func processAlive(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	fields := strings.Fields(string(data))
	return len(fields) < 3 || fields[2] != "Z"
}

func killProcessFromFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func waitForHelperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var record helperRecordData
			if json.Unmarshal(data, &record) == nil && record.PID > 0 {
				return record.PID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper record %q did not contain a PID", path)
	return 0
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %q was not written", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("helper process %d did not exit", pid)
	}
}
