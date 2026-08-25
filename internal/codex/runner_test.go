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
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"qqcodex/internal/model"
	"qqcodex/internal/tasklog"
)

const (
	helperEnabled     = "GO_WANT_CODEX_HELPER"
	helperRecord      = "QQ_CODEX_HELPER_RECORD"
	helperFinal       = "QQ_CODEX_HELPER_FINAL"
	helperFinalSize   = "QQ_CODEX_HELPER_FINAL_SIZE"
	helperFinalSecret = "QQ_CODEX_HELPER_FINAL_SECRET"
	helperStderr      = "QQ_CODEX_HELPER_STDERR"
	helperStderrSize  = "QQ_CODEX_HELPER_STDERR_SIZE"
	helperExit        = "QQ_CODEX_HELPER_EXIT"
	helperSleep       = "QQ_CODEX_HELPER_SLEEP"
	helperLarge       = "QQ_CODEX_HELPER_LARGE"
	helperSpawn       = "QQ_CODEX_HELPER_SPAWN"
	helperPID         = "QQ_CODEX_HELPER_PID"
	helperChild       = "QQ_CODEX_HELPER_CHILD"
	helperStdout      = "QQ_CODEX_HELPER_STDOUT"
	helperDirty       = "QQ_CODEX_HELPER_DIRTY_TEMP"
	helperMany        = "QQ_CODEX_HELPER_MANY_EVENTS"
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
	TempDir       string   `json:"temp_dir"`
	TempMode      uint32   `json:"temp_mode"`
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
	record.TempDir = os.Getenv("TMPDIR")
	if info, err := os.Stat(record.TempDir); err == nil {
		record.TempMode = uint32(info.Mode().Perm())
	}
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
		final := envOr(helperFinal, "final answer")
		if size, _ := strconv.Atoi(os.Getenv(helperFinalSize)); size > 0 {
			final = strings.Repeat("h", size/2) + strings.Repeat("t", size-size/2)
		}
		if secret := os.Getenv(helperFinalSecret); secret != "" {
			final = strings.Repeat("p", maxFinalBytes/2-len(secret)/2) + secret + strings.Repeat("q", maxFinalBytes)
		}
		_ = os.WriteFile(record.LastPath, []byte(final), 0o600)
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
	} else if os.Getenv(helperDirty) == "populate-git-temp" {
		nested := filepath.Join(record.TempDir, "nested")
		helperMust(os.Mkdir(nested, 0o700))
		helperMust(os.WriteFile(filepath.Join(nested, "content"), []byte("temporary"), 0o600))
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
		if count, _ := strconv.Atoi(os.Getenv(helperMany)); count > 0 {
			for i := range count {
				helperPrintln(fmt.Sprintf(`{"type":"item.completed","index":%d,"data":"%s"}`, i, strings.Repeat("e", 128)))
			}
		}
		if size, _ := strconv.Atoi(os.Getenv(helperLarge)); size > 0 {
			_, err = fmt.Fprintf(os.Stdout, `{"type":"item.completed","data":"%s"}`+"\n", strings.Repeat("x", size))
			helperMust(err)
		}
		helperPrintln(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
	}
	stderr := os.Getenv(helperStderr)
	if size, _ := strconv.Atoi(os.Getenv(helperStderrSize)); size > 0 {
		stderr = strings.Repeat("s", size)
	}
	_, _ = fmt.Fprint(os.Stderr, stderr)
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
	if record.SchemaMode != 0o600 {
		t.Fatalf("schema mode = %o, want 600", record.SchemaMode)
	}
	if record.LastPath != "/proc/self/fd/3" {
		t.Fatalf("final output path = %q, want inherited pipe", record.LastPath)
	}
	wantSchema, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if record.SchemaContent != string(wantSchema) {
		t.Fatalf("schema content differs from embedded schema")
	}
	assertRemoved(t, record.SchemaPath)
	assertNoFinalOutputFile(t, runner.LogDir)
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
	want = append(want, expectedWorkspaceTempArgs()...)
	want = append(want, "-C", req.WorkingDir, "--sandbox", "workspace-write", "--add-dir", req.GitCommonDir,
		"--json", "-o", record.LastPath, "--", req.Prompt)
	if !reflect.DeepEqual(record.Args, want) {
		t.Fatalf("argv = %#v, want %#v", record.Args, want)
	}
	if got := countArg(record.Args, "--add-dir"); got != 1 {
		t.Fatalf("--add-dir count = %d, want 1", got)
	}
	assertPrivateGitTemp(t, record, req.GitCommonDir)
	assertNoFinalOutputFile(t, runner.LogDir)
}

func TestRunnerResumeArgumentsAndWorkingDirectory(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.SessionID = "session-exact"
	req.Prompt = "--last"
	req.GitCommonDir = t.TempDir()

	if _, err := runner.Resume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := append([]string{"exec", "resume"}, expectedPolicyArgs(runner.KeepEnv)...)
	want = append(want, expectedWorkspaceTempArgs()...)
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
	assertPrivateGitTemp(t, record, req.GitCommonDir)
}

func TestRunnerSanitizesEnvironment(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	setEnv(t, "CODEX_API_KEY", "codex-key")
	setEnv(t, "PATH", "/bin")
	setEnv(t, "HOME", "/private/home")
	unsetEnv(t, "CODEX_HOME")
	setEnv(t, "LANG", "C.UTF-8")
	setEnv(t, "TMPDIR", "/tmp")
	unsetEnv(t, "TMP")
	unsetEnv(t, "TEMP")
	setEnv(t, "ALLOWED_CUSTOM", "kept")
	setEnv(t, "UNKNOWN_SECRET", "must-not-leak")
	runner.KeepEnv = append(runner.KeepEnv, "ALLOWED_CUSTOM", "PATH", "CODEX_API_KEY", "CODEX_HOME", "ALLOWED_CUSTOM")

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := []string{
		"CODEX_API_KEY=codex-key", "CODEX_HOME=/private/home/.codex", "PATH=/bin", "HOME=/private/home", "LANG=C.UTF-8", "TMPDIR=/tmp",
		"GO_WANT_CODEX_HELPER=1", "QQ_CODEX_HELPER_RECORD=" + recordPath, "ALLOWED_CUSTOM=kept",
	}
	if !reflect.DeepEqual(record.Env, want) {
		t.Fatalf("environment = %#v, want %#v", record.Env, want)
	}
	for _, arg := range record.Args {
		if strings.Contains(arg, "filters.CODEX_API_KEY") || strings.Contains(arg, "filters.CODEX_HOME") {
			t.Fatalf("authentication environment exposed to tool subprocess policy: %q", arg)
		}
	}
	wantPolicy := expectedPolicyArgs(runner.KeepEnv)
	if len(record.Args) < 1+len(wantPolicy) || !slices.Equal(record.Args[1:1+len(wantPolicy)], wantPolicy) {
		t.Fatalf("argv = %#v, want policy prefix %#v", record.Args, wantPolicy)
	}
}

func TestRunnerRequiresAPIKeyAndAbsoluteHomes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{"empty API key", func(t *testing.T) { setEnv(t, "CODEX_API_KEY", "") }, "CODEX_API_KEY"},
		{"missing API key", func(t *testing.T) { unsetEnv(t, "CODEX_API_KEY") }, "CODEX_API_KEY"},
		{"empty HOME", func(t *testing.T) { setEnv(t, "HOME", "") }, "HOME"},
		{"relative HOME", func(t *testing.T) { setEnv(t, "HOME", "relative") }, "HOME"},
		{"empty CODEX_HOME", func(t *testing.T) { setEnv(t, "CODEX_HOME", "") }, "CODEX_HOME"},
		{"relative CODEX_HOME", func(t *testing.T) { setEnv(t, "CODEX_HOME", "relative") }, "CODEX_HOME"},
		{"auth lookup error", func(t *testing.T) {
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, []byte("block auth lookup"), 0o600); err != nil {
				t.Fatal(err)
			}
			setEnv(t, "CODEX_HOME", filepath.Join(blocker, "codex"))
		}, "inspect Codex auth.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, recordPath := helperRunner(t)
			tt.mutate(t)

			_, err := runner.Plan(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q validation error", err, tt.want)
			}
			assertNotCreated(t, recordPath)
		})
	}
}

func TestRunnerRejectsCachedCodexAuthenticationBeforeSpawning(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"token":"cached"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, req, recordPath := helperRunner(t)
			codexHome := t.TempDir()
			setEnv(t, "CODEX_HOME", codexHome)
			tt.setup(t, filepath.Join(codexHome, "auth.json"))

			_, err := runner.Plan(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "auth.json") {
				t.Fatalf("error = %v, want cached authentication error", err)
			}
			assertNotCreated(t, recordPath)
		})
	}
}

func TestRunnerRejectsDefaultHomeAuthenticationWithExplicitCodexHome(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	setEnv(t, "CODEX_HOME", t.TempDir())
	defaultCodexHome := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.Mkdir(defaultCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultCodexHome, "auth.json"), []byte(`{"token":"cached"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runner.Plan(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "auth.json") {
		t.Fatalf("error = %v, want default HOME authentication rejection", err)
	}
	assertNotCreated(t, recordPath)
}

func TestRunnerPassesAmbientCodexHomeOnlyToParentProcess(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	codexHome := t.TempDir()
	setEnv(t, "CODEX_HOME", codexHome)
	runner.KeepEnv = append(runner.KeepEnv, "CODEX_HOME", "CODEX_HOME")

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	if got := environmentValue(record.Env, "CODEX_HOME"); got != codexHome {
		t.Fatalf("CODEX_HOME = %q, want %q", got, codexHome)
	}
	for _, arg := range record.Args {
		if strings.Contains(arg, "filters.CODEX_HOME") {
			t.Fatalf("CODEX_HOME exposed to tool subprocess policy: %q", arg)
		}
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
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperFinal, "partial final")
	setEnv(t, helperStderr, "private-secret-stderr")
	setEnv(t, helperExit, "7")
	runner.KeepEnv = append(runner.KeepEnv, helperFinal, helperStderr, helperExit)
	sink := &recordingLogSink{}
	runner.Log = sink

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
	if got := sink.joined("codex.stderr"); got != "private-secret-stderr" {
		t.Fatalf("codex.stderr = %q", got)
	}
	info, statErr := os.Stat(runner.LogDir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestRunnerLogsTruncatedStderrWithRealTaskLogStore(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperStderrSize, strconv.Itoa(maxStderrBytes+1024))
	runner.KeepEnv = append(runner.KeepEnv, helperStderrSize)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(req.TaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "codex stderr truncated") {
		t.Fatalf("task log omitted stderr truncation marker: %q", summary)
	}
}

func TestRunnerRoutesEventsStderrAndFinalToLogSink(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperStderr, "private helper detail\n")
	runner.KeepEnv = append(runner.KeepEnv, helperStderr)
	sink := &recordingLogSink{}
	runner.Log = sink

	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.joined("codex.events"); !strings.Contains(got, `"thread.started"`) {
		t.Fatalf("codex.events = %q, want JSONL events", got)
	}
	if got := sink.joined("codex.stderr"); got != "private helper detail\n" {
		t.Fatalf("codex.stderr = %q", got)
	}
	if got := sink.joined("codex.final"); got != result.Final {
		t.Fatalf("codex.final = %q, want %q", got, result.Final)
	}
	for _, entry := range sink.entriesCopy() {
		if entry.taskID != req.TaskID {
			t.Fatalf("logged task ID = %q, want %q", entry.taskID, req.TaskID)
		}
	}
	entries, readErr := os.ReadDir(runner.LogDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".stderr.log") {
			t.Fatalf("raw stderr file remains: %s", entry.Name())
		}
	}
}

func TestRunnerBoundsReturnedEventsWhileLoggingEveryLine(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperMany, "12000")
	runner.KeepEnv = append(runner.KeepEnv, helperMany)
	sink := &recordingLogSink{}
	runner.Log = sink

	result, err := runner.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.EventsJSONL) > 1<<20 {
		t.Fatalf("returned events length = %d, want <= 1 MiB", len(result.EventsJSONL))
	}
	if got := strings.Count(sink.joined("codex.events"), "\n"); got != 12003 {
		t.Fatalf("logged event count = %d, want 12003", got)
	}
}

func TestRunnerFailsAndCancelsWhenEventLoggingFails(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	wantErr := errors.New("audit sink unavailable")
	runner.Log = failingLogSink{stream: "codex.events", err: wantErr}

	_, err := runner.Plan(context.Background(), req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Plan() error = %v, want audit sink error", err)
	}
}

func TestRunnerTaskLogIntegrationRedactsEveryStream(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	secret := "company-codex-secret"
	setEnv(t, helperStdout, `{"type":"thread.started","thread_id":"thread-123","data":"`+secret+`"}`+"\n")
	setEnv(t, helperStderr, "CODEX_API_KEY="+secret+"\n")
	setEnv(t, helperFinal, "Bearer "+secret)
	runner.KeepEnv = append(runner.KeepEnv, helperStdout, helperStderr, helperFinal)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got, err := store.Summary(req.TaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("task log leaked secret: %s", got)
	}
	for _, stream := range []string{"codex.events", "codex.stderr", "codex.final"} {
		if !strings.Contains(got, `"stream":"`+stream+`"`) {
			t.Fatalf("task log lacks %s: %s", stream, got)
		}
	}
}

func TestRunnerTaskLogRedactsJSONEscapedSecretForms(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	secrets := []string{`quote"secret`, `back\slash-secret`, "control\tsecret"}
	event, err := json.Marshal(map[string]any{
		"type":      "thread.started",
		"thread_id": "thread-123",
		"values":    secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, helperStdout, string(event)+"\n")
	setEnv(t, helperStderr, strings.Join(secrets, "|"))
	setEnv(t, helperFinal, strings.Join(secrets, "|"))
	runner.KeepEnv = append(runner.KeepEnv, helperStdout, helperStderr, helperFinal)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), secrets)
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(store.Root, req.TaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(req.TaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecretForms(t, string(raw), secrets)
	assertNoSecretForms(t, summary, secrets)
	assertDecodedTaskLogHasNoSecrets(t, raw, secrets)
}

func TestRunnerOversizedEventFailsThroughTaskLogSink(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperLarge, strconv.Itoa((2<<20)+1024))
	runner.KeepEnv = append(runner.KeepEnv, helperLarge)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	_, err = runner.Plan(context.Background(), req)
	if !errors.Is(err, tasklog.ErrLimitExceeded) {
		t.Fatalf("Plan() error = %v, want task log record limit", err)
	}
}

func TestRunnerBoundsOversizedFinalAndReturnsMarkerOnly(t *testing.T) {
	runner, req, _ := helperRunner(t)
	setEnv(t, helperFinalSize, strconv.Itoa(maxFinalBytes+4096))
	runner.KeepEnv = append(runner.KeepEnv, helperFinalSize)
	sink := &recordingLogSink{}
	runner.Log = sink

	result, err := runner.Plan(context.Background(), req)
	if !errors.Is(err, ErrFinalTooLarge) {
		t.Fatalf("Plan() error = %v, want final output limit", err)
	}
	if result.Final != finalTruncationMarker || !utf8.ValidString(result.Final) {
		t.Fatalf("bounded final = %q, want valid marker only", result.Final)
	}
	if got := sink.joined("codex.final"); got != result.Final {
		t.Fatalf("logged final differs from bounded Result: length=%d", len(got))
	}
}

func TestRunnerStreamsOversizedFinalThroughPipeWithoutLeavingFile(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	setEnv(t, helperFinalSize, strconv.Itoa(32<<20))
	runner.KeepEnv = append(runner.KeepEnv, helperFinalSize)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	result, err := runner.Plan(context.Background(), req)
	if !errors.Is(err, ErrFinalTooLarge) {
		t.Fatalf("Plan() error = %v, want final output limit", err)
	}
	if result.Final != finalTruncationMarker {
		t.Fatalf("final = %q, want marker only", result.Final)
	}
	assertNoFinalOutputFile(t, runner.LogDir)
	summary, err := store.Summary(req.TaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "codex final output truncated") || strings.Contains(summary, strings.Repeat("h", 64)) {
		t.Fatalf("task log summary retained oversized final data: %q", summary)
	}
}

func TestRunnerDropsOversizedFinalBeforeTaskLogRedaction(t *testing.T) {
	runner, req, _ := helperRunner(t)
	req.TaskID = "T-ABCDEF012345"
	secret := strings.Repeat("密", 100) + `quote"\\` + "\tcontrol"
	setEnv(t, helperFinalSecret, secret)
	runner.KeepEnv = append(runner.KeepEnv, helperFinalSecret)
	store, err := tasklog.Open(filepath.Join(t.TempDir(), "task-logs"), []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	runner.Log = store

	result, err := runner.Plan(context.Background(), req)
	if !errors.Is(err, ErrFinalTooLarge) || result.Final != finalTruncationMarker {
		t.Fatalf("final result = %q, error = %v; want marker and ErrFinalTooLarge", result.Final, err)
	}
	raw, err := os.ReadFile(filepath.Join(store.Root, req.TaskID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(req.TaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{string(raw), summary, result.Final} {
		if strings.Contains(value, secret) || strings.Contains(value, "quote") || strings.Contains(value, "control") {
			t.Fatalf("oversized final secret leaked: %q", value)
		}
	}
	if !utf8.ValidString(result.Final) {
		t.Fatal("final marker is not valid UTF-8")
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
	setEnv(t, helperLarge, strconv.Itoa((3<<20)+1024))
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
			return r.Resume(context.Background(), withGitCommonDir(t, req))
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

func TestRunnerRemovesGitTemporaryDirectoryAfterFailure(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.GitCommonDir = t.TempDir()
	setEnv(t, helperExit, "7")
	setEnv(t, helperDirty, "populate-git-temp")
	runner.KeepEnv = append(runner.KeepEnv, helperExit, helperDirty)

	result, err := runner.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected nonzero-exit error")
	}
	record := readHelperRecord(t, recordPath)
	assertPrivateGitTemp(t, record, req.GitCommonDir)
	if result.SessionID != "thread-123" || result.Final != "final answer" {
		t.Fatalf("partial result = %#v", result)
	}
}

func TestRunnerRemovesGitTemporaryDirectoryAfterCancellation(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.GitCommonDir = t.TempDir()
	setEnv(t, helperSleep, "10s")
	runner.KeepEnv = append(runner.KeepEnv, helperSleep)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runner.Execute(ctx, req)
		done <- outcome{result: result, err: err}
	}()
	waitForHelperPID(t, recordPath)
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", got.err)
	}
	record := readHelperRecord(t, recordPath)
	assertPrivateGitTemp(t, record, req.GitCommonDir)
	if got.result.SessionID != "thread-123" || got.result.Final != "final answer" {
		t.Fatalf("partial result = %#v", got.result)
	}
}

func TestRunnerRemovesGitTemporaryDirectoryAfterSetupError(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	req.GitCommonDir = t.TempDir()
	logFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(logFile, []byte("block log setup"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.LogDir = logFile

	_, err := runner.Execute(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "log directory") {
		t.Fatalf("error = %v, want log setup error", err)
	}
	assertNotCreated(t, recordPath)
	entries, err := os.ReadDir(req.GitCommonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Git common directory contains leaked setup files: %#v", entries)
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
	assertNoFinalOutputFile(t, runner.LogDir)
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
		{"empty git common directory for resume", "resume", func(_ *Runner, q *Request) { q.GitCommonDir = "" }},
		{"relative git common directory for resume", "resume", func(_ *Runner, q *Request) { q.GitCommonDir = "relative" }},
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
				req = withGitCommonDir(t, req)
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
	setEnv(t, "CODEX_API_KEY", "test-codex-api-key")
	setEnv(t, "HOME", t.TempDir())
	unsetEnv(t, "CODEX_HOME")
	setEnv(t, helperEnabled, "1")
	setEnv(t, helperRecord, recordPath)
	for _, name := range []string{
		helperFinal, helperFinalSize, helperFinalSecret, helperStderr, helperStderrSize, helperExit, helperSleep, helperLarge, helperSpawn,
		helperPID, helperChild, helperStdout, helperDirty, helperMany,
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

func assertNoSecretForms(t *testing.T, value string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		form := secret
		for range 3 {
			if strings.Contains(value, form) {
				t.Fatalf("task log contains recoverable secret form %q", form)
			}
			encoded, err := json.Marshal(form)
			if err != nil {
				t.Fatal(err)
			}
			form = string(encoded[1 : len(encoded)-1])
		}
	}
}

func assertDecodedTaskLogHasNoSecrets(t *testing.T, data []byte, secrets []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry struct {
			Stream string `json:"stream"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		assertNoSecretForms(t, entry.Data, secrets)
		if entry.Stream != "codex.events" {
			continue
		}
		var event struct {
			Values []string `json:"values"`
		}
		if err := json.Unmarshal([]byte(entry.Data), &event); err != nil {
			t.Fatal(err)
		}
		for _, value := range event.Values {
			for _, secret := range secrets {
				if value == secret {
					t.Fatalf("decoded event restored secret %q", secret)
				}
			}
		}
	}
}

type logEntry struct {
	taskID string
	stream string
	data   string
}

type recordingLogSink struct {
	mu      sync.Mutex
	entries []logEntry
}

func (s *recordingLogSink) Append(taskID, stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, logEntry{taskID: taskID, stream: stream, data: string(data)})
	return nil
}

func (s *recordingLogSink) entriesCopy() []logEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logEntry(nil), s.entries...)
}

func (s *recordingLogSink) joined(stream string) string {
	var value strings.Builder
	for _, entry := range s.entriesCopy() {
		if entry.stream == stream {
			value.WriteString(entry.data)
		}
	}
	return value.String()
}

type failingLogSink struct {
	stream string
	err    error
}

func (s failingLogSink) Append(_ string, stream string, _ []byte) error {
	if stream == s.stream {
		return s.err
	}
	return nil
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
		if name == "CODEX_API_KEY" || name == "CODEX_HOME" {
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

func expectedWorkspaceTempArgs() []string {
	return []string{
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
	}
}

func environmentValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, prefix); ok {
			return value
		}
	}
	return ""
}

func assertPrivateGitTemp(t *testing.T, record helperRecordData, gitCommonDir string) {
	t.Helper()
	if record.TempDir == "" {
		t.Fatal("Codex process did not receive TMPDIR")
	}
	rel, err := filepath.Rel(gitCommonDir, record.TempDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("temporary directory %q is not a child of Git common directory %q", record.TempDir, gitCommonDir)
	}
	if record.TempMode != 0o700 {
		t.Fatalf("temporary directory mode = %o, want 700", record.TempMode)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := environmentValue(record.Env, name); got != record.TempDir {
			t.Errorf("%s = %q, want %q", name, got, record.TempDir)
		}
	}
	assertRemoved(t, record.TempDir)
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file %q still exists or stat failed: %v", path, err)
	}
}

func assertNoFinalOutputFile(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "-last-") {
			t.Fatalf("final output file leaked: %q", entry.Name())
		}
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
