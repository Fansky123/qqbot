package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const maxResponseBytes = 64 << 10

const maxTaskBundleBytes int64 = 256 << 20

type Client struct {
	command        []string
	sourceRepos    map[string]string
	sourceGit      string
	maxBundleBytes int64
	log            LogSink
}

type LogSink interface {
	Append(taskID, stream string, data []byte) error
}

type helperResponse struct {
	OK            *bool  `json:"ok"`
	Error         string `json:"error,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	CurrentCommit string `json:"current_commit,omitempty"`
	RCCommit      string `json:"rc_commit,omitempty"`
}

func NewClient(command []string, sourceRepos map[string]string, log LogSink) (*Client, error) {
	if len(command) != 3 || command[1] != "-config" {
		return nil, errors.New("ops command is invalid")
	}
	configPath, err := trustedConfigFile(command[2])
	if err != nil {
		return nil, errors.New("ops command is invalid")
	}
	clonedCommand := append([]string(nil), command...)
	clonedCommand[2] = configPath
	return newClient(clonedCommand, sourceRepos, false, log)
}

func newClient(command []string, sourceRepos map[string]string, allowCurrentUID bool, log LogSink) (*Client, error) {
	if err := validateArgv(command); err != nil {
		return nil, errors.New("ops command is invalid")
	}
	executable, err := trustedExecutable(command[0], nil, allowCurrentUID)
	if err != nil {
		return nil, errors.New("ops command is invalid")
	}
	sourceGit, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("source Git is unavailable")
	}
	sourceGit, err = trustedExecutable(sourceGit, nil, allowCurrentUID)
	if err != nil {
		return nil, errors.New("source Git is invalid")
	}
	repositories := make(map[string]string, len(sourceRepos))
	for projectID, repo := range sourceRepos {
		if err := validateProjectID(projectID); err != nil {
			return nil, err
		}
		canonical, err := canonicalDirectory(repo)
		if err != nil {
			return nil, fmt.Errorf("source repository %q: %w", projectID, err)
		}
		repositories[projectID] = canonical
	}
	clonedCommand := append([]string(nil), command...)
	clonedCommand[0] = executable
	return &Client{
		command:        clonedCommand,
		sourceRepos:    repositories,
		sourceGit:      sourceGit,
		maxBundleBytes: maxTaskBundleBytes,
		log:            log,
	}, nil
}

func (c *Client) Sync(ctx context.Context, projectID string) error {
	_, err := c.call(ctx, "", nil, "sync", "--project", projectID)
	return err
}

// Preflight asks the privileged helper to validate one expected release configuration.
func (c *Client) Preflight(ctx context.Context, projectID, remote, baseBranch, rcBranch string, checks [][]string) error {
	if c == nil || c.sourceRepos == nil {
		return errors.New("ops client is not initialized")
	}
	source, ok := c.sourceRepos[projectID]
	if !ok {
		return errors.New("unknown source project")
	}
	device, inode, err := directoryIdentity(source)
	if err != nil {
		return err
	}
	fingerprint := ProjectFingerprint(Project{Remote: remote, BaseBranch: baseBranch, RCBranch: rcBranch, Checks: checks})
	_, err = c.call(ctx, "", nil,
		"validate", "--project", projectID,
		"--config-sha256", fingerprint,
		"--source-device", strconv.FormatUint(device, 10),
		"--source-inode", strconv.FormatUint(inode, 10),
	)
	return err
}

func (c *Client) PushTask(ctx context.Context, projectID, taskID, branch, commit string) error {
	if c == nil || c.sourceRepos == nil {
		return errors.New("ops client is not initialized")
	}
	if err := validateTask(taskID, branch, commit); err != nil {
		return err
	}
	repo, ok := c.sourceRepos[projectID]
	if !ok {
		return errors.New("unknown source project")
	}
	bundle, err := c.createTaskBundle(ctx, repo, branch, commit)
	if err != nil {
		return err
	}
	_, callErr := c.call(ctx, "", bundle, "push", "--project", projectID, "--task", taskID, "--branch", branch, "--commit", commit)
	closeErr := bundle.Close()
	if callErr != nil {
		return callErr
	}
	if closeErr != nil {
		// The helper may already have pushed; a close error must not trigger a retry.
		return nil
	}
	return nil
}

func (c *Client) createTaskBundle(ctx context.Context, repo, branch, commit string) (*os.File, error) {
	fullRef := "refs/heads/" + branch
	root, err := os.MkdirTemp("", "qqcodex-task-bundle-")
	if err != nil {
		return nil, errors.New("create task bundle directory failed")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, errors.New("secure task bundle directory failed")
	}
	path := filepath.Join(root, "task.bundle")
	cleanup := func() {
		_ = os.Remove(path)
		_ = os.Remove(root)
	}
	env := sourceGitEnvironment(root)
	resolved, err := runProcess(ctx, repo, []string{c.sourceGit, "rev-parse", "--verify", "--end-of-options", fullRef + "^{commit}"}, env)
	if err != nil || resolved != commit {
		cleanup()
		return nil, errors.New("source task branch does not match claimed commit")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return nil, errors.New("create task bundle failed")
	}
	if err := runBundleProcess(ctx, repo, []string{c.sourceGit, "bundle", "create", "-", fullRef}, env, file, c.maxBundleBytes); err != nil {
		_ = file.Close()
		cleanup()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return nil, errors.New("sync task bundle failed")
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, errors.New("close task bundle failed")
	}
	heads, err := runProcess(ctx, repo, []string{c.sourceGit, "bundle", "list-heads", path}, env)
	if err != nil || !singleBundleHead(heads, fullRef, commit) {
		cleanup()
		return nil, errors.New("task bundle contains unexpected refs")
	}
	reader, err := os.Open(path)
	if err != nil {
		cleanup()
		return nil, errors.New("open task bundle failed")
	}
	if err := os.Remove(path); err != nil {
		_ = reader.Close()
		cleanup()
		return nil, errors.New("unlink task bundle failed")
	}
	if err := os.Remove(root); err != nil {
		_ = reader.Close()
		return nil, errors.New("clean task bundle directory failed")
	}
	return reader, nil
}

func runBundleProcess(ctx context.Context, dir string, argv, env []string, output *os.File, limit int64) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	var canceled atomic.Bool
	cmd.Cancel = processGroupCancel(cmd, &canceled)
	var overflow atomic.Bool
	cmd.Stdout = &bundleWriter{file: output, limit: limit, overflow: &overflow, kill: func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}}
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if overflow.Load() {
		return errors.New("task bundle exceeded limit")
	}
	if canceled.Load() {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return fmt.Errorf("task bundle canceled: %w", cause)
	}
	if stderr.truncated {
		return errors.New("task bundle output exceeded limit")
	}
	if err != nil {
		return errors.New("create task bundle failed")
	}
	return nil
}

type bundleWriter struct {
	file     *os.File
	written  int64
	limit    int64
	overflow *atomic.Bool
	kill     func()
}

func (w *bundleWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.overflow.Store(true)
		w.kill()
		return 0, errors.New("bundle limit exceeded")
	}
	if int64(len(p)) > remaining {
		n, err := w.file.Write(p[:remaining])
		w.written += int64(n)
		w.overflow.Store(true)
		w.kill()
		if err != nil {
			return n, err
		}
		return n, errors.New("bundle limit exceeded")
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

func singleBundleHead(output, expectedRef, expectedCommit string) bool {
	fields := strings.Fields(output)
	return len(fields) == 2 && fields[0] == expectedCommit && fields[1] == expectedRef
}

func sourceGitEnvironment(temp string) []string {
	return append(fixedEnvironment("/nonexistent", temp),
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
}

func processGroupCancel(cmd *exec.Cmd, canceled *atomic.Bool) func() error {
	return func() error {
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
}

func (c *Client) MergeRC(ctx context.Context, projectID, taskID, commit string) (string, error) {
	return c.call(ctx, "merge", nil, "merge", "--project", projectID, "--task", taskID, "--commit", commit)
}

func (c *Client) DeployRC(ctx context.Context, projectID, taskID, rcCommit string) error {
	_, err := c.call(ctx, "", nil, "deploy", "--project", projectID, "--task", taskID, "--rc-commit", rcCommit)
	return err
}

func (c *Client) call(ctx context.Context, wantCommit string, stdin io.Reader, args ...string) (string, error) {
	if err := validateClientInputs(args); err != nil {
		return "", err
	}
	if c == nil || validateArgv(c.command) != nil {
		return "", errors.New("ops command is invalid")
	}
	argv := append(append([]string(nil), c.command...), args...)
	stdout, stderr, runErr := runClientProcess(ctx, argv, stdin)
	taskID := ""
	if args[0] != "sync" && args[0] != "validate" {
		taskID = args[4]
	}
	logErr := appendTaskLog(c.log, taskID, "ops.stderr", stderr)
	finish := func(value string, resultErr error) (string, error) {
		if logErr == nil {
			return value, resultErr
		}
		if resultErr != nil {
			return value, errors.Join(resultErr, logErr)
		}
		// The helper has confirmed the external mutation. Surface the audit failure
		// to service logs without returning a retryable action error.
		slog.ErrorContext(ctx, "append completed ops action to task log", "task_id", taskID, "error", logErr)
		return value, nil
	}
	response, decodeErr := decodeHelperResponse(stdout)
	if decodeErr != nil {
		if runErr != nil {
			return finish("", errors.New("ops helper failed with an invalid response"))
		}
		return finish("", decodeErr)
	}
	if *response.OK {
		if runErr != nil {
			return finish("", errors.New("ops helper reported success with a non-zero exit status"))
		}
		if response.Error != "" || response.ErrorCode != "" || response.CurrentCommit != "" {
			return finish("", errors.New("ops helper success response contains an error"))
		}
		if wantCommit == "merge" {
			if !commitPattern.MatchString(response.RCCommit) {
				return finish("", errors.New("ops helper returned an invalid RC commit"))
			}
			return finish(response.RCCommit, nil)
		}
		if response.RCCommit != "" {
			return finish("", errors.New("ops helper returned an unexpected RC commit"))
		}
		return finish("", nil)
	}
	if runErr == nil || response.Error == "" || response.RCCommit != "" {
		return finish("", errors.New("ops helper returned an invalid failure response"))
	}
	var responseErr error
	switch response.ErrorCode {
	case "":
		if response.CurrentCommit != "" {
			return finish("", errors.New("ops helper returned an invalid failure response"))
		}
		responseErr = errors.New(response.Error)
	case ErrorCodeTaskCommitChanged:
		if !commitPattern.MatchString(response.CurrentCommit) {
			return finish("", errors.New("ops helper returned an invalid changed commit"))
		}
		responseErr = fmt.Errorf("%w: %s", NewTaskCommitChanged(response.CurrentCommit), response.Error)
	case ErrorCodeRCCommitChanged:
		if !commitPattern.MatchString(response.CurrentCommit) {
			return finish("", errors.New("ops helper returned an invalid changed commit"))
		}
		responseErr = fmt.Errorf("%w: %s", NewRCCommitChanged(response.CurrentCommit), response.Error)
	case ErrorCodeMergeConflict:
		if response.CurrentCommit != "" {
			return finish("", errors.New("ops helper returned an invalid merge conflict"))
		}
		responseErr = fmt.Errorf("%w: %s", ErrMergeConflict, response.Error)
	default:
		return finish("", errors.New("ops helper returned an invalid failure code"))
	}
	return finish("", responseErr)
}

func validateClientInputs(args []string) error {
	if len(args) < 3 || args[1] != "--project" {
		return errors.New("invalid ops request")
	}
	if err := validateProjectID(args[2]); err != nil {
		return err
	}
	switch args[0] {
	case "sync":
		if len(args) != 3 {
			return errors.New("invalid sync request")
		}
	case "validate":
		if len(args) != 9 || args[3] != "--config-sha256" || len(args[4]) != sha256.Size*2 ||
			args[5] != "--source-device" || args[7] != "--source-inode" {
			return errors.New("invalid validate request")
		}
		if _, err := hex.DecodeString(args[4]); err != nil {
			return errors.New("invalid validate request")
		}
		for _, value := range []string{args[6], args[8]} {
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil || strconv.FormatUint(parsed, 10) != value {
				return errors.New("invalid validate request")
			}
		}
	case "push":
		if len(args) != 9 || args[3] != "--task" || args[5] != "--branch" || args[7] != "--commit" {
			return errors.New("invalid push request")
		}
		return validateTask(args[4], args[6], args[8])
	case "merge":
		if len(args) != 7 || args[3] != "--task" || args[5] != "--commit" {
			return errors.New("invalid merge request")
		}
		return validateTask(args[4], "codex/"+args[4], args[6])
	case "deploy":
		if len(args) != 7 || args[3] != "--task" || args[5] != "--rc-commit" || !taskIDPattern.MatchString(args[4]) || !commitPattern.MatchString(args[6]) {
			return errors.New("invalid deploy request")
		}
	default:
		return errors.New("invalid ops action")
	}
	return nil
}

func decodeHelperResponse(data []byte) (helperResponse, error) {
	if len(data) > maxResponseBytes {
		return helperResponse{}, errors.New("ops helper response exceeded limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response helperResponse
	if err := decoder.Decode(&response); err != nil {
		return helperResponse{}, errors.New("ops helper returned invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return helperResponse{}, errors.New("ops helper returned multiple JSON values")
	}
	if response.OK == nil {
		return helperResponse{}, errors.New("ops helper response has no status")
	}
	return response, nil
}

func runClientProcess(ctx context.Context, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = clientEnvironment()
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	var stdout limitedBytes
	var stderr boundedBuffer
	stdout.limit = maxResponseBytes + 1
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	var canceled atomic.Bool
	cmd.Cancel = processGroupCancel(cmd, &canceled)
	err := cmd.Run()
	stderrBytes := append([]byte(nil), stderr.buffer.Bytes()...)
	if stderr.truncated {
		stderrBytes = append(stderrBytes, []byte("\n[ops stderr truncated]\n")...)
	}
	if canceled.Load() {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return stdout.bytes, stderrBytes, fmt.Errorf("ops helper canceled: %w", cause)
	}
	return stdout.bytes, stderrBytes, err
}

func appendTaskLog(log LogSink, taskID, stream string, data []byte) error {
	if log == nil || taskID == "" || len(data) == 0 {
		return nil
	}
	if err := log.Append(taskID, stream, data); err != nil {
		return fmt.Errorf("append %s task log: %w", stream, err)
	}
	return nil
}

type limitedBytes struct {
	bytes []byte
	limit int
}

func (b *limitedBytes) Write(p []byte) (int, error) {
	want := len(p)
	remaining := b.limit - len(b.bytes)
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		b.bytes = append(b.bytes, p[:remaining]...)
	}
	return want, nil
}

func clientEnvironment() []string {
	return fixedEnvironment("/nonexistent", "/tmp")
}
