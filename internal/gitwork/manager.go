package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"qqcodex/internal/config"
)

var (
	taskIDPattern = regexp.MustCompile(`^T-[A-F0-9]{12}$`)
	branchPattern = regexp.MustCompile(`^codex/T-[A-F0-9]{12}$`)
	commitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

const (
	checkWaitDelay         = time.Second
	prepareRollbackTimeout = 5 * time.Second
	maxCheckOutputBytes    = 1 << 20
)

type Prepared struct {
	Path, Branch, BaseCommit, GitCommonDir string
}

type Manager struct {
	Root string
}

func (m Manager) Prepare(ctx context.Context, project config.Project, taskID string) (Prepared, error) {
	if !taskIDPattern.MatchString(taskID) {
		return Prepared{}, fmt.Errorf("invalid task ID %q", taskID)
	}

	root, err := m.resolveRoot(true)
	if err != nil {
		return Prepared{}, err
	}
	repo, err := resolveDirectory(project.RepoPath)
	if err != nil {
		return Prepared{}, fmt.Errorf("resolve repository: %w", err)
	}
	worktree, err := resolveContained(root, filepath.Join(root, taskID), false)
	if err != nil {
		return Prepared{}, fmt.Errorf("resolve worktree: %w", err)
	}
	baseRef, err := remoteBaseRef(ctx, repo, project.Remote, project.BaseBranch)
	if err != nil {
		return Prepared{}, err
	}
	baseCommit, err := runGit(ctx, repo, "rev-parse", "--verify", "--end-of-options", baseRef+"^{commit}")
	if err != nil {
		return Prepared{}, fmt.Errorf("resolve base commit: %w", err)
	}
	branch := "codex/" + taskID
	if _, err := runGit(ctx, repo, "worktree", "add", "-b", branch, worktree, baseCommit); err != nil {
		return Prepared{}, fmt.Errorf("add worktree: %w", err)
	}

	commonDir, err := gitCommonDir(ctx, worktree)
	if err != nil {
		rollbackErr := rollbackPreparedWorktree(repo, worktree, branch)
		return Prepared{}, errors.Join(fmt.Errorf("resolve Git common directory: %w", err), rollbackErr)
	}

	return Prepared{
		Path:         worktree,
		Branch:       branch,
		BaseCommit:   baseCommit,
		GitCommonDir: commonDir,
	}, nil
}

func rollbackPreparedWorktree(repo, worktree, branch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), prepareRollbackTimeout)
	defer cancel()
	if _, err := runGit(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
		return fmt.Errorf("roll back worktree: %w", err)
	}
	if _, err := runGit(ctx, repo, "branch", "-D", "--", branch); err != nil {
		return fmt.Errorf("roll back task branch: %w", err)
	}
	return nil
}

func (m Manager) RunChecks(ctx context.Context, project config.Project, worktree string) error {
	root, err := m.resolveRoot(false)
	if err != nil {
		return err
	}
	worktree, err = resolveContained(root, worktree, true)
	if err != nil {
		return fmt.Errorf("resolve worktree: %w", err)
	}

	for i, check := range project.Checks {
		if len(check) == 0 || check[0] == "" {
			return fmt.Errorf("check %d has no executable", i+1)
		}
		if err := runCheck(ctx, worktree, check); err != nil {
			return fmt.Errorf("check %d: %w", i+1, err)
		}
	}
	return nil
}

func runCheck(ctx context.Context, worktree string, check []string) error {
	cmd := exec.CommandContext(ctx, check[0], check[1:]...)
	cmd.Dir = worktree
	cmd.Env = minimalEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = checkWaitDelay
	var output boundedCapture
	cmd.Stdout = &output
	cmd.Stderr = &output

	var canceled atomic.Bool
	cmd.Cancel = func() error {
		canceled.Store(true)
		return killProcessGroup(cmd.Process)
	}
	err := cmd.Run()
	if canceled.Load() {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return fmt.Errorf("canceled: %w", cause)
	}
	if err != nil {
		return fmt.Errorf("failed: %w: %s", err, output.String())
	}
	return nil
}

type boundedCapture struct {
	buffer    bytes.Buffer
	truncated bool
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxCheckOutputBytes - c.buffer.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = c.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		c.truncated = true
	}
	return written, nil
}

func (c *boundedCapture) String() string {
	output := strings.TrimSpace(c.buffer.String())
	if c.truncated {
		output += fmt.Sprintf("\n[check output truncated after %d bytes]", maxCheckOutputBytes)
	}
	return output
}

func (m Manager) ValidateCommit(ctx context.Context, prepared Prepared) (string, error) {
	root, err := m.resolveRoot(false)
	if err != nil {
		return "", err
	}
	worktree, err := resolveContained(root, prepared.Path, true)
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	if !branchPattern.MatchString(prepared.Branch) {
		return "", fmt.Errorf("invalid task branch %q", prepared.Branch)
	}
	if !commitPattern.MatchString(prepared.BaseCommit) {
		return "", errors.New("invalid base commit")
	}

	commonDir, err := gitCommonDir(ctx, worktree)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	recordedCommonDir, err := resolveDirectory(prepared.GitCommonDir)
	if err != nil {
		return "", fmt.Errorf("resolve recorded Git common directory: %w", err)
	}
	if commonDir != recordedCommonDir {
		return "", errors.New("git common directory changed")
	}

	branch, err := runGit(ctx, worktree, "branch", "--show-current")
	if err != nil {
		return "", fmt.Errorf("resolve current branch: %w", err)
	}
	if branch != prepared.Branch {
		return "", fmt.Errorf("wrong branch %q, want %q", branch, prepared.Branch)
	}
	status, err := runGit(ctx, worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("inspect worktree: %w", err)
	}
	if status != "" {
		return "", errors.New("worktree is dirty")
	}

	commit, err := runGit(ctx, worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve task commit: %w", err)
	}
	if strings.EqualFold(commit, prepared.BaseCommit) {
		return "", errors.New("task has no new commits")
	}
	if err := isAncestor(ctx, worktree, prepared.BaseCommit, commit); err != nil {
		return "", err
	}
	return commit, nil
}

func (m Manager) Remove(ctx context.Context, repoPath, worktree string) error {
	root, err := m.resolveRoot(false)
	if err != nil {
		return err
	}
	worktree, err = resolveContained(root, worktree, true)
	if err != nil {
		return fmt.Errorf("resolve worktree: %w", err)
	}
	repo, err := resolveDirectory(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	if _, err := runGit(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

func (m Manager) resolveRoot(create bool) (string, error) {
	if m.Root == "" {
		return "", errors.New("worktree root is required")
	}
	abs, err := filepath.Abs(m.Root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	if create {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", fmt.Errorf("create worktree root: %w", err)
		}
	}
	root, err := resolveDirectory(abs)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	return root, nil
}

func resolveDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return resolved, nil
}

func resolveContained(root, path string, mustExist bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if mustExist || !os.IsNotExist(err) {
			return "", err
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(abs))
		if parentErr != nil {
			return "", parentErr
		}
		resolved = filepath.Join(parent, filepath.Base(abs))
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is outside worktree root", resolved)
	}
	return resolved, nil
}

func gitCommonDir(ctx context.Context, worktree string) (string, error) {
	commonDir, err := runGit(ctx, worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktree, commonDir)
	}
	return resolveDirectory(commonDir)
}

func remoteBaseRef(ctx context.Context, repo, remote, branch string) (string, error) {
	if remote == "" || branch == "" {
		return "", errors.New("project remote and base branch are required")
	}
	remotes, err := runGit(ctx, repo, "remote")
	if err != nil {
		return "", fmt.Errorf("list repository remotes: %w", err)
	}
	found := false
	for _, name := range strings.Split(remotes, "\n") {
		if name == remote {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("repository remote %q does not exist", remote)
	}

	ref := "refs/remotes/" + remote + "/" + branch
	if _, err := runGit(ctx, repo, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("invalid remote base ref %q: %w", ref, err)
	}
	return ref, nil
}

func isAncestor(ctx context.Context, repo, base, commit string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", base, commit)
	cmd.Env = minimalEnvironment()
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return errors.New("task commit is not descended from base")
	}
	return fmt.Errorf("verify task ancestry: %w: %s", err, strings.TrimSpace(string(output)))
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	gitArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Env = minimalEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func minimalEnvironment() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "LANG", "TMPDIR", "TMP", "TEMP"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
