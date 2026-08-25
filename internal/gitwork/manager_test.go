package gitwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

	if err := manager.Remove(ctx, fixture.project.RepoPath, first.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("removed worktree stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("second worktree was affected: %v", err)
	}
	listed := git(t, fixture.project.RepoPath, "worktree", "list", "--porcelain")
	if strings.Contains(listed, first.Path) {
		t.Fatalf("removed worktree remains registered:\n%s", listed)
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
