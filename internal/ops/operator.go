package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
)

var (
	taskIDPattern = regexp.MustCompile(`^T-[A-F0-9]{12}$`)
	commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

const (
	maxCommandOutput = 1 << 20
	commandWaitDelay = time.Second
	cleanupTimeout   = 10 * time.Second
)

type Operator struct {
	config    Config
	gitBinary string
	gitEnv    []string
}

func NewOperator(cfg Config) (*Operator, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Operator{
		config:    normalized,
		gitBinary: normalized.GitBinary,
		gitEnv:    gitEnvironment(),
	}, nil
}

func (o *Operator) Sync(ctx context.Context, projectID string) error {
	project, err := o.project(projectID)
	if err != nil {
		return err
	}
	if err := o.guardRepository(ctx, project); err != nil {
		return err
	}
	refspecs := []string{
		"refs/heads/" + project.BaseBranch + ":refs/remotes/" + project.Remote + "/" + project.BaseBranch,
		"refs/heads/" + project.RCBranch + ":refs/remotes/" + project.Remote + "/" + project.RCBranch,
	}
	args := []string{"fetch", "--prune", "--no-tags", "--no-prune-tags", "--recurse-submodules=no", "--no-auto-maintenance", project.RemoteURL}
	args = append(args, refspecs...)
	if _, err := o.runGit(ctx, project.RepoPath, args...); err != nil {
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
	if err := o.guardRepository(ctx, project); err != nil {
		return err
	}
	local, err := o.resolveCommit(ctx, project.RepoPath, "refs/heads/"+branch)
	if err != nil {
		return errors.New("local task branch does not exist")
	}
	if local != commit {
		return errors.New("local task branch does not match approved commit")
	}

	remoteCommit, cleanup, err := o.fetchRemoteBranch(ctx, project, branch)
	if cleanup != nil {
		defer func() {
			runErr = errors.Join(runErr, cleanup())
		}()
	}
	if err != nil {
		return err
	}
	if remoteCommit != "" {
		ancestor, err := o.isAncestor(ctx, project.RepoPath, remoteCommit, commit)
		if err != nil {
			return err
		}
		if !ancestor {
			return errors.New("remote task branch is not an ancestor of approved commit")
		}
	}
	if err := o.guardRepository(ctx, project); err != nil {
		return err
	}
	refspec := commit + ":refs/heads/" + branch
	if _, err := o.runGit(ctx, project.RepoPath, pushArgs(project.RemoteURL, refspec)...); err != nil {
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
	if err := o.guardRepository(ctx, project); err != nil {
		return "", err
	}

	remoteTask, remoteRC, cleanupRefs, err := o.fetchMergeBranches(ctx, project, branch)
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
		runErr = errors.Join(runErr, o.cleanupWorktree(project.RepoPath, worktree, added))
	}()
	if _, err := o.runGit(ctx, project.RepoPath, "worktree", "add", "--detach", worktree, remoteRC); err != nil {
		return "", errors.New("create RC worktree failed")
	}
	added = true
	if _, err := o.runGit(ctx, worktree, "merge", "--no-ff", "--no-edit", taskCommit); err != nil {
		return "", errors.New("RC merge failed")
	}
	merged, err := o.resolveCommit(ctx, worktree, "HEAD")
	if err != nil {
		return "", errors.New("resolve merged RC commit failed")
	}
	if err := runChecks(ctx, project, worktree); err != nil {
		return "", err
	}
	status, err := o.runGit(ctx, worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", errors.New("inspect checked RC worktree failed")
	}
	if status != "" {
		return "", errors.New("RC checks left the worktree dirty")
	}
	checkedHead, err := o.resolveCommit(ctx, worktree, "HEAD")
	if err != nil || checkedHead != merged {
		return "", errors.New("RC checks changed the merge commit")
	}
	if err := o.guardRepository(ctx, project); err != nil {
		return "", err
	}
	if _, err := o.runGit(ctx, worktree, pushArgs(project.RemoteURL, "HEAD:refs/heads/"+project.RCBranch)...); err != nil {
		return "", errors.New("RC push failed")
	}
	return merged, nil
}

func (o *Operator) DeployRC(ctx context.Context, projectID, taskID, rcCommit string) error {
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
	if err := o.guardRepository(ctx, project); err != nil {
		return err
	}
	remoteRC, cleanup, err := o.fetchRemoteBranch(ctx, project, project.RCBranch)
	if err != nil {
		return err
	}
	if remoteRC != rcCommit {
		if cleanup != nil {
			err = cleanup()
		}
		return errors.Join(errors.New("remote RC changed"), err)
	}
	if cleanup == nil {
		return errors.New("remote RC is unavailable")
	}
	if err := cleanup(); err != nil {
		return err
	}
	env := replaceEnvironment(os.Environ(), map[string]string{
		"QQCODEX_PROJECT_ID": projectID,
		"QQCODEX_TASK_ID":    taskID,
		"QQCODEX_RC_COMMIT":  rcCommit,
		"QQCODEX_DEPLOY_KEY": deployKey(projectID, taskID, rcCommit),
	})
	if _, err := runProcess(ctx, "/", project.DeployAction, env); err != nil {
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

func fetchArgs(remoteURL string, refspecs ...string) []string {
	args := []string{"fetch", "--no-tags", "--no-prune-tags", "--recurse-submodules=no", "--no-auto-maintenance", remoteURL}
	return append(args, refspecs...)
}

func pushArgs(remoteURL, refspec string) []string {
	return []string{
		"push", "--no-follow-tags", "--recurse-submodules=no", "--no-push-option", "--signed=no", "--no-verify",
		remoteURL, refspec,
	}
}

type localConfigEntry struct {
	origin, key, value string
}

func (o *Operator) guardRepository(ctx context.Context, project Project) error {
	repository, err := canonicalDirectory(project.RepoPath)
	if err != nil || repository != project.RepoPath {
		return errors.New("repository is unavailable")
	}
	configPath, err := o.runGit(ctx, project.RepoPath, "rev-parse", "--path-format=absolute", "--git-path", "config")
	if err != nil {
		return errors.New("inspect repository configuration failed")
	}
	configPath, err = filepath.EvalSymlinks(configPath)
	if err != nil {
		return errors.New("inspect repository configuration failed")
	}
	output, err := o.runGit(ctx, project.RepoPath, "config", "--local", "--includes", "--show-origin", "--null", "--list")
	if err != nil {
		return errors.New("inspect repository configuration failed")
	}
	entries, err := parseLocalConfig(output)
	if err != nil {
		return errors.New("repository contains unsafe local configuration")
	}
	seenURL, seenFetch := 0, 0
	for _, entry := range entries {
		origin := strings.TrimPrefix(entry.origin, "file:")
		if !filepath.IsAbs(origin) {
			origin = filepath.Join(project.RepoPath, origin)
		}
		origin, originErr := filepath.EvalSymlinks(origin)
		if originErr != nil || origin != configPath || !allowedLocalConfig(project, entry, &seenURL, &seenFetch) {
			return errors.New("repository contains unsafe local configuration")
		}
	}
	if seenURL != 1 || seenFetch != 1 {
		return errors.New("repository contains unsafe local configuration")
	}
	return nil
}

func parseLocalConfig(output string) ([]localConfigEntry, error) {
	fields := strings.Split(output, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, errors.New("invalid local config output")
	}
	entries := make([]localConfigEntry, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key, value, ok := strings.Cut(fields[i+1], "\n")
		if !ok || key == "" {
			return nil, errors.New("invalid local config entry")
		}
		entries = append(entries, localConfigEntry{origin: fields[i], key: strings.ToLower(key), value: value})
	}
	return entries, nil
}

func allowedLocalConfig(project Project, entry localConfigEntry, seenURL, seenFetch *int) bool {
	switch entry.key {
	case "core.repositoryformatversion":
		return entry.value == "0" || entry.value == "1"
	case "extensions.objectformat":
		return entry.value == "sha256"
	case "core.filemode", "core.bare", "core.logallrefupdates", "core.ignorecase", "core.precomposeunicode":
		return entry.value == "true" || entry.value == "false"
	case "user.name", "user.email":
		return entry.value != "" && !strings.ContainsRune(entry.value, 0)
	case "remote." + strings.ToLower(project.Remote) + ".url":
		(*seenURL)++
		value, err := normalizeRemoteURL(entry.value, project.AllowLocalRemote)
		return err == nil && value == project.RemoteURL
	case "remote." + strings.ToLower(project.Remote) + ".fetch":
		(*seenFetch)++
		return entry.value == "+refs/heads/*:refs/remotes/"+project.Remote+"/*"
	}
	if strings.HasPrefix(entry.key, "branch.") && strings.HasSuffix(entry.key, ".remote") {
		return entry.value == project.Remote
	}
	if strings.HasPrefix(entry.key, "branch.") && strings.HasSuffix(entry.key, ".merge") {
		return literalBranchRef(entry.value)
	}
	return false
}

func literalBranchRef(value string) bool {
	if !strings.HasPrefix(value, "refs/heads/") {
		return false
	}
	branch := strings.TrimPrefix(value, "refs/heads/")
	return branch != "" && !strings.ContainsAny(branch, " ~^:?*[\\") && !strings.Contains(branch, "..") && !strings.HasSuffix(branch, ".")
}

func (o *Operator) fetchRemoteBranch(ctx context.Context, project Project, branch string) (string, func() error, error) {
	remoteRef := "refs/heads/" + branch
	listed, err := o.runGit(ctx, project.RepoPath, "ls-remote", "--refs", project.RemoteURL, remoteRef)
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
	cleanup := o.refCleanup(project.RepoPath, temporary)
	if _, err := o.runGit(ctx, project.RepoPath, fetchArgs(project.RemoteURL, remoteRef+":"+temporary)...); err != nil {
		return "", nil, errors.Join(errors.New("fetch remote ref failed"), cleanup())
	}
	commit, err := o.resolveCommit(ctx, project.RepoPath, temporary)
	if err != nil {
		return "", nil, errors.Join(errors.New("resolve fetched remote ref failed"), cleanup())
	}
	return commit, cleanup, nil
}

func (o *Operator) fetchMergeBranches(ctx context.Context, project Project, taskBranch string) (string, string, func() error, error) {
	taskRef, err := temporaryRef("task")
	if err != nil {
		return "", "", nil, err
	}
	rcRef, err := temporaryRef("rc")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := o.refCleanup(project.RepoPath, taskRef, rcRef)
	refspecs := []string{
		"refs/heads/" + taskBranch + ":" + taskRef,
		"refs/heads/" + project.RCBranch + ":" + rcRef,
	}
	if _, err := o.runGit(ctx, project.RepoPath, fetchArgs(project.RemoteURL, refspecs...)...); err != nil {
		return "", "", nil, errors.Join(errors.New("fetch merge refs failed"), cleanup())
	}
	taskCommit, err := o.resolveCommit(ctx, project.RepoPath, taskRef)
	if err != nil {
		return "", "", nil, errors.Join(errors.New("resolve remote task commit failed"), cleanup())
	}
	rcCommit, err := o.resolveCommit(ctx, project.RepoPath, rcRef)
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

func (o *Operator) refCleanup(repo string, refs ...string) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		var cleanupErr error
		for _, ref := range refs {
			if _, err := o.runGit(ctx, repo, "update-ref", "-d", ref); err != nil {
				cleanupErr = errors.Join(cleanupErr, errors.New("clean temporary Git ref failed"))
			}
		}
		return cleanupErr
	}
}

func (o *Operator) resolveCommit(ctx context.Context, repo, revision string) (string, error) {
	commit, err := o.runGit(ctx, repo, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil || !commitPattern.MatchString(commit) {
		return "", errors.New("invalid commit")
	}
	return commit, nil
}

func (o *Operator) isAncestor(ctx context.Context, repo, ancestor, commit string) (bool, error) {
	_, err := o.runGit(ctx, repo, "merge-base", "--is-ancestor", ancestor, commit)
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
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return "", errors.New("secure RC worktree root failed")
	}
	return filepath.Join(root, "worktree"), nil
}

func (o *Operator) cleanupWorktree(repo, worktree string, added bool) error {
	var cleanupErr error
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if added {
		if _, err := o.runGit(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("clean RC worktree failed"))
		}
	}
	root := filepath.Dir(worktree)
	if err := os.RemoveAll(root); err != nil {
		cleanupErr = errors.Join(cleanupErr, errors.New("clean RC worktree directory failed"))
	}
	if !added || cleanupErr != nil {
		if _, err := o.runGit(ctx, repo, "worktree", "prune"); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("prune RC worktree registration failed"))
		}
	}
	return cleanupErr
}

func runChecks(ctx context.Context, project Project, worktree string) error {
	root := filepath.Dir(worktree)
	home := filepath.Join(root, "check-home")
	temp := filepath.Join(root, "check-tmp")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return errors.New("create check home failed")
	}
	if err := os.MkdirAll(temp, 0o700); err != nil {
		return errors.New("create check temporary directory failed")
	}
	env := fixedEnvironment(home, temp)
	for i, check := range project.Checks {
		argv := append(append([]string(nil), project.CheckRunner...), "--")
		argv = append(argv, check...)
		if _, err := runProcess(ctx, worktree, argv, env); err != nil {
			return fmt.Errorf("RC check %d failed", i+1)
		}
	}
	return nil
}

func (o *Operator) runGit(ctx context.Context, repo string, args ...string) (string, error) {
	argv := []string{
		o.gitBinary,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		// Git 2.43 accepts --no-push-option but does not clear configured push.pushOption.
		"-c", "push.pushOption=",
		"-C", repo,
	}
	argv = append(argv, args...)
	return runProcess(ctx, repo, argv, o.gitEnv)
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
		return strings.TrimSpace(output.buffer.String()), fmt.Errorf("process canceled: %w", cause)
	}
	if output.truncated {
		return strings.TrimSpace(output.buffer.String()), errors.New("process output exceeded limit")
	}
	if err != nil {
		return strings.TrimSpace(output.buffer.String()), err
	}
	return strings.TrimSpace(output.buffer.String()), nil
}

func checkEnvironment() []string {
	return fixedEnvironment("/nonexistent", "/tmp")
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

func gitEnvironment() []string {
	home := "/nonexistent"
	if value, ok := os.LookupEnv("HOME"); ok && filepath.IsAbs(value) {
		home = filepath.Clean(value)
	}
	env := fixedEnvironment(home, "/tmp")
	if socket, ok := os.LookupEnv("SSH_AUTH_SOCK"); ok && filepath.IsAbs(socket) {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			env = append(env, "SSH_AUTH_SOCK="+filepath.Clean(socket))
		}
	}
	return env
}

func deployKey(projectID, taskID, rcCommit string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + taskID + "\x00" + rcCommit))
	return hex.EncodeToString(sum[:])
}
