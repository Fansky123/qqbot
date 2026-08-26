package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"qqcodex/internal/tasklog"
)

func TestClientBuildsTypedArgvAndParsesResponse(t *testing.T) {
	ctx := context.Background()
	record := filepath.Join(t.TempDir(), "argv.json")
	source, commit := clientTaskSource(t)
	client := mustNewClient(t, helperCommand(t, record, "success"), map[string]string{"order-api": source})
	device, inode, err := directoryIdentity(source)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() (string, error)
		want []string
	}{
		{
			name: "validate",
			call: func() (string, error) {
				return "", client.Preflight(ctx, "order-api", "origin", "main", "rc", [][]string{{"go", "test", "./..."}})
			},
			want: []string{
				"validate", "--project", "order-api",
				"--config-sha256", ProjectFingerprint(Project{Remote: "origin", BaseBranch: "main", RCBranch: "rc", Checks: [][]string{{"go", "test", "./..."}}}),
				"--source-device", strconv.FormatUint(device, 10), "--source-inode", strconv.FormatUint(inode, 10),
			},
		},
		{
			name: "sync",
			call: func() (string, error) {
				return "", client.Sync(ctx, "order-api")
			},
			want: []string{"sync", "--project", "order-api"},
		},
		{
			name: "push",
			call: func() (string, error) {
				return "", client.PushTask(ctx, "order-api", testTaskID, "codex/"+testTaskID, commit)
			},
			want: []string{"push", "--project", "order-api", "--task", testTaskID, "--branch", "codex/" + testTaskID, "--commit", commit},
		},
		{
			name: "merge",
			call: func() (string, error) {
				return client.MergeRC(ctx, "order-api", testTaskID, commit)
			},
			want: []string{"merge", "--project", "order-api", "--task", testTaskID, "--commit", commit},
		},
		{
			name: "deploy",
			call: func() (string, error) {
				return "", client.DeployRC(ctx, "order-api", testTaskID, commit)
			},
			want: []string{"deploy", "--project", "order-api", "--task", testTaskID, "--rc-commit", commit},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCommit, err := tt.call()
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "merge" && gotCommit != commit {
				t.Fatalf("MergeRC() commit = %q, want %q", gotCommit, commit)
			}
			var got []string
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv = %#v, want %#v", got, tt.want)
			}
			if tt.name == "validate" && strings.Contains(strings.Join(got, "\x00"), source) {
				t.Fatal("validate argv exposed source repository path")
			}
		})
	}
}

func TestClientRejectsInvalidOrMultipleJSONResponses(t *testing.T) {
	for _, mode := range []string{
		"unknown-field", "multiple", "success-with-error", "failure-without-error",
		"missing-status", "null-status",
	} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			client := mustNewClient(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), mode), nil)
			err := client.Sync(context.Background(), "order-api")
			if err == nil {
				t.Fatal("Sync() error = nil, want strict response error")
			}
			if (mode == "missing-status" || mode == "null-status") && err.Error() != "ops helper failed with an invalid response" {
				t.Fatalf("Sync() error = %v, want invalid response error", err)
			}
		})
	}
}

func TestClientPreservesTypedRemoteCommitFailures(t *testing.T) {
	for _, test := range []struct {
		mode          string
		want          error
		changedCommit string
	}{
		{"task-commit-changed", ErrTaskCommitChanged, strings.Repeat("b", 40)},
		{"rc-commit-changed", ErrRCCommitChanged, strings.Repeat("b", 40)},
		{"merge-conflict", ErrMergeConflict, ""},
	} {
		t.Run(test.mode, func(t *testing.T) {
			client := mustNewClient(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), test.mode), nil)
			err := client.DeployRC(context.Background(), "order-api", testTaskID, strings.Repeat("a", 40))
			if !errors.Is(err, test.want) {
				t.Fatalf("typed helper error = %v, want %v", err, test.want)
			}
			if got := ChangedCommit(err); got != test.changedCommit {
				t.Fatalf("changed commit = %q", got)
			}
		})
	}
}

func TestClientDoesNotPassCodexCredential(t *testing.T) {
	client := mustNewClient(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "check-env"), nil)
	t.Setenv("CODEX_API_KEY", "must-not-cross-boundary")
	t.Setenv("OPENAI_API_KEY", "must-not-cross-boundary")
	t.Setenv("NAPCAT_ACCESS_TOKEN", "must-not-cross-boundary")
	t.Setenv("CUSTOM_TOKEN", "must-not-cross-boundary")
	t.Setenv("HOME", "/attacker/home")
	t.Setenv("PATH", "/attacker/bin")
	if err := client.Sync(context.Background(), "order-api"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsUntrustedExecutable(t *testing.T) {
	repo := filepath.Join(privateTempDir(t), "source")
	git(t, filepath.Dir(repo), "init", repo)
	workerOwned := writeExecutable(t, filepath.Join(privateTempDir(t), "qqcodex-ops"), "#!/bin/sh\nexit 0\n")
	if _, err := NewClient([]string{workerOwned, "-config", "/etc/hosts"}, map[string]string{"order-api": repo}, nil); err == nil {
		t.Fatal("NewClient() error = nil, want worker-owned executable rejection")
	}
}

func TestNewClientAcceptsOnlyRootOwnedExecutables(t *testing.T) {
	repo := filepath.Join(privateTempDir(t), "source")
	git(t, filepath.Dir(repo), "init", repo)
	rootCommand := realExecutable(t, "true")
	validCommand := []string{rootCommand, "-config", "/etc/hosts"}
	client, err := NewClient(validCommand, map[string]string{"order-api": repo}, nil)
	if err != nil {
		t.Fatalf("NewClient() rejected root-owned command and source Git: %v", err)
	}
	validCommand[1] = "--changed"
	if client.command[1] != "-config" {
		t.Fatal("NewClient() did not deep-copy command")
	}

	gitDir := privateTempDir(t)
	writeExecutable(t, filepath.Join(gitDir, "git"), "#!/bin/sh\nexec /usr/bin/git \"$@\"\n")
	t.Setenv("PATH", gitDir+":/usr/bin:/bin")
	if _, err := NewClient([]string{rootCommand, "-config", "/etc/hosts"}, map[string]string{"order-api": repo}, nil); err == nil {
		t.Fatal("NewClient() error = nil, want worker-owned source Git rejection")
	}
}

func TestNewClientRejectsCommandsOutsideFixedContract(t *testing.T) {
	repo := filepath.Join(privateTempDir(t), "source")
	git(t, filepath.Dir(repo), "init", repo)
	helper := realExecutable(t, "true")
	workerConfig := filepath.Join(privateTempDir(t), "ops.json")
	writeFile(t, workerConfig, "{}")
	writableConfig := filepath.Join(privateTempDir(t), "writable-ops.json")
	writeFile(t, writableConfig, "{}")
	if err := os.Chmod(writableConfig, 0o666); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command []string
	}{
		{name: "shell worker script", command: []string{realExecutable(t, "sh"), workerConfig}},
		{name: "missing config flag", command: []string{helper, "/etc/hosts"}},
		{name: "duplicate config flag", command: []string{helper, "-config", "/etc/hosts", "-config", "/etc/hosts"}},
		{name: "extra argument", command: []string{helper, "-config", "/etc/hosts", "sync"}},
		{name: "worker-owned config", command: []string{helper, "-config", workerConfig}},
		{name: "writable config", command: []string{helper, "-config", writableConfig}},
		{name: "relative config", command: []string{helper, "-config", "ops.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewClient(tt.command, map[string]string{"order-api": repo}, nil); err == nil {
				t.Fatal("NewClient() error = nil, want fixed command rejection")
			}
		})
	}
}

func TestClientDoesNotExposeHelperStderr(t *testing.T) {
	client := mustNewClient(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "secret-stderr"), nil)
	err := client.Sync(context.Background(), "order-api")
	if err == nil || err.Error() != "public failure" {
		t.Fatalf("Sync() error = %v, want only public JSON error", err)
	}
}

func TestClientLogsTaskHelperStderrWithoutExposingIt(t *testing.T) {
	sink := &opsRecordingSink{}
	client := mustNewClientWithLog(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "secret-stderr"), nil, sink)
	_, err := client.MergeRC(context.Background(), "order-api", testTaskID, strings.Repeat("a", 40))
	if err == nil || err.Error() != "public failure" {
		t.Fatalf("MergeRC() error = %v, want only public JSON error", err)
	}
	if got := sink.joined("ops.stderr"); got != "PRIVATE-OPS-CREDENTIAL\n" {
		t.Fatalf("ops.stderr = %q", got)
	}
}

func TestClientLogsTruncatedHelperStderrWithRealTaskLogStore(t *testing.T) {
	root := t.TempDir()
	store, err := tasklog.Open(filepath.Join(root, "task-logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mustNewClientWithLog(t, helperCommand(t, filepath.Join(root, "argv.json"), "large-stderr"), nil, store)
	_, err = client.MergeRC(context.Background(), "order-api", testTaskID, strings.Repeat("a", 40))
	if err == nil || err.Error() != "public failure" {
		t.Fatalf("MergeRC() error = %v, want public failure", err)
	}
	summary, err := store.Summary(testTaskID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "ops stderr truncated") {
		t.Fatalf("task log omitted ops stderr truncation marker: %q", summary)
	}
}

func TestClientDoesNotFailCompletedActionWhenTaskLogFails(t *testing.T) {
	wantErr := errors.New("task log unavailable")
	record := filepath.Join(t.TempDir(), "argv.json")
	client := mustNewClientWithLog(t, helperCommand(t, record, "success-stderr"), nil, opsFailingSink{err: wantErr})
	commit := strings.Repeat("a", 40)

	got, err := client.MergeRC(context.Background(), "order-api", testTaskID, commit)
	if err != nil || got != commit {
		t.Fatalf("MergeRC() = %q, %v; want completed commit despite audit failure", got, err)
	}
}

func TestClientDoesNotCreatePerTaskLogForSync(t *testing.T) {
	sink := &opsRecordingSink{}
	client := mustNewClientWithLog(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "success-stderr"), nil, sink)
	if err := client.Sync(context.Background(), "order-api"); err != nil {
		t.Fatal(err)
	}
	if len(sink.entries) != 0 {
		t.Fatalf("Sync() task logs = %#v, want none without task ID", sink.entries)
	}
}

func TestClientJoinsTaskLogFailureWithFailedAction(t *testing.T) {
	wantErr := errors.New("task log unavailable")
	client := mustNewClientWithLog(t, helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "secret-stderr"), nil, opsFailingSink{err: wantErr})

	_, err := client.MergeRC(context.Background(), "order-api", testTaskID, strings.Repeat("a", 40))
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "public failure") {
		t.Fatalf("MergeRC() error = %v, want public and audit failures", err)
	}
}

func TestOpsClientHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "qqcodex-client-helper" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	if separator < 0 || len(os.Args) < separator+4 {
		os.Exit(120)
	}
	record, mode := os.Args[separator+1], os.Args[separator+2]
	args := os.Args[separator+3:]
	data, _ := json.Marshal(args)
	_ = os.WriteFile(record, data, 0o600)
	if len(args) > 0 && args[0] == "push" {
		bundle, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(record+".bundle", bundle, 0o600)
	}
	commit := strings.Repeat("a", 40)
	for i, arg := range args {
		if arg == "--commit" && i+1 < len(args) {
			commit = args[i+1]
		}
	}
	switch mode {
	case "success":
		if len(args) > 0 && args[0] == "merge" {
			_, _ = os.Stdout.WriteString(`{"ok":true,"rc_commit":"` + commit + `"}`)
		} else {
			_, _ = os.Stdout.WriteString(`{"ok":true}`)
		}
	case "unknown-field":
		_, _ = os.Stdout.WriteString(`{"ok":true,"extra":1}`)
	case "multiple":
		_, _ = os.Stdout.WriteString("{\"ok\":true}\n{\"ok\":true}\n")
	case "success-with-error":
		_, _ = os.Stdout.WriteString(`{"ok":true,"error":"bad"}`)
	case "failure-without-error":
		_, _ = os.Stdout.WriteString(`{"ok":false}`)
	case "missing-status":
		_, _ = os.Stdout.WriteString(`{"error":"public failure"}`)
		os.Exit(1)
	case "null-status":
		_, _ = os.Stdout.WriteString(`{"ok":null,"error":"public failure"}`)
		os.Exit(1)
	case "check-env":
		if os.Getenv("PATH") != "/usr/bin:/bin" || os.Getenv("HOME") != "/nonexistent" ||
			os.Getenv("CODEX_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != "" ||
			os.Getenv("NAPCAT_ACCESS_TOKEN") != "" || os.Getenv("CUSTOM_TOKEN") != "" {
			_, _ = os.Stdout.WriteString(`{"ok":false,"error":"credential leaked"}`)
			os.Exit(1)
		}
		_, _ = os.Stdout.WriteString(`{"ok":true}`)
	case "secret-stderr":
		_, _ = os.Stderr.WriteString("PRIVATE-OPS-CREDENTIAL\n")
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure"}`)
		os.Exit(1)
	case "success-stderr":
		_, _ = os.Stderr.WriteString("PRIVATE-OPS-CREDENTIAL\n")
		if len(args) > 0 && args[0] == "merge" {
			_, _ = os.Stdout.WriteString(`{"ok":true,"rc_commit":"` + commit + `"}`)
		} else {
			_, _ = os.Stdout.WriteString(`{"ok":true}`)
		}
	case "large-stderr":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'s'}, (1<<20)+1024))
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure"}`)
		os.Exit(1)
	case "task-commit-changed":
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure","error_code":"task_commit_changed","current_commit":"` + strings.Repeat("b", 40) + `"}`)
		os.Exit(1)
	case "rc-commit-changed":
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure","error_code":"rc_commit_changed","current_commit":"` + strings.Repeat("b", 40) + `"}`)
		os.Exit(1)
	case "merge-conflict":
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure","error_code":"merge_conflict"}`)
		os.Exit(1)
	default:
		os.Exit(121)
	}
	os.Exit(0)
}

func TestOpsBundleOperatorHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "qqcodex-bundle-operator-helper" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	if len(os.Args) < separator+12 {
		os.Exit(122)
	}
	configPath, record := os.Args[separator+1], os.Args[separator+2]
	args := os.Args[separator+3:]
	data, _ := json.Marshal(args)
	_ = os.WriteFile(record, data, 0o600)
	if err := validateClientInputs(args); err != nil || args[0] != "push" {
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"invalid push"}`)
		os.Exit(1)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"load failed"}`)
		os.Exit(1)
	}
	operator, err := NewOperator(cfg)
	if err == nil {
		err = operator.PushTaskBundle(context.Background(), args[2], args[4], args[6], args[8], os.Stdin)
	}
	if err != nil {
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"push failed"}`)
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString(`{"ok":true}`)
	os.Exit(0)
}

func helperCommand(t *testing.T, record, mode string) []string {
	t.Helper()
	trusted := copiedTestExecutable(t, "qqcodex-ops-test")
	return []string{trusted, "-test.run=^TestOpsClientHelper$", "--", "qqcodex-client-helper", record, mode}
}

func operatorHelperCommand(t *testing.T, configPath, record string) []string {
	t.Helper()
	trusted := copiedTestExecutable(t, "qqcodex-bundle-ops-test")
	return []string{trusted, "-test.run=^TestOpsBundleOperatorHelper$", "--", "qqcodex-bundle-operator-helper", configPath, record}
}

func copiedTestExecutable(t *testing.T, name string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(privateTempDir(t), name)
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trusted, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return trusted
}

func mustNewClient(t *testing.T, command []string, sourceRepos map[string]string) *Client {
	t.Helper()
	client, err := newClient(command, sourceRepos, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func mustNewClientWithLog(t *testing.T, command []string, sourceRepos map[string]string, log LogSink) *Client {
	t.Helper()
	client, err := newClient(command, sourceRepos, true, log)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type opsLogEntry struct {
	stream string
	data   string
}

type opsRecordingSink struct {
	mu      sync.Mutex
	entries []opsLogEntry
}

func (s *opsRecordingSink) Append(_ string, stream string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, opsLogEntry{stream: stream, data: string(data)})
	return nil
}

func (s *opsRecordingSink) joined(stream string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value strings.Builder
	for _, entry := range s.entries {
		if entry.stream == stream {
			value.WriteString(entry.data)
		}
	}
	return value.String()
}

type opsFailingSink struct{ err error }

func (s opsFailingSink) Append(string, string, []byte) error { return s.err }

func clientTaskSource(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(privateTempDir(t), "source")
	git(t, filepath.Dir(repo), "init", "-b", "main", repo)
	configureGitUser(t, repo)
	writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
	git(t, repo, "add", "base.txt")
	git(t, repo, "commit", "-m", "base")
	git(t, repo, "switch", "-c", "codex/"+testTaskID)
	writeFile(t, filepath.Join(repo, "task.txt"), "task\n")
	git(t, repo, "add", "task.txt")
	git(t, repo, "commit", "-m", "task")
	return repo, git(t, repo, "rev-parse", "HEAD")
}

func TestClientPushTaskSendsSingleRefBundleOnStdin(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.json")
	source, commit := clientTaskSource(t)
	client := mustNewClient(t, helperCommand(t, record, "success"), map[string]string{"order-api": source})

	if err := client.PushTask(context.Background(), "order-api", testTaskID, "codex/"+testTaskID, commit); err != nil {
		t.Fatal(err)
	}
	heads := git(t, source, "bundle", "list-heads", record+".bundle")
	want := commit + " refs/heads/codex/" + testTaskID
	if heads != want {
		t.Fatalf("bundle heads = %q, want %q", heads, want)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), source) {
		t.Fatalf("helper argv exposed source repository %q", source)
	}
}

func TestClientPushTaskRejectsClaimedCommitMismatch(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.json")
	source, _ := clientTaskSource(t)
	client := mustNewClient(t, helperCommand(t, record, "success"), map[string]string{"order-api": source})

	err := client.PushTask(context.Background(), "order-api", testTaskID, "codex/"+testTaskID, strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("PushTask() error = nil, want source commit mismatch")
	}
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatalf("helper started for mismatched commit, stat error = %v", statErr)
	}
}

func TestClientConstructionDeepCopiesSourceRepositories(t *testing.T) {
	source, commit := clientTaskSource(t)
	record := filepath.Join(t.TempDir(), "argv.json")
	repos := map[string]string{"order-api": source}
	client := mustNewClient(t, helperCommand(t, record, "success"), repos)
	repos["order-api"] = t.TempDir()

	if err := client.PushTask(context.Background(), "order-api", testTaskID, "codex/"+testTaskID, commit); err != nil {
		t.Fatal(err)
	}
}

func TestClientPushTaskTransfersBundleToIsolatedOperator(t *testing.T) {
	fixture := newOpsFixture(t)
	worker := fixture.clone(t)
	branch := "codex/" + testTaskID
	git(t, worker, "switch", "-c", branch, "origin/main")
	writeFile(t, filepath.Join(worker, "client-task.txt"), "client task\n")
	git(t, worker, "add", "client-task.txt")
	git(t, worker, "commit", "-m", "client task")
	commit := git(t, worker, "rev-parse", "HEAD")
	configPath := filepath.Join(privateTempDir(t), "ops.json")
	writeJSON(t, configPath, fixture.config)
	record := filepath.Join(t.TempDir(), "operator-argv.json")
	client := mustNewClient(t, operatorHelperCommand(t, configPath, record), map[string]string{fixture.projectID: worker})

	if err := client.PushTask(context.Background(), fixture.projectID, testTaskID, branch, commit); err != nil {
		t.Fatal(err)
	}
	if got := fixture.remoteRef(t, branch); got != commit {
		t.Fatalf("remote task commit = %q, want %q", got, commit)
	}
	if local := git(t, fixture.repo, "for-each-ref", "--format=%(refname)", "refs/heads/codex"); local != "" {
		t.Fatalf("ops clone acquired worker task ref %q", local)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), worker) {
		t.Fatalf("helper argv exposed worker repository %q", worker)
	}
}

func TestClientPushTaskKillsBundleProducerOnOverflow(t *testing.T) {
	source, commit := clientTaskSource(t)
	record := filepath.Join(t.TempDir(), "argv.json")
	marker := filepath.Join(t.TempDir(), "producer-pid")
	gitDir := privateTempDir(t)
	writeExecutable(t, filepath.Join(gitDir, "git"), `#!/bin/sh
if [ "$1" = bundle ] && [ "$2" = create ]; then
  echo $$ > "`+marker+`"
  while :; do /usr/bin/head -c 4096 /dev/zero; done
fi
exec /usr/bin/git "$@"
`)
	t.Setenv("PATH", gitDir+":/usr/bin:/bin")
	client := mustNewClient(t, helperCommand(t, record, "success"), map[string]string{"order-api": source})
	client.maxBundleBytes = 1024

	err := client.PushTask(context.Background(), "order-api", testTaskID, "codex/"+testTaskID, commit)
	if err == nil || !strings.Contains(err.Error(), "exceeded limit") {
		t.Fatalf("PushTask() error = %v, want bundle limit error", err)
	}
	assertProcessGone(t, marker)
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatalf("helper started after bundle overflow, stat error = %v", statErr)
	}
}

func TestClientPushTaskCancellationKillsBundleProducer(t *testing.T) {
	source, commit := clientTaskSource(t)
	record := filepath.Join(t.TempDir(), "argv.json")
	marker := filepath.Join(t.TempDir(), "producer-pid")
	gitDir := privateTempDir(t)
	writeExecutable(t, filepath.Join(gitDir, "git"), `#!/bin/sh
if [ "$1" = bundle ] && [ "$2" = create ]; then
  echo $$ > "`+marker+`"
  exec /usr/bin/sleep 600
fi
exec /usr/bin/git "$@"
`)
	t.Setenv("PATH", gitDir+":/usr/bin:/bin")
	client := mustNewClient(t, helperCommand(t, record, "success"), map[string]string{"order-api": source})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.PushTask(ctx, "order-api", testTaskID, "codex/"+testTaskID, commit)
	}()
	waitForFile(t, marker)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("PushTask() error = %v, want cancellation", err)
	}
	assertProcessGone(t, marker)
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatalf("helper started after bundle cancellation, stat error = %v", statErr)
	}
}

func assertProcessGone(t *testing.T, marker string) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bundle producer %d remains after failure: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
