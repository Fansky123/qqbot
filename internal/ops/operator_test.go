package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

	root := privateTempDir(t)
	repo := filepath.Join(root, "repo")
	mustMkdir(t, repo)
	remote := filepath.Join(root, "remote.git")
	mustMkdir(t, remote)
	runner := writeExecutable(t, filepath.Join(root, "check-runner"), "#!/bin/sh\nexit 0\n")
	runnerLink := filepath.Join(root, "check-runner-link")
	if err := os.Symlink(runner, runnerLink); err != nil {
		t.Skipf("cannot create executable symlink: %v", err)
	}
	deploy := writeExecutable(t, filepath.Join(root, "deploy"), "#!/bin/sh\nexit 0\n")
	repoLink := filepath.Join(root, "repo-link")
	if err := os.Symlink(repo, repoLink); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	configPath := filepath.Join(root, "ops.json")
	writeJSON(t, configPath, map[string]any{
		"git_binary": realGit(t),
		"projects": map[string]any{
			"order-api": map[string]any{
				"repo_path":          repoLink,
				"remote":             "origin",
				"remote_url":         remote,
				"allow_local_remote": true,
				"base_branch":        "main",
				"rc_branch":          "rc",
				"check_runner":       []string{runnerLink, "fixed-runner-arg"},
				"checks":             [][]string{{"go", "test", "./..."}},
				"deploy_action":      []string{deploy},
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
	if got := cfg.Projects["order-api"].CheckRunner[0]; got != runner {
		t.Fatalf("canonical check runner = %q, want %q", got, runner)
	}
}

func TestLoadConfigRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	root := privateTempDir(t)
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	mustMkdir(t, repo)
	mustMkdir(t, remote)
	runner := writeExecutable(t, filepath.Join(root, "check-runner"), "#!/bin/sh\nexit 0\n")
	deploy := writeExecutable(t, filepath.Join(root, "deploy"), "#!/bin/sh\nexit 0\n")
	tests := []struct {
		name   string
		mutate func(*testing.T, map[string]any, map[string]any)
	}{
		{
			name: "relative repository",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["repo_path"] = "relative/repo"
			},
		},
		{
			name: "revision syntax in branch",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["base_branch"] = "main^{commit}"
			},
		},
		{
			name: "option-like remote",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["remote"] = "--upload-pack=bad"
			},
		},
		{
			name: "empty check argument",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["checks"] = [][]string{{"go", ""}}
			},
		},
		{
			name: "missing deploy action",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["deploy_action"] = []string{}
			},
		},
		{
			name: "secret field",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["credential"] = "secret"
			},
		},
		{
			name: "relative Git binary",
			mutate: func(_ *testing.T, config map[string]any, _ map[string]any) {
				config["git_binary"] = "git"
			},
		},
		{
			name: "local remote without fake mode",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["allow_local_remote"] = false
			},
		},
		{
			name: "scp-like remote",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["allow_local_remote"] = false
				project["remote_url"] = "git@example.com:company/order-api.git"
			},
		},
		{
			name: "Git protocol remote",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["allow_local_remote"] = false
				project["remote_url"] = "git://example.com/company/order-api.git"
			},
		},
		{
			name: "credential in HTTPS remote",
			mutate: func(_ *testing.T, _ map[string]any, project map[string]any) {
				project["allow_local_remote"] = false
				project["remote_url"] = "https://user:secret@example.com/company/order-api.git"
			},
		},
		{
			name: "check runner inside repository",
			mutate: func(t *testing.T, _ map[string]any, project map[string]any) {
				inside := writeExecutable(t, filepath.Join(repo, "runner"), "#!/bin/sh\nexit 0\n")
				project["check_runner"] = []string{inside}
			},
		},
		{
			name: "replaceable deploy parent",
			mutate: func(t *testing.T, _ map[string]any, project map[string]any) {
				parent := filepath.Join(root, "replaceable")
				mustMkdir(t, parent)
				if err := os.Chmod(parent, 0o777); err != nil {
					t.Fatal(err)
				}
				project["deploy_action"] = []string{writeExecutable(t, filepath.Join(parent, "deploy"), "#!/bin/sh\nexit 0\n")}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			project := map[string]any{
				"repo_path":          repo,
				"remote":             "origin",
				"remote_url":         remote,
				"allow_local_remote": true,
				"base_branch":        "main",
				"rc_branch":          "rc",
				"check_runner":       []string{runner},
				"checks":             [][]string{{"go", "test", "./..."}},
				"deploy_action":      []string{deploy},
			}
			config := map[string]any{"git_binary": realGit(t), "projects": map[string]any{"order-api": project}}
			tt.mutate(t, config, project)
			path := filepath.Join(t.TempDir(), "ops.json")
			writeJSON(t, path, config)
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("LoadConfig() error = nil, want validation error")
			}
		})
	}
}

func TestNewOperatorRevalidatesConfiguration(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	project := fixture.config.Projects[fixture.projectID]
	project.DeployAction[0] = "relative-deploy"
	fixture.config.Projects[fixture.projectID] = project
	if _, err := NewOperator(fixture.config); err == nil {
		t.Fatal("NewOperator() error = nil, want executable validation error")
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

func TestOperatorPushTaskBundleFromIsolatedWorkerClone(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	worker := fixture.clone(t)
	branch := "codex/" + testTaskID
	git(t, worker, "switch", "-c", branch, "origin/main")
	writeFile(t, filepath.Join(worker, "first.txt"), "first\n")
	git(t, worker, "add", "first.txt")
	git(t, worker, "commit", "-m", "first")
	first := git(t, worker, "rev-parse", "HEAD")

	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, worker, testTaskID, branch, first, nil); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got := fixture.remoteRef(t, branch); got != first {
		t.Fatalf("remote task commit = %q, want %q", got, first)
	}
	if local := git(t, fixture.repo, "for-each-ref", "--format=%(refname)", "refs/heads/codex"); local != "" {
		t.Fatalf("ops clone acquired worker task ref %q", local)
	}
	if workerGit, opsGit := git(t, worker, "rev-parse", "--path-format=absolute", "--git-common-dir"), git(t, fixture.repo, "rev-parse", "--path-format=absolute", "--git-common-dir"); workerGit == opsGit {
		t.Fatalf("worker and ops clone share Git metadata %q", workerGit)
	}

	writeFile(t, filepath.Join(worker, "second.txt"), "second\n")
	git(t, worker, "add", "second.txt")
	git(t, worker, "commit", "-m", "second")
	second := git(t, worker, "rev-parse", "HEAD")
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, worker, testTaskID, branch, second, nil); err != nil {
		t.Fatalf("supplement push: %v", err)
	}
	if got := fixture.remoteRef(t, branch); got != second {
		t.Fatalf("remote supplement commit = %q, want %q", got, second)
	}

	other := fixture.clone(t)
	git(t, other, "switch", "-c", branch, "origin/"+branch)
	writeFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	git(t, other, "add", "remote.txt")
	git(t, other, "commit", "-m", "remote")
	remoteCommit := git(t, other, "rev-parse", "HEAD")
	git(t, other, "push", "origin", "HEAD:"+branch)

	writeFile(t, filepath.Join(worker, "local.txt"), "local\n")
	git(t, worker, "add", "local.txt")
	git(t, worker, "commit", "-m", "divergent local")
	localCommit := git(t, worker, "rev-parse", "HEAD")
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, worker, testTaskID, branch, localCommit, nil); err == nil {
		t.Fatal("PushTaskBundle() accepted a non-fast-forward update")
	}
	if got := fixture.remoteRef(t, branch); got != remoteCommit {
		t.Fatalf("rejected push changed remote to %q, want %q", got, remoteCommit)
	}
}

func TestOperatorPushTaskBundleRejectsUnexpectedOrCorruptBundle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, string, string, string) io.Reader
	}{
		{
			name: "extra advertised ref",
			prepare: func(t *testing.T, worker, branch, _ string) io.Reader {
				git(t, worker, "branch", "extra", branch)
				return openBundle(t, worker, "refs/heads/"+branch, "refs/heads/extra")
			},
		},
		{
			name: "wrong advertised ref",
			prepare: func(t *testing.T, worker, branch, _ string) io.Reader {
				git(t, worker, "branch", "wrong", branch)
				return openBundle(t, worker, "refs/heads/wrong")
			},
		},
		{
			name: "appended junk",
			prepare: func(t *testing.T, worker, branch, _ string) io.Reader {
				bundle := openBundle(t, worker, "refs/heads/"+branch)
				data, err := io.ReadAll(bundle)
				if err != nil {
					t.Fatal(err)
				}
				return bytes.NewReader(append(data, []byte("trailing-junk")...))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newOpsFixture(t)
			worker := fixture.clone(t)
			branch := "codex/" + testTaskID
			git(t, worker, "switch", "-c", branch, "origin/main")
			writeFile(t, filepath.Join(worker, "task.txt"), "task\n")
			git(t, worker, "add", "task.txt")
			git(t, worker, "commit", "-m", "task")
			commit := git(t, worker, "rev-parse", "HEAD")

			err := fixture.operator.PushTaskBundle(context.Background(), fixture.projectID, testTaskID, branch, commit, tt.prepare(t, worker, branch, commit))
			if err == nil {
				t.Fatal("PushTaskBundle() error = nil, want bundle rejection")
			}
			if got := remoteRefOrEmpty(t, fixture.remote, branch); got != "" {
				t.Fatalf("rejected bundle pushed remote task %q", got)
			}
		})
	}
}

func TestOperatorPushTaskBundleRequiresTrustedBaseAncestry(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	worker := fixture.clone(t)
	branch := "codex/" + testTaskID
	git(t, worker, "switch", "--orphan", branch)
	git(t, worker, "rm", "-rf", "--ignore-unmatch", ".")
	writeFile(t, filepath.Join(worker, "orphan.txt"), "orphan\n")
	git(t, worker, "add", "orphan.txt")
	git(t, worker, "commit", "-m", "orphan")
	commit := git(t, worker, "rev-parse", "HEAD")

	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, worker, testTaskID, branch, commit, nil); err == nil {
		t.Fatal("PushTaskBundle() accepted commit outside trusted base history")
	}
	if got := remoteRefOrEmpty(t, fixture.remote, branch); got != "" {
		t.Fatalf("base ancestry rejection pushed remote task %q", got)
	}
}

func TestOperatorPushTaskBundleCleanupFailurePreventsRemotePush(t *testing.T) {
	fixture := newOpsFixture(t)
	worker := fixture.clone(t)
	branch := "codex/" + testTaskID
	git(t, worker, "switch", "-c", branch, "origin/main")
	writeFile(t, filepath.Join(worker, "task.txt"), "task\n")
	git(t, worker, "add", "task.txt")
	git(t, worker, "commit", "-m", "task")
	commit := git(t, worker, "rev-parse", "HEAD")
	real := realGit(t)
	wrapper := writeExecutable(t, filepath.Join(privateTempDir(t), "cleanup-failing-git"), `#!/bin/sh
case " $* " in
  *" update-ref -d refs/qqcodex/"*) exit 88 ;;
esac
exec "`+real+`" "$@"
`)
	fixture.config.GitBinary = wrapper
	fixture.operator = mustNewOperator(t, fixture.config)

	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, worker, testTaskID, branch, commit, nil); err == nil {
		t.Fatal("PushTaskBundle() error = nil, want temporary ref cleanup failure")
	}
	if got := remoteRefOrEmpty(t, fixture.remote, branch); got != "" {
		t.Fatalf("cleanup failure pushed remote task %q", got)
	}
}

func TestSpoolTaskBundleRejectsEmptyAndOverflow(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "overflow", data: bytes.Repeat([]byte("x"), 1025)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := spoolTaskBundle(bytes.NewReader(tt.data), 1024); err == nil {
				t.Fatal("spoolTaskBundle() error = nil, want rejection")
			}
		})
	}
	path, cleanup, err := spoolTaskBundle(bytes.NewReader([]byte("bundle")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spooled bundle mode = %o, want 600", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("spool directory mode = %o, want 700", rootInfo.Mode().Perm())
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spooled bundle remains after cleanup, stat error = %v", err)
	}
}

func TestOperatorPushTaskBundleAllowsOnlyExactFastForwardTaskRef(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	branch, first := fixture.createTaskCommit(t, testTaskID, "first.txt", "first\n")
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, first, nil); err != nil {
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
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, second, nil); err != nil {
		t.Fatalf("supplement push: %v", err)
	}
	if got := fixture.remoteRef(t, branch); got != second {
		t.Fatalf("remote supplement commit = %q, want %q", got, second)
	}

	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, "main", second, nil); err == nil {
		t.Fatal("PushTaskBundle() accepted a non-task branch")
	}
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, first, nil); err == nil {
		t.Fatal("PushTaskBundle() accepted a bundle that does not match the claim")
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
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, localCommit, nil); err == nil {
		t.Fatal("PushTaskBundle() accepted a non-fast-forward update")
	}
	if got := fixture.remoteRef(t, branch); got != remoteCommit {
		t.Fatalf("rejected push changed remote to %q, want %q", got, remoteCommit)
	}
	assertNoTemporaryRefs(t, fixture.repo)
}

func TestOperatorMergeRCVerifiesCommitRunsChecksAndSkipsHooks(t *testing.T) {
	fixture := newOpsFixture(t)
	branch, taskCommit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, taskCommit, nil); err != nil {
		t.Fatal(err)
	}
	checkMarker := filepath.Join(t.TempDir(), "check-ran")
	check := writeExecutable(t, filepath.Join(t.TempDir(), "check"), "#!/bin/sh\n[ \"$PATH\" = /usr/bin:/bin ] || exit 65\n[ -z \"${CUSTOM_TOKEN-}\" ] || exit 66\n[ \"$HOME\" != /attacker/home ] || exit 67\nprintf '%s' \"$PWD\" > \"$1\"\n")
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(fixture.repo, ".git", "hooks")
	writeExecutable(t, filepath.Join(hooks, "post-merge"), "#!/bin/sh\ntouch \""+hookMarker+"\"\n")
	project := fixture.config.Projects[fixture.projectID]
	project.Checks = [][]string{{check, checkMarker}}
	fixture.config.Projects[fixture.projectID] = project
	fixture.operator = mustNewOperator(t, fixture.config)
	t.Setenv("CUSTOM_TOKEN", "must-not-cross-check-boundary")
	t.Setenv("HOME", "/attacker/home")
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
				if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, commit, nil); err != nil {
					t.Fatal(err)
				}
				return fixture.mainCommit
			},
		},
		{
			name: "check failure",
			prepare: func(t *testing.T, fixture *opsFixture) string {
				branch, commit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
				if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, commit, nil); err != nil {
					t.Fatal(err)
				}
				project := fixture.config.Projects[fixture.projectID]
				project.Checks = [][]string{{writeExecutable(t, filepath.Join(t.TempDir(), "fail"), "#!/bin/sh\nexit 17\n")}}
				fixture.config.Projects[fixture.projectID] = project
				fixture.operator = mustNewOperator(t, fixture.config)
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
				if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, "codex/"+testTaskID, commit, nil); err != nil {
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

func TestOperatorMergeRCRejectsCheckRepositoryChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		check func(*testing.T, opsFixture) []string
	}{
		{
			name: "dirty worktree",
			check: func(t *testing.T, _ opsFixture) []string {
				return []string{writeExecutable(t, filepath.Join(privateTempDir(t), "dirty"), "#!/bin/sh\nprintf dirty > check-dirty.txt\n")}
			},
		},
		{
			name: "new commit",
			check: func(t *testing.T, _ opsFixture) []string {
				return []string{writeExecutable(t, filepath.Join(privateTempDir(t), "commit"), "#!/bin/sh\nprintf checked > check-commit.txt\n\""+realGit(t)+"\" add check-commit.txt\n\""+realGit(t)+"\" commit -m check-commit\n")}
			},
		},
		{
			name: "local Git configuration",
			check: func(t *testing.T, _ opsFixture) []string {
				alternate := filepath.Join(privateTempDir(t), "alternate.git")
				git(t, filepath.Dir(alternate), "init", "--bare", alternate)
				return []string{writeExecutable(t, filepath.Join(privateTempDir(t), "config"), "#!/bin/sh\n\""+realGit(t)+"\" config remote.origin.pushurl \""+alternate+"\"\n")}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newOpsFixture(t)
			branch, taskCommit := fixture.createTaskCommit(t, testTaskID, "feature.txt", "feature\n")
			if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, taskCommit, nil); err != nil {
				t.Fatal(err)
			}
			project := fixture.config.Projects[fixture.projectID]
			project.Checks = [][]string{tt.check(t, fixture)}
			fixture.config.Projects[fixture.projectID] = project
			fixture.operator = mustNewOperator(t, fixture.config)
			oldRC := fixture.remoteRef(t, "rc")

			if _, err := fixture.operator.MergeRC(context.Background(), fixture.projectID, testTaskID, taskCommit); err == nil {
				t.Fatal("MergeRC() error = nil, want check repository change rejection")
			}
			if got := fixture.remoteRef(t, "rc"); got != oldRC {
				t.Fatalf("failed merge changed remote RC to %q, want %q", got, oldRC)
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

func TestOperatorRejectsWorktreePushURLAndPinnedRemoteMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, opsFixture, string)
	}{
		{
			name: "worktree push URL",
			mutate: func(t *testing.T, fixture opsFixture, alternate string) {
				git(t, fixture.repo, "config", "extensions.worktreeConfig", "true")
				git(t, fixture.repo, "config", "--worktree", "remote.origin.pushurl", alternate)
			},
		},
		{
			name: "local remote URL changed",
			mutate: func(t *testing.T, fixture opsFixture, alternate string) {
				git(t, fixture.repo, "config", "remote.origin.url", alternate)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newOpsFixture(t)
			alternate := filepath.Join(privateTempDir(t), "alternate.git")
			git(t, filepath.Dir(alternate), "init", "--bare", alternate)
			branch, commit := fixture.createTaskCommit(t, testTaskID, "redirect.txt", "redirect\n")
			tt.mutate(t, fixture, alternate)

			if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, commit, nil); err == nil {
				t.Fatal("PushTaskBundle() accepted redirected repository configuration")
			}
			if got := remoteRefOrEmpty(t, alternate, branch); got != "" {
				t.Fatalf("alternate remote received task commit %q", got)
			}
		})
	}
}

func TestOperatorRejectsPushConfigAndNeverLeaksTags(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	branch, commit := fixture.createTaskCommit(t, testTaskID, "tagged.txt", "tagged\n")
	git(t, fixture.repo, "tag", "release-leak", commit)
	git(t, fixture.repo, "config", "push.followTags", "true")
	git(t, fixture.repo, "config", "push.pushOption", "leak")

	if err := pushTaskBundle(t, fixture.operator, fixture.projectID, fixture.repo, testTaskID, branch, commit, nil); err == nil || !strings.Contains(err.Error(), "unsafe local") {
		t.Fatalf("PushTaskBundle() error = %v, want unsafe local configuration rejection", err)
	}
	if got := remoteRefOrEmpty(t, fixture.remote, "release-leak"); got != "" {
		t.Fatalf("remote received tag %q", got)
	}
	if got := remoteRefOrEmpty(t, fixture.remote, branch); got != "" {
		t.Fatalf("remote received task branch %q", got)
	}
}

func TestOperatorSyncFetchesOnlyPinnedBranchesWithoutTags(t *testing.T) {
	t.Parallel()

	fixture := newOpsFixture(t)
	producer := fixture.clone(t)
	writeFile(t, filepath.Join(producer, "tag-source.txt"), "tag source\n")
	git(t, producer, "add", "tag-source.txt")
	git(t, producer, "commit", "-m", "tag source")
	wantMain := git(t, producer, "rev-parse", "HEAD")
	git(t, producer, "tag", "sync-leak", wantMain)
	git(t, producer, "push", "origin", "HEAD:main", "refs/tags/sync-leak")

	if err := fixture.operator.Sync(context.Background(), fixture.projectID); err != nil {
		t.Fatal(err)
	}
	if tags := git(t, fixture.repo, "tag", "--list", "sync-leak"); tags != "" {
		t.Fatalf("Sync() fetched tag %q", tags)
	}
	if got := git(t, fixture.repo, "rev-parse", "refs/remotes/origin/main"); got != wantMain {
		t.Fatalf("Sync() main = %q, want %q", got, wantMain)
	}
}

func TestOperatorUsesConfiguredGitWithSanitizedEnvironment(t *testing.T) {
	fixture := newOpsFixture(t)
	record := filepath.Join(privateTempDir(t), "git-env")
	real := realGit(t)
	wrapper := writeExecutable(t, filepath.Join(privateTempDir(t), "trusted-git"), `#!/bin/sh
printf '%s|%s|%s|%s|%s\n' "$PATH" "$HOME" "${CUSTOM_TOKEN-unset}" "${GIT_ASKPASS-unset}" "${HTTP_PROXY-unset}" >> "`+record+`"
exec "`+real+`" "$@"
`)
	fixture.config.GitBinary = wrapper
	t.Setenv("PATH", "/attacker/bin")
	t.Setenv("HOME", "relative-home")
	t.Setenv("CUSTOM_TOKEN", "secret")
	t.Setenv("GIT_ASKPASS", "/attacker/askpass")
	t.Setenv("HTTP_PROXY", "http://attacker.invalid")
	fixture.operator = mustNewOperator(t, fixture.config)
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fixture.operator.Sync(context.Background(), fixture.projectID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "/usr/bin:/bin|/nonexistent|unset|unset|unset" {
			t.Fatalf("Git environment = %q, want fixed sanitized values", line)
		}
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
	helper := writeExecutable(t, filepath.Join(privateTempDir(t), "deploy"), `#!/bin/sh
printf '{"project":"%s","task":"%s","commit":"%s","key":"%s","dir":"%s","arg":"%s"}' \
  "$QQCODEX_PROJECT_ID" "$QQCODEX_TASK_ID" "$QQCODEX_RC_COMMIT" "$QQCODEX_DEPLOY_KEY" "$PWD" "$1" > "$2"
`)
	project := fixture.config.Projects[fixture.projectID]
	project.DeployAction = []string{helper, "fixed", record}
	fixture.config.Projects[fixture.projectID] = project
	fixture.operator = mustNewOperator(t, fixture.config)
	t.Setenv("QQCODEX_PROJECT_ID", "attacker-project")
	t.Setenv("QQCODEX_TASK_ID", "attacker-task")
	t.Setenv("QQCODEX_RC_COMMIT", "attacker-commit")
	t.Setenv("QQCODEX_DEPLOY_KEY", "attacker-key")

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
		"key":     deployKey(fixture.projectID, testTaskID, fixture.rcCommit),
		"dir":     "/",
		"arg":     "fixed",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("deploy %s = %q, want %q", key, got[key], value)
		}
	}
	firstKey := got["key"]
	if err := fixture.operator.DeployRC(context.Background(), fixture.projectID, testTaskID, fixture.rcCommit); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["key"] != firstKey {
		t.Fatalf("deploy key changed from %q to %q", firstKey, got["key"])
	}

	if err := fixture.operator.DeployRC(context.Background(), fixture.projectID, testTaskID+";touch /tmp/bad", fixture.rcCommit); err == nil {
		t.Fatal("DeployRC() accepted an invalid task ID")
	}
}

func TestOperatorDeployRCCleanupFailurePreventsAction(t *testing.T) {
	fixture := newOpsFixture(t)
	marker := filepath.Join(t.TempDir(), "deploy-ran")
	deploy := writeExecutable(t, filepath.Join(privateTempDir(t), "deploy"), "#!/bin/sh\ntouch \""+marker+"\"\n")
	real := realGit(t)
	wrapper := writeExecutable(t, filepath.Join(privateTempDir(t), "cleanup-failing-git"), `#!/bin/sh
case " $* " in
  *" update-ref -d refs/qqcodex/"*) exit 88 ;;
esac
exec "`+real+`" "$@"
`)
	project := fixture.config.Projects[fixture.projectID]
	project.DeployAction = []string{deploy}
	fixture.config.GitBinary = wrapper
	fixture.config.Projects[fixture.projectID] = project
	fixture.operator = mustNewOperator(t, fixture.config)

	if err := fixture.operator.DeployRC(context.Background(), fixture.projectID, testTaskID, fixture.rcCommit); err == nil {
		t.Fatal("DeployRC() error = nil, want temporary ref cleanup failure")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("deploy action ran after cleanup failure, stat error = %v", err)
	}
}

func pushTaskBundle(t *testing.T, operator *Operator, projectID, worker, taskID, branch, commit string, bundle io.Reader) error {
	t.Helper()
	if bundle == nil {
		bundle = openBundle(t, worker, "refs/heads/"+branch)
	}
	return operator.PushTaskBundle(context.Background(), projectID, taskID, branch, commit, bundle)
}

func openBundle(t *testing.T, repo string, refs ...string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.bundle")
	args := append([]string{"bundle", "create", path}, refs...)
	git(t, repo, args...)
	bundle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	return bundle
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

	root := privateTempDir(t)
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
	runner := writeExecutable(t, filepath.Join(root, "check-runner"), `#!/bin/sh
if [ "$1" != "--" ]; then
  exit 64
fi
shift
exec "$@"
`)
	cfg := Config{GitBinary: realGit(t), Projects: map[string]Project{
		projectID: {
			RepoPath:         repo,
			Remote:           "origin",
			RemoteURL:        remote,
			AllowLocalRemote: true,
			BaseBranch:       "main",
			RCBranch:         "rc",
			CheckRunner:      []string{runner},
			Checks:           [][]string{{realGit(t), "diff", "--check", "HEAD^", "HEAD"}},
			DeployAction:     []string{realExecutable(t, "true")},
		},
	}}
	return opsFixture{
		projectID:  projectID,
		remote:     remote,
		repo:       repo,
		mainCommit: mainCommit,
		rcCommit:   rcCommit,
		config:     cfg,
		operator:   mustNewOperator(t, cfg),
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

func remoteRefOrEmpty(t *testing.T, remote, branch string) string {
	t.Helper()
	output := git(t, filepath.Dir(remote), "ls-remote", "--refs", remote, "refs/heads/"+branch, "refs/tags/"+branch)
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return ""
	}
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

func mustNewOperator(t *testing.T, cfg Config) *Operator {
	t.Helper()
	operator, err := NewOperator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return operator
}

func realGit(t *testing.T) string {
	t.Helper()
	return realExecutable(t, "git")
}

func realExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
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

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}
