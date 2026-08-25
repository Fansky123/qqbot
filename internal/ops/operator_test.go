package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testTaskID = "T-012345ABCDEF"

func TestLoadConfigValidatesAndCanonicalizesProjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mustMkdir(t, repo)
	repoLink := filepath.Join(root, "repo-link")
	if err := os.Symlink(repo, repoLink); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	configPath := filepath.Join(root, "ops.json")
	writeJSON(t, configPath, map[string]any{
		"projects": map[string]any{
			"order-api": map[string]any{
				"repo_path":     repoLink,
				"remote":        "origin",
				"base_branch":   "main",
				"rc_branch":     "rc",
				"checks":        [][]string{{"go", "test", "./..."}},
				"deploy_action": []string{"/usr/local/libexec/deploy-order-rc"},
			},
		},
	})

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Projects["order-api"].RepoPath; got != repo {
		t.Fatalf("canonical repo path = %q, want %q", got, repo)
	}
}

func TestLoadConfigRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "relative repository",
			mutate: func(project map[string]any) {
				project["repo_path"] = "relative/repo"
			},
		},
		{
			name: "revision syntax in branch",
			mutate: func(project map[string]any) {
				project["base_branch"] = "main^{commit}"
			},
		},
		{
			name: "option-like remote",
			mutate: func(project map[string]any) {
				project["remote"] = "--upload-pack=bad"
			},
		},
		{
			name: "empty check argument",
			mutate: func(project map[string]any) {
				project["checks"] = [][]string{{"go", ""}}
			},
		},
		{
			name: "missing deploy action",
			mutate: func(project map[string]any) {
				project["deploy_action"] = []string{}
			},
		},
		{
			name: "secret field",
			mutate: func(project map[string]any) {
				project["credential"] = "secret"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			project := map[string]any{
				"repo_path":     repo,
				"remote":        "origin",
				"base_branch":   "main",
				"rc_branch":     "rc",
				"checks":        [][]string{{"go", "test", "./..."}},
				"deploy_action": []string{"deploy-rc"},
			}
			tt.mutate(project)
			path := filepath.Join(t.TempDir(), "ops.json")
			writeJSON(t, path, map[string]any{"projects": map[string]any{"order-api": project}})
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("LoadConfig() error = nil, want validation error")
			}
		})
	}
}

func TestOperatorSyncFetchesBaseAndRC(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	clone := fixture.clone(t)
	writeFile(t, filepath.Join(clone, "main-next.txt"), "main next\n")
	git(t, clone, "add", "main-next.txt")
	git(t, clone, "commit", "-m", "main next")
	wantMain := git(t, clone, "rev-parse", "HEAD")
	git(t, clone, "push", "origin", "HEAD:main")
	git(t, clone, "switch", "rc")
	writeFile(t, filepath.Join(clone, "rc-next.txt"), "rc next\n")
	git(t, clone, "add", "rc-next.txt")
	git(t, clone, "commit", "-m", "rc next")
	wantRC := git(t, clone, "rev-parse", "HEAD")
	git(t, clone, "push", "origin", "HEAD:rc")

	if err := fixture.operator.Sync(context.Background(), fixture.projectID); err != nil {
		t.Fatal(err)
	}
	if got := git(t, fixture.repo, "rev-parse", "refs/remotes/origin/main"); got != wantMain {
		t.Fatalf("fetched main = %q, want %q", got, wantMain)
	}
	if got := git(t, fixture.repo, "rev-parse", "refs/remotes/origin/rc"); got != wantRC {
		t.Fatalf("fetched RC = %q, want %q", got, wantRC)
	}
}

func TestOperatorPushTaskAllowsOnlyExactFastForwardTaskRef(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	branch, first := fixture.createTaskCommit(t, testTaskID, "first.txt", "first\n")
	ctx := context.Background()

	if err := fixture.operator.PushTask(ctx, fixture.projectID, testTaskID, branch, first); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got := fixture.remoteRef(t, branch); got != first {
		t.Fatalf("remote task commit = %q, want %q", got, first)
	}

	git(t, fixture.repo, "switch", branch)
	writeFile(t, filepath.Join(fixture.repo, "second.txt"), "second\n")
	git(t, fixture.repo, "add", "second.txt")
	git(t, fixture.repo, "commit", "-m", "task supplement")
	second := git(t, fixture.repo, "rev-parse", "HEAD")
	if err := fixture.operator.PushTask(ctx, fixture.projectID, testTaskID, branch, second); err != nil {
		t.Fatalf("supplement push: %v", err)
	}
	if got := fixture.remoteRef(t, branch); got != second {
		t.Fatalf("remote supplement commit = %q, want %q", got, second)
	}

	if err := fixture.operator.PushTask(ctx, fixture.projectID, testTaskID, "main", second); err == nil {
		t.Fatal("PushTask() accepted a non-task branch")
	}
	if err := fixture.operator.PushTask(ctx, fixture.projectID, testTaskID, branch, first); err == nil {
		t.Fatal("PushTask() accepted a commit that does not match the local branch")
	}

	other := fixture.clone(t)
	git(t, other, "switch", "-c", branch, "origin/"+branch)
	writeFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	git(t, other, "add", "remote.txt")
	git(t, other, "commit", "-m", "remote task change")
	remoteCommit := git(t, other, "rev-parse", "HEAD")
	git(t, other, "push", "origin", "HEAD:"+branch)

	writeFile(t, filepath.Join(fixture.repo, "local.txt"), "local\n")
	git(t, fixture.repo, "add", "local.txt")
	git(t, fixture.repo, "commit", "-m", "divergent local task change")
	localCommit := git(t, fixture.repo, "rev-parse", "HEAD")
	if err := fixture.operator.PushTask(ctx, fixture.projectID, testTaskID, branch, localCommit); err == nil {
		t.Fatal("PushTask() accepted a non-fast-forward update")
	}
	if got := fixture.remoteRef(t, branch); got != remoteCommit {
		t.Fatalf("rejected push changed remote to %q, want %q", got, remoteCommit)
	}
	assertNoTemporaryRefs(t, fixture.repo)
}

func TestOperatorMergeRCVerifiesCommitRunsChecksAndSkipsHooks(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	branch, taskCommit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
	if err := fixture.operator.PushTask(context.Background(), fixture.projectID, testTaskID, branch, taskCommit); err != nil {
		t.Fatal(err)
	}
	checkMarker := filepath.Join(t.TempDir(), "check-ran")
	check := writeExecutable(t, filepath.Join(t.TempDir(), "check"), "#!/bin/sh\nprintf '%s' \"$PWD\" > \"$1\"\n")
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(fixture.repo, ".git", "hooks")
	writeExecutable(t, filepath.Join(hooks, "post-merge"), "#!/bin/sh\ntouch \""+hookMarker+"\"\n")
	project := fixture.config.Projects[fixture.projectID]
	project.Checks = [][]string{{check, checkMarker}}
	fixture.config.Projects[fixture.projectID] = project
	fixture.operator = NewOperator(fixture.config)
	oldRC := fixture.remoteRef(t, "rc")

	merged, err := fixture.operator.MergeRC(context.Background(), fixture.projectID, testTaskID, taskCommit)
	if err != nil {
		t.Fatal(err)
	}
	if merged == oldRC || merged != fixture.remoteRef(t, "rc") {
		t.Fatalf("merged RC = %q, old %q, remote %q", merged, oldRC, fixture.remoteRef(t, "rc"))
	}
	if got := git(t, fixture.repo, "rev-list", "--parents", "-n", "1", merged); len(strings.Fields(got)) != 3 {
		t.Fatalf("merge commit parents = %q, want two parents", got)
	}
	worktree, err := os.ReadFile(checkMarker)
	if err != nil {
		t.Fatalf("check did not run: %v", err)
	}
	if strings.TrimSpace(string(worktree)) == fixture.repo {
		t.Fatal("checks ran in the main repository instead of a temporary worktree")
	}
	if _, err := os.Stat(strings.TrimSpace(string(worktree))); !os.IsNotExist(err) {
		t.Fatalf("temporary worktree remains, stat error = %v", err)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("repository hook ran, stat error = %v", err)
	}
	assertNoTemporaryRefs(t, fixture.repo)
}

func TestOperatorMergeRCFailureLeavesRemoteUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, *opsFixture) string
	}{
		{
			name: "approved commit differs from remote task",
			prepare: func(t *testing.T, fixture *opsFixture) string {
				branch, commit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
				if err := fixture.operator.PushTask(context.Background(), fixture.projectID, testTaskID, branch, commit); err != nil {
					t.Fatal(err)
				}
				return fixture.mainCommit
			},
		},
		{
			name: "check failure",
			prepare: func(t *testing.T, fixture *opsFixture) string {
				branch, commit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
				if err := fixture.operator.PushTask(context.Background(), fixture.projectID, testTaskID, branch, commit); err != nil {
					t.Fatal(err)
				}
				project := fixture.config.Projects[fixture.projectID]
				project.Checks = [][]string{{writeExecutable(t, filepath.Join(t.TempDir(), "fail"), "#!/bin/sh\nexit 17\n")}}
				fixture.config.Projects[fixture.projectID] = project
				fixture.operator = NewOperator(fixture.config)
				return commit
			},
		},
		{
			name: "merge conflict",
			prepare: func(t *testing.T, fixture *opsFixture) string {
				git(t, fixture.repo, "switch", "-c", "codex/"+testTaskID, "origin/main")
				writeFile(t, filepath.Join(fixture.repo, "shared.txt"), "task\n")
				git(t, fixture.repo, "add", "shared.txt")
				git(t, fixture.repo, "commit", "-m", "task conflict")
				commit := git(t, fixture.repo, "rev-parse", "HEAD")
				if err := fixture.operator.PushTask(context.Background(), fixture.projectID, testTaskID, "codex/"+testTaskID, commit); err != nil {
					t.Fatal(err)
				}
				other := fixture.clone(t)
				git(t, other, "switch", "rc")
				writeFile(t, filepath.Join(other, "shared.txt"), "rc\n")
				git(t, other, "add", "shared.txt")
				git(t, other, "commit", "-m", "rc conflict")
				git(t, other, "push", "origin", "HEAD:rc")
				return commit
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newOpsFixture(t)
			approved := tt.prepare(t, &fixture)
			oldRC := fixture.remoteRef(t, "rc")
			if _, err := fixture.operator.MergeRC(context.Background(), fixture.projectID, testTaskID, approved); err == nil {
				t.Fatal("MergeRC() error = nil, want failure")
			}
			if got := fixture.remoteRef(t, "rc"); got != oldRC {
				t.Fatalf("failed merge changed remote RC to %q, want %q", got, oldRC)
			}
			assertNoTemporaryRefs(t, fixture.repo)
			if listed := git(t, fixture.repo, "worktree", "list", "--porcelain"); strings.Contains(listed, "qqcodex-rc-") {
				t.Fatalf("temporary worktree remains registered:\n%s", listed)
			}
		})
	}
}

func TestOperatorRejectsUnsafeLocalGitCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key, value string
	}{
		{"remote.origin.pushurl", "ext::sh -c touch% /tmp/not-allowed"},
		{"core.gitproxy", "sh -c touch /tmp/not-allowed"},
		{"core.alternaterefscommand", "sh -c touch /tmp/not-allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			fixture := newOpsFixture(t)
			git(t, fixture.repo, "config", "--local", tt.key, tt.value)
			if err := fixture.operator.Sync(context.Background(), fixture.projectID); err == nil {
				t.Fatalf("Sync() accepted unsafe local Git config %q", tt.key)
			}
		})
	}
}

func TestRunProcessBoundsOutput(t *testing.T) {
	t.Parallel()

	if _, err := runProcess(context.Background(), t.TempDir(), []string{"head", "-c", "1048577", "/dev/zero"}, checkEnvironment()); err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("runProcess() error = %v, want bounded-output error", err)
	}
}

func TestRunProcessCancellationKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-pid")
	helper := writeExecutable(t, filepath.Join(t.TempDir(), "block"), "#!/bin/sh\nsleep 600 &\necho $! > \"$1\"\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runProcess(ctx, t.TempDir(), []string{helper, marker}, checkEnvironment())
		done <- err
	}()
	waitForFile(t, marker)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runProcess() error = %v, want context cancellation", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d remains after cancellation: %v", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOperatorDeployRCUsesOnlyConfiguredAction(t *testing.T) {
	fixture := newOpsFixture(t)
	record := filepath.Join(t.TempDir(), "deploy.json")
	helper := writeExecutable(t, filepath.Join(t.TempDir(), "deploy"), `#!/bin/sh
printf '{"project":"%s","task":"%s","commit":"%s","arg":"%s"}' \
  "$QQCODEX_PROJECT_ID" "$QQCODEX_TASK_ID" "$QQCODEX_RC_COMMIT" "$1" > "$2"
`)
	project := fixture.config.Projects[fixture.projectID]
	project.DeployAction = []string{helper, "fixed", record}
	fixture.config.Projects[fixture.projectID] = project
	fixture.operator = NewOperator(fixture.config)
	t.Setenv("QQCODEX_PROJECT_ID", "attacker-project")
	t.Setenv("QQCODEX_TASK_ID", "attacker-task")
	t.Setenv("QQCODEX_RC_COMMIT", "attacker-commit")

	if err := fixture.operator.DeployRC(context.Background(), fixture.projectID, testTaskID, fixture.rcCommit); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"project": fixture.projectID,
		"task":    testTaskID,
		"commit":  fixture.rcCommit,
		"arg":     "fixed",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("deploy %s = %q, want %q", key, got[key], value)
		}
	}

	if err := fixture.operator.DeployRC(context.Background(), fixture.projectID, testTaskID+";touch /tmp/bad", fixture.rcCommit); err == nil {
		t.Fatal("DeployRC() accepted an invalid task ID")
	}
}

type opsFixture struct {
	projectID  string
	remote     string
	repo       string
	mainCommit string
	rcCommit   string
	config     Config
	operator   *Operator
}

func newOpsFixture(t *testing.T) opsFixture {
	t.Helper()

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	repo := filepath.Join(root, "operator-repo")
	git(t, root, "init", "--bare", remote)
	git(t, root, "init", "-b", "main", seed)
	configureGitUser(t, seed)
	writeFile(t, filepath.Join(seed, "base.txt"), "base\n")
	writeFile(t, filepath.Join(seed, "shared.txt"), "base\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "main base")
	mainCommit := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "-u", "origin", "main")
	git(t, seed, "switch", "-c", "rc")
	writeFile(t, filepath.Join(seed, "rc.txt"), "rc\n")
	git(t, seed, "add", "rc.txt")
	git(t, seed, "commit", "-m", "rc base")
	rcCommit := git(t, seed, "rev-parse", "HEAD")
	git(t, seed, "push", "-u", "origin", "rc")
	git(t, root, "clone", remote, repo)
	configureGitUser(t, repo)
	git(t, repo, "fetch", "origin", "main", "rc")
	git(t, repo, "switch", "-c", "main", "origin/main")

	projectID := "order-api"
	cfg := Config{Projects: map[string]Project{
		projectID: {
			RepoPath:     repo,
			Remote:       "origin",
			BaseBranch:   "main",
			RCBranch:     "rc",
			Checks:       [][]string{{"git", "diff", "--check", "HEAD^", "HEAD"}},
			DeployAction: []string{"true"},
		},
	}}
	return opsFixture{
		projectID:  projectID,
		remote:     remote,
		repo:       repo,
		mainCommit: mainCommit,
		rcCommit:   rcCommit,
		config:     cfg,
		operator:   NewOperator(cfg),
	}
}

func (f opsFixture) clone(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clone")
	git(t, filepath.Dir(path), "clone", f.remote, path)
	configureGitUser(t, path)
	git(t, path, "fetch", "origin", "main", "rc")
	git(t, path, "switch", "-c", "main", "origin/main")
	return path
}

func (f opsFixture) createTaskCommit(t *testing.T, taskID, name, content string) (string, string) {
	t.Helper()
	branch := "codex/" + taskID
	git(t, f.repo, "switch", "-c", branch, "origin/main")
	writeFile(t, filepath.Join(f.repo, name), content)
	git(t, f.repo, "add", name)
	git(t, f.repo, "commit", "-m", "task change")
	return branch, git(t, f.repo, "rev-parse", "HEAD")
}

func (f opsFixture) remoteRef(t *testing.T, branch string) string {
	t.Helper()
	output := git(t, f.repo, "ls-remote", "--refs", f.remote, "refs/heads/"+branch)
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("remote ref %q output = %q", branch, output)
	}
	return fields[0]
}

func configureGitUser(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "config", "user.name", "QQ Codex Test")
	git(t, repo, "config", "user.email", "qqcodex@example.invalid")
}

func assertNoTemporaryRefs(t *testing.T, repo string) {
	t.Helper()
	if refs := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/qqcodex"); refs != "" {
		t.Fatalf("temporary refs remain:\n%s", refs)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}
