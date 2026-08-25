package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	taskIDPattern  = regexp.MustCompile(`^T-[A-F0-9]{12}$`)
	commitPattern  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	scpURLPattern  = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+:[A-Za-z0-9._~/-]+$`)
	sshPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]+$`)
)

const (
	maxCommandOutput = 1 << 20
	commandWaitDelay = time.Second
	cleanupTimeout   = 10 * time.Second
)

type Operator struct {
	config Config
}

func NewOperator(cfg Config) *Operator {
	return &Operator{config: cloneConfig(cfg)}
}

func (o *Operator) Sync(ctx context.Context, projectID string) error {
	project, err := o.project(projectID)
	if err != nil {
		return err
	}
	if err := guardRepository(ctx, project); err != nil {
		return err
	}
	if _, err := runGit(ctx, project.RepoPath, "fetch", "--prune", project.Remote, project.BaseBranch, project.RCBranch); err != nil {
		return errors.New("sync fetch failed")
	}
	return nil
}

func (o *Operator) PushTask(ctx context.Context, projectID, taskID, branch, commit string) (runErr error) {
	project, err := o.project(projectID)
	if err != nil {
		return err
	}
	if err := validateTask(taskID, branch, commit); err != nil {
		return err
	}
	if err := guardRepository(ctx, project); err != nil {
		return err
	}
	local, err := resolveCommit(ctx, project.RepoPath, "refs/heads/"+branch)
	if err != nil {
		return errors.New("local task branch does not exist")
	}
	if local != commit {
		return errors.New("local task branch does not match approved commit")
	}

	remoteCommit, cleanup, err := fetchRemoteBranch(ctx, project, branch)
	if cleanup != nil {
		defer func() {
			runErr = errors.Join(runErr, cleanup())
		}()
	}
	if err != nil {
		return err
	}
	if remoteCommit != "" {
		ancestor, err := isAncestor(ctx, project.RepoPath, remoteCommit, commit)
		if err != nil {
			return err
		}
		if !ancestor {
			return errors.New("remote task branch is not an ancestor of approved commit")
		}
	}
	refspec := commit + ":refs/heads/" + branch
	if _, err := runGit(ctx, project.RepoPath, "push", project.Remote, refspec); err != nil {
		return errors.New("task push failed")
	}
	return nil
}

func (o *Operator) MergeRC(ctx context.Context, projectID, taskID, taskCommit string) (rcCommit string, runErr error) {
	project, err := o.project(projectID)
	if err != nil {
		return "", err
	}
	branch := "codex/" + taskID
	if err := validateTask(taskID, branch, taskCommit); err != nil {
		return "", err
	}
	if err := guardRepository(ctx, project); err != nil {
		return "", err
	}

	remoteTask, remoteRC, cleanupRefs, err := fetchMergeBranches(ctx, project, branch)
	if cleanupRefs != nil {
		defer func() {
			runErr = errors.Join(runErr, cleanupRefs())
		}()
	}
	if err != nil {
		return "", err
	}
	if remoteTask != taskCommit {
		return "", errors.New("remote task commit changed")
	}

	worktree, err := reserveWorktreePath()
	if err != nil {
		return "", err
	}
	added := false
	defer func() {
		cleanupErr := cleanupWorktree(project.RepoPath, worktree, added)
		runErr = errors.Join(runErr, cleanupErr)
	}()
	if _, err := runGit(ctx, project.RepoPath, "worktree", "add", "--detach", worktree, remoteRC); err != nil {
		return "", errors.New("create RC worktree failed")
	}
	added = true
	if _, err := runGit(ctx, worktree, "merge", "--no-ff", "--no-edit", taskCommit); err != nil {
		return "", errors.New("RC merge failed")
	}
	for i, check := range project.Checks {
		if _, err := runProcess(ctx, worktree, check, checkEnvironment()); err != nil {
			return "", fmt.Errorf("RC check %d failed", i+1)
		}
	}
	merged, err := resolveCommit(ctx, worktree, "HEAD")
	if err != nil {
		return "", errors.New("resolve merged RC commit failed")
	}
	if _, err := runGit(ctx, worktree, "push", project.Remote, "HEAD:refs/heads/"+project.RCBranch); err != nil {
		return "", errors.New("RC push failed")
	}
	return merged, nil
}

func (o *Operator) DeployRC(ctx context.Context, projectID, taskID, rcCommit string) (runErr error) {
	project, err := o.project(projectID)
	if err != nil {
		return err
	}
	if !taskIDPattern.MatchString(taskID) {
		return errors.New("invalid task ID")
	}
	if !commitPattern.MatchString(rcCommit) {
		return errors.New("invalid RC commit")
	}
	if err := guardRepository(ctx, project); err != nil {
		return err
	}
	remoteRC, cleanup, err := fetchRemoteBranch(ctx, project, project.RCBranch)
	if cleanup != nil {
		defer func() {
			runErr = errors.Join(runErr, cleanup())
		}()
	}
	if err != nil {
		return err
	}
	if remoteRC != rcCommit {
		return errors.New("remote RC changed")
	}
	env := replaceEnvironment(os.Environ(), map[string]string{
		"QQCODEX_PROJECT_ID": projectID,
		"QQCODEX_TASK_ID":    taskID,
		"QQCODEX_RC_COMMIT":  rcCommit,
	})
	if _, err := runProcess(ctx, project.RepoPath, project.DeployAction, env); err != nil {
		return errors.New("RC deploy failed")
	}
	return nil
}

func (o *Operator) project(projectID string) (Project, error) {
	if err := validateProjectID(projectID); err != nil {
		return Project{}, err
	}
	project, ok := o.config.Projects[projectID]
	if !ok {
		return Project{}, errors.New("unknown project")
	}
	return project, nil
}

func validateTask(taskID, branch, commit string) error {
	if !taskIDPattern.MatchString(taskID) {
		return errors.New("invalid task ID")
	}
	if branch != "codex/"+taskID {
		return errors.New("invalid task branch")
	}
	if !commitPattern.MatchString(commit) {
		return errors.New("invalid task commit")
	}
	return nil
}

func guardRepository(ctx context.Context, project Project) error {
	if _, err := canonicalDirectory(project.RepoPath); err != nil {
		return errors.New("repository is unavailable")
	}
	keys, err := runGit(ctx, project.RepoPath, "config", "--local", "--name-only", "--null", "--list")
	if err != nil {
		return errors.New("inspect repository configuration failed")
	}
	for _, key := range strings.Split(strings.ToLower(keys), "\x00") {
		if unsafeLocalConfigKey(key, strings.ToLower(project.Remote)) {
			return errors.New("repository contains unsafe local configuration")
		}
	}
	remoteURL, err := runGit(ctx, project.RepoPath, "config", "--local", "--get", "remote."+project.Remote+".url")
	if err != nil || !safeRemoteURL(remoteURL) {
		return errors.New("repository remote is unsafe")
	}
	return nil
}

func unsafeLocalConfigKey(key, remote string) bool {
	if key == "" {
		return false
	}
	for _, prefix := range []string{"include.", "includeif.", "filter.", "credential.", "url."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, exact := range []string{
		"core.alternaterefscommand", "core.askpass", "core.fsmonitor", "core.gitproxy", "core.hookspath", "core.sshcommand", "core.worktree",
		"commit.gpgsign", "merge.gpgsign", "gpg.program", "gpg.ssh.program",
		"remote." + remote + ".mirror", "remote." + remote + ".proxy", "remote." + remote + ".pushurl",
		"remote." + remote + ".receivepack", "remote." + remote + ".uploadpack", "remote." + remote + ".vcs",
	} {
		if key == exact {
			return true
		}
	}
	return strings.HasPrefix(key, "protocol.") && strings.HasSuffix(key, ".allow") ||
		strings.HasPrefix(key, "diff.") && strings.HasSuffix(key, ".command") ||
		strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver")
}

func safeRemoteURL(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	if filepath.IsAbs(value) {
		_, err := canonicalDirectory(value)
		return err == nil
	}
	if scpURLPattern.MatchString(value) && !strings.Contains(value, "::") {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return false
	}
	switch parsed.Scheme {
	case "https", "git":
	case "ssh":
		if parsed.RawPath != "" || !sshPathPattern.MatchString(parsed.Path) {
			return false
		}
	default:
		return false
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return false
		}
	}
	return true
}

func fetchRemoteBranch(ctx context.Context, project Project, branch string) (string, func() error, error) {
	remoteRef := "refs/heads/" + branch
	listed, err := runGit(ctx, project.RepoPath, "ls-remote", "--refs", project.Remote, remoteRef)
	if err != nil {
		return "", nil, errors.New("inspect remote ref failed")
	}
	if listed == "" {
		return "", nil, nil
	}
	fields := strings.Fields(listed)
	if len(fields) != 2 || fields[1] != remoteRef || !commitPattern.MatchString(fields[0]) {
		return "", nil, errors.New("invalid remote ref response")
	}
	temporary, err := temporaryRef("branch")
	if err != nil {
		return "", nil, err
	}
	cleanup := refCleanup(project.RepoPath, temporary)
	if _, err := runGit(ctx, project.RepoPath, "fetch", "--no-tags", project.Remote, remoteRef+":"+temporary); err != nil {
		return "", nil, errors.Join(errors.New("fetch remote ref failed"), cleanup())
	}
	commit, err := resolveCommit(ctx, project.RepoPath, temporary)
	if err != nil {
		return "", nil, errors.Join(errors.New("resolve fetched remote ref failed"), cleanup())
	}
	return commit, cleanup, nil
}

func fetchMergeBranches(ctx context.Context, project Project, taskBranch string) (string, string, func() error, error) {
	taskRef, err := temporaryRef("task")
	if err != nil {
		return "", "", nil, err
	}
	rcRef, err := temporaryRef("rc")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := refCleanup(project.RepoPath, taskRef, rcRef)
	_, err = runGit(ctx, project.RepoPath, "fetch", "--no-tags", project.Remote,
		"refs/heads/"+taskBranch+":"+taskRef,
		"refs/heads/"+project.RCBranch+":"+rcRef,
	)
	if err != nil {
		return "", "", nil, errors.Join(errors.New("fetch merge refs failed"), cleanup())
	}
	taskCommit, err := resolveCommit(ctx, project.RepoPath, taskRef)
	if err != nil {
		return "", "", nil, errors.Join(errors.New("resolve remote task commit failed"), cleanup())
	}
	rcCommit, err := resolveCommit(ctx, project.RepoPath, rcRef)
	if err != nil {
		return "", "", nil, errors.Join(errors.New("resolve remote RC commit failed"), cleanup())
	}
	return taskCommit, rcCommit, cleanup, nil
}

func temporaryRef(label string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("generate temporary ref failed")
	}
	return "refs/qqcodex/" + hex.EncodeToString(random[:]) + "/" + label, nil
}

func refCleanup(repo string, refs ...string) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		var cleanupErr error
		for _, ref := range refs {
			if _, err := runGit(ctx, repo, "update-ref", "-d", ref); err != nil {
				cleanupErr = errors.Join(cleanupErr, errors.New("clean temporary Git ref failed"))
			}
		}
		return cleanupErr
	}
}

func resolveCommit(ctx context.Context, repo, revision string) (string, error) {
	commit, err := runGit(ctx, repo, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil || !commitPattern.MatchString(commit) {
		return "", errors.New("invalid commit")
	}
	return commit, nil
}

func isAncestor(ctx context.Context, repo, ancestor, commit string) (bool, error) {
	_, err := runGit(ctx, repo, "merge-base", "--is-ancestor", ancestor, commit)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, errors.New("verify commit ancestry failed")
}

func reserveWorktreePath() (string, error) {
	root, err := os.MkdirTemp("", "qqcodex-rc-")
	if err != nil {
		return "", errors.New("reserve RC worktree failed")
	}
	return filepath.Join(root, "worktree"), nil
}

func cleanupWorktree(repo, worktree string, added bool) error {
	var cleanupErr error
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if added {
		if _, err := runGit(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("clean RC worktree failed"))
		}
	}
	root := filepath.Dir(worktree)
	if err := os.RemoveAll(root); err != nil {
		cleanupErr = errors.Join(cleanupErr, errors.New("clean RC worktree directory failed"))
	}
	if !added || cleanupErr != nil {
		if _, err := runGit(ctx, repo, "worktree", "prune"); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("prune RC worktree registration failed"))
		}
	}
	return cleanupErr
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	argv := []string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", repo}
	argv = append(argv, args...)
	return runProcess(ctx, repo, argv, os.Environ())
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	want := len(p)
	remaining := maxCommandOutput - b.buffer.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return want, nil
}

func runProcess(ctx context.Context, dir string, argv, env []string) (string, error) {
	if err := validateArgv(argv); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = commandWaitDelay
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	var canceled atomic.Bool
	cmd.Cancel = func() error {
		canceled.Store(true)
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := cmd.Run()
	if canceled.Load() {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return "", fmt.Errorf("process canceled: %w", cause)
	}
	if output.truncated {
		return "", errors.New("process output exceeded limit")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output.buffer.String()), nil
}

func checkEnvironment() []string {
	var env []string
	for _, name := range []string{
		"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR", "TMP", "TEMP",
		"XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "GOPATH", "GOENV", "GOPROXY", "GOSUMDB", "CGO_ENABLED",
	} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func replaceEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replace := values[name]; replace {
				continue
			}
		}
		result = append(result, entry)
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}
