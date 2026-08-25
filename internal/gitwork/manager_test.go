package gitwork

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"qqcodex/internal/config"
)

const (
	firstTaskID  = "T-012345ABCDEF"
	secondTaskID = "T-FEDCBA654321"
)

func TestManagerPrepareAndRemove(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	manager := Manager{Root: fixture.worktreeRoot}
	ctx := context.Background()

	first, err := manager.Prepare(ctx, fixture.project, firstTaskID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Prepare(ctx, fixture.project, secondTaskID)
	if err != nil {
		t.Fatal(err)
	}

	if first.Branch != "codex/"+firstTaskID || second.Branch != "codex/"+secondTaskID {
		t.Fatalf("branches = %q, %q", first.Branch, second.Branch)
	}
	if first.BaseCommit != fixture.mainCommit || second.BaseCommit != fixture.mainCommit {
		t.Fatalf("base commits = %q, %q, want %q", first.BaseCommit, second.BaseCommit, fixture.mainCommit)
	}
	assertWorktree(t, fixture.worktreeRoot, first)
	assertWorktree(t, fixture.worktreeRoot, second)
	if first.Path == second.Path {
		t.Fatal("two tasks share a worktree")
	}
	if first.GitCommonDir != second.GitCommonDir {
		t.Fatalf("Git common dirs differ: %q and %q", first.GitCommonDir, second.GitCommonDir)
	}
	wantCommonDir := git(t, fixture.project.RepoPath, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(wantCommonDir) {
		wantCommonDir = filepath.Join(fixture.project.RepoPath, wantCommonDir)
	}
	wantCommonDir, err = filepath.EvalSymlinks(wantCommonDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.GitCommonDir != wantCommonDir {
		t.Fatalf("Git common dir = %q, want %q", first.GitCommonDir, wantCommonDir)
	}

	writeFile(t, filepath.Join(first.Path, "untracked.txt"), "unfinished task output\n")
	if err := manager.Remove(ctx, fixture.project.RepoPath, first.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("removed worktree stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("second worktree was affected: %v", err)
	}
	assertWorktree(t, fixture.worktreeRoot, second)
	listed := git(t, fixture.project.RepoPath, "worktree", "list", "--porcelain")
	if strings.Contains(listed, first.Path) {
		t.Fatalf("removed worktree remains registered:\n%s", listed)
	}
	if !strings.Contains(listed, "worktree "+second.Path+"\n") {
		t.Fatalf("second worktree is no longer registered:\n%s", listed)
	}
}

func TestManagerRemoveDoesNotOverrideWorktreeLock(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	manager := Manager{Root: fixture.worktreeRoot}
	prepared, err := manager.Prepare(context.Background(), fixture.project, firstTaskID)
	if err != nil {
		t.Fatal(err)
	}
	git(t, fixture.project.RepoPath, "worktree", "lock", prepared.Path)

	err = manager.Remove(context.Background(), fixture.project.RepoPath, prepared.Path)
	if err == nil || !strings.Contains(err.Error(), "remove worktree") {
		t.Fatalf("Remove() error = %v, want preserved Git removal error", err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("locked worktree was removed: %v", err)
	}
	listed := git(t, fixture.project.RepoPath, "worktree", "list", "--porcelain")
	if !strings.Contains(listed, "worktree "+prepared.Path+"\n") {
		t.Fatalf("locked worktree is no longer registered:\n%s", listed)
	}
}

func TestManagerPrepareResolvesSymlinkedRoot(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	realRoot := filepath.Join(t.TempDir(), "real-worktrees")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	prepared, err := (Manager{Root: rootLink}).Prepare(context.Background(), fixture.project, firstTaskID)
	if err != nil {
		t.Fatal(err)
	}
	assertWorktree(t, realRoot, prepared)
}

func TestManagerPrepareRejectsInvalidTaskIDBeforeCreatingRoot(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	root := filepath.Join(t.TempDir(), "not-created")
	_, err := (Manager{Root: root}).Prepare(context.Background(), fixture.project, "../../escape")
	if err == nil {
		t.Fatal("Prepare() error = nil, want invalid task ID error")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("worktree root stat error = %v, want not exist", statErr)
	}
}

func TestManagerPrepareRejectsNonLiteralRemoteRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*config.Project)
	}{
		{
			name: "revision syntax in base branch",
			mutate: func(project *config.Project) {
				project.BaseBranch = "main^{commit}"
			},
		},
		{
			name: "ref path instead of configured remote",
			mutate: func(project *config.Project) {
				project.Remote = "refs/remotes/origin"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newGitFixture(t)
			tt.mutate(&fixture.project)
			if _, err := (Manager{Root: fixture.worktreeRoot}).Prepare(context.Background(), fixture.project, firstTaskID); err == nil {
				t.Fatal("Prepare() error = nil, want invalid remote ref error")
			}
		})
	}
}

func TestManagerPrepareRollsBackAfterCommonDirCancellation(t *testing.T) {
	fixture := newGitFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "common-dir.started")
	once := filepath.Join(binDir, "common-dir.once")
	wrapper := filepath.Join(binDir, "git")
	script := "#!/bin/sh\ncase \"$*\" in\n*'rev-parse --git-common-dir'*)\n" +
		"  if [ ! -e " + strconv.Quote(once) + " ]; then\n" +
		"    : > " + strconv.Quote(once) + "\n" +
		"    : > " + strconv.Quote(marker) + "\n" +
		"    while :; do :; done\n" +
		"  fi\n;;\nesac\nexec " + strconv.Quote(realGit) + " \"$@\"\n"
	writeExecutable(t, wrapper, script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager := Manager{Root: fixture.worktreeRoot}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		prepared Prepared
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		prepared, err := manager.Prepare(ctx, fixture.project, firstTaskID)
		done <- outcome{prepared: prepared, err: err}
	}()
	waitForFile(t, marker)
	cancel()
	got := <-done
	if got.err == nil {
		t.Fatal("Prepare() error = nil, want canceled common-dir error")
	}

	worktree := filepath.Join(fixture.worktreeRoot, firstTaskID)
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rolled-back worktree stat error = %v, want not exist", err)
	}
	listed := git(t, fixture.project.RepoPath, "worktree", "list", "--porcelain")
	if strings.Contains(listed, worktree) {
		t.Errorf("rolled-back worktree remains registered:\n%s", listed)
	}
	branch := "codex/" + firstTaskID
	if got := git(t, fixture.project.RepoPath, "branch", "--list", branch); got != "" {
		t.Errorf("rolled-back branch still exists: %s", got)
	}

	prepared, err := manager.Prepare(context.Background(), fixture.project, firstTaskID)
	if err != nil {
		t.Fatalf("deterministic retry failed: %v", err)
	}
	assertWorktree(t, fixture.worktreeRoot, prepared)
}

func TestManagerValidateCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*testing.T, gitFixture, Prepared)
		wantErr string
	}{
		{
			name: "clean new commit",
			mutate: func(t *testing.T, _ gitFixture, prepared Prepared) {
				commitChange(t, prepared.Path)
			},
		},
		{
			name:    "no new commits",
			wantErr: "no new commits",
		},
		{
			name: "dirty tree",
			mutate: func(t *testing.T, _ gitFixture, prepared Prepared) {
				writeFile(t, filepath.Join(prepared.Path, "dirty.txt"), "dirty\n")
			},
			wantErr: "dirty",
		},
		{
			name: "wrong branch",
			mutate: func(t *testing.T, _ gitFixture, prepared Prepared) {
				commitChange(t, prepared.Path)
				git(t, prepared.Path, "switch", "-c", "wrong-branch")
			},
			wantErr: "wrong branch",
		},
		{
			name: "commit not descended from base",
			mutate: func(t *testing.T, fixture gitFixture, prepared Prepared) {
				git(t, prepared.Path, "reset", "--hard", fixture.rcCommit)
			},
			wantErr: "not descended",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newGitFixture(t)
			manager := Manager{Root: fixture.worktreeRoot}
			prepared, err := manager.Prepare(context.Background(), fixture.project, firstTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.mutate != nil {
				tt.mutate(t, fixture, prepared)
			}

			commit, err := manager.ValidateCommit(context.Background(), prepared)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ValidateCommit() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := git(t, prepared.Path, "rev-parse", "HEAD")
			if commit != want {
				t.Fatalf("ValidateCommit() = %q, want %q", commit, want)
			}
		})
	}
}

func TestManagerValidateCommitComparesHashesCaseInsensitively(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	manager := Manager{Root: fixture.worktreeRoot}
	prepared, err := manager.Prepare(context.Background(), fixture.project, firstTaskID)
	if err != nil {
		t.Fatal(err)
	}
	prepared.BaseCommit = strings.ToUpper(prepared.BaseCommit)

	if _, err := manager.ValidateCommit(context.Background(), prepared); err == nil || !strings.Contains(err.Error(), "no new commits") {
		t.Fatalf("ValidateCommit() error = %v, want no new commits", err)
	}
}

func TestManagerRunChecksDoesNotInvokeShell(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(worktree, "record")
	marker := filepath.Join(worktree, "injected")
	script := filepath.Join(root, "record-argument")
	writeExecutable(t, script, "#!/bin/sh\nprintf '%s' \"$1\" > \"$2\"\n")
	payload := "$(touch " + marker + ")"
	project := config.Project{Checks: [][]string{{script, payload, record}}}

	if err := (Manager{Root: root}).RunChecks(context.Background(), project, worktree); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("recorded argument = %q, want %q", got, payload)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell payload marker stat error = %v, want not exist", err)
	}
}

func TestManagerSubprocessesUseMinimalEnvironment(t *testing.T) {
	t.Run("configured check", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		if err := os.Mkdir(worktree, 0o700); err != nil {
			t.Fatal(err)
		}
		record := filepath.Join(root, "check.env")
		script := filepath.Join(root, "record-environment")
		writeExecutable(t, script, "#!/bin/sh\nenv > \"$1\"\n")
		setSensitiveEnvironment(t)

		project := config.Project{Checks: [][]string{{script, record}}}
		if err := (Manager{Root: root}).RunChecks(context.Background(), project, worktree); err != nil {
			t.Fatal(err)
		}
		assertMinimalEnvironment(t, record)
	})

	t.Run("git", func(t *testing.T) {
		fixture := newGitFixture(t)
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		binDir := t.TempDir()
		record := filepath.Join(binDir, "git.env")
		wrapper := filepath.Join(binDir, "git")
		writeExecutable(t, wrapper, "#!/bin/sh\nenv > "+strconv.Quote(record)+"\nexec "+strconv.Quote(realGit)+" \"$@\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		setSensitiveEnvironment(t)

		if _, err := (Manager{Root: fixture.worktreeRoot}).Prepare(context.Background(), fixture.project, firstTaskID); err != nil {
			t.Errorf("Prepare() was affected by inherited Git environment: %v", err)
		}
		assertMinimalEnvironment(t, record)
	})
}

func TestManagerRunChecksCancellationKillsProcessGroup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(root, "child.pid")
	script := filepath.Join(root, "spawn-child")
	writeExecutable(t, script, "#!/bin/sh\nsleep 60 &\necho $! > \"$1\"\nwait\n")
	t.Cleanup(func() { killProcessFromFile(pidPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		project := config.Project{Checks: [][]string{{script, pidPath}}}
		done <- (Manager{Root: root}).RunChecks(ctx, project, worktree)
	}()

	pid := waitForPIDFile(t, pidPath)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunChecks() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		killProcessFromFile(pidPath)
		<-done
		t.Fatal("RunChecks did not return after cancellation")
	}
	waitForProcessExit(t, pid)
}

func TestManagerRunChecksCancellationBoundsDetachedOutput(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("setsid is unavailable: %v", err)
	}
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(root, "detached.pid")
	fifoPath := filepath.Join(root, "outer.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	script := filepath.Join(root, "retain-output")
	writeExecutable(t, script, "#!/bin/sh\nsetsid sh -c 'sleep 60' &\necho $! > \"$1\"\nread ignored < \"$2\"\n")
	t.Cleanup(func() { killProcessFromFile(pidPath) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		project := config.Project{Checks: [][]string{{script, pidPath, fifoPath}}}
		done <- (Manager{Root: root}).RunChecks(ctx, project, worktree)
	}()

	waitForPIDFile(t, pidPath)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunChecks() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		killProcessFromFile(pidPath)
		<-done
		t.Fatal("RunChecks remained blocked by detached process output")
	}
}

func TestManagerRunChecksBoundsFailureOutput(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	project := config.Project{Checks: [][]string{{os.Args[0], "-test.run=^TestGitworkCheckHelperProcess$", "--", "large-output"}}}

	err := (Manager{Root: root}).RunChecks(context.Background(), project, worktree)
	if err == nil {
		t.Fatal("RunChecks() error = nil, want failed check error")
	}
	message := err.Error()
	if !strings.Contains(message, "[check output truncated after 1048576 bytes]") {
		t.Fatalf("RunChecks() error lacks truncation marker: length=%d", len(message))
	}
	if len(message) > (1<<20)+512 {
		t.Fatalf("RunChecks() error length = %d, want bounded output", len(message))
	}
	if !strings.Contains(message, strings.Repeat("x", 64)) {
		t.Fatal("RunChecks() error did not retain initial output")
	}
}

func TestGitworkCheckHelperProcess(t *testing.T) {
	if os.Args[len(os.Args)-1] != "large-output" {
		return
	}
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	for range 96 {
		_, _ = os.Stdout.Write(chunk)
	}
	_, _ = os.Stderr.WriteString("TAIL\n")
	os.Exit(7)
}

func TestManagerRejectsWorktreeOutsideRoot(t *testing.T) {
	t.Parallel()

	fixture := newGitFixture(t)
	manager := Manager{Root: fixture.worktreeRoot}
	outside := t.TempDir()

	if err := manager.RunChecks(context.Background(), fixture.project, outside); err == nil {
		t.Fatal("RunChecks() error = nil, want containment error")
	}
	if err := manager.Remove(context.Background(), fixture.project.RepoPath, outside); err == nil {
		t.Fatal("Remove() error = nil, want containment error")
	}
}

type gitFixture struct {
	project      config.Project
	worktreeRoot string
	mainCommit   string
	rcCommit     string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "-b", "main", seed)
	git(t, seed, "config", "user.name", "QQ Codex Test")
	git(t, seed, "config", "user.email", "qqcodex@example.invalid")
	writeFile(t, filepath.Join(seed, "base.txt"), "main\n")
	git(t, seed, "add", "base.txt")
	git(t, seed, "commit", "-m", "main base")
	mainCommit := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "-u", "origin", "main")

	git(t, seed, "switch", "--orphan", "rc")
	if err := os.Remove(filepath.Join(seed, "base.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(seed, "rc.txt"), "rc\n")
	git(t, seed, "add", "rc.txt")
	git(t, seed, "commit", "-m", "unrelated rc base")
	rcCommit := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "push", "-u", "origin", "rc")
	git(t, seed, "switch", "main")

	return gitFixture{
		project: config.Project{
			ID:         "order-api",
			RepoPath:   seed,
			BaseBranch: "main",
			Remote:     "origin",
			Checks:     [][]string{{"git", "status", "--short"}},
		},
		worktreeRoot: worktreeRoot,
		mainCommit:   mainCommit,
		rcCommit:     rcCommit,
	}
}

func assertWorktree(t *testing.T, root string, prepared Prepared) {
	t.Helper()

	if !filepath.IsAbs(prepared.Path) {
		t.Fatalf("worktree path %q is not absolute", prepared.Path)
	}
	if !filepath.IsAbs(prepared.GitCommonDir) {
		t.Fatalf("Git common dir %q is not absolute", prepared.GitCommonDir)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPath, err := filepath.EvalSymlinks(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("worktree %q is outside root %q", resolvedPath, resolvedRoot)
	}
	if got := git(t, prepared.Path, "branch", "--show-current"); got != prepared.Branch {
		t.Fatalf("current branch = %q, want %q", got, prepared.Branch)
	}
}

func commitChange(t *testing.T, worktree string) {
	t.Helper()

	writeFile(t, filepath.Join(worktree, "change.txt"), "change\n")
	git(t, worktree, "add", "change.txt")
	git(t, worktree, "commit", "-m", "task change")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func setSensitiveEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"CODEX_API_KEY":       "codex-secret",
		"NAPCAT_ACCESS_TOKEN": "napcat-secret",
		"QQCODEX_TEST_SECRET": "arbitrary-secret",
		"GIT_DIR":             filepath.Join(t.TempDir(), "wrong.git"),
		"GIT_WORK_TREE":       t.TempDir(),
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "core.bare",
		"GIT_CONFIG_VALUE_0":  "true",
		"GIT_SSH_COMMAND":     "false",
	} {
		t.Setenv(name, value)
	}
}

func assertMinimalEnvironment(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	environment := string(data)
	for _, name := range []string{
		"CODEX_API_KEY", "NAPCAT_ACCESS_TOKEN", "QQCODEX_TEST_SECRET",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0", "GIT_SSH_COMMAND",
	} {
		if strings.Contains(environment, name+"=") {
			t.Errorf("subprocess environment contains %s", name)
		}
	}
	if !strings.Contains(environment, "PATH="+os.Getenv("PATH")+"\n") {
		t.Errorf("subprocess environment did not preserve PATH:\n%s", environment)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %q was not written", path)
	return 0
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q was not created", path)
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("process %d survived cancellation", pid)
	}
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
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
