package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

const maxResponseBytes = 64 << 10

type Client struct {
	Command []string
}

type helperResponse struct {
	OK       *bool  `json:"ok"`
	Error    string `json:"error,omitempty"`
	RCCommit string `json:"rc_commit,omitempty"`
}

func (c Client) Sync(ctx context.Context, projectID string) error {
	_, err := c.call(ctx, "", "sync", "--project", projectID)
	return err
}

func (c Client) PushTask(ctx context.Context, projectID, taskID, branch, commit string) error {
	_, err := c.call(ctx, "", "push", "--project", projectID, "--task", taskID, "--branch", branch, "--commit", commit)
	return err
}

func (c Client) MergeRC(ctx context.Context, projectID, taskID, commit string) (string, error) {
	return c.call(ctx, "merge", "merge", "--project", projectID, "--task", taskID, "--commit", commit)
}

func (c Client) DeployRC(ctx context.Context, projectID, taskID, rcCommit string) error {
	_, err := c.call(ctx, "", "deploy", "--project", projectID, "--task", taskID, "--rc-commit", rcCommit)
	return err
}

func (c Client) call(ctx context.Context, wantCommit string, args ...string) (string, error) {
	if err := validateClientInputs(args); err != nil {
		return "", err
	}
	if err := validateArgv(c.Command); err != nil {
		return "", errors.New("ops command is invalid")
	}
	executable, err := trustedExecutable(c.Command[0], nil)
	if err != nil {
		return "", errors.New("ops command is invalid")
	}
	command := append([]string(nil), c.Command...)
	command[0] = executable
	argv := append(command, args...)
	stdout, runErr := runClientProcess(ctx, argv)
	response, decodeErr := decodeHelperResponse(stdout)
	if decodeErr != nil {
		if runErr != nil {
			return "", errors.New("ops helper failed with an invalid response")
		}
		return "", decodeErr
	}
	if *response.OK {
		if runErr != nil {
			return "", errors.New("ops helper reported success with a non-zero exit status")
		}
		if response.Error != "" {
			return "", errors.New("ops helper success response contains an error")
		}
		if wantCommit == "merge" {
			if !commitPattern.MatchString(response.RCCommit) {
				return "", errors.New("ops helper returned an invalid RC commit")
			}
			return response.RCCommit, nil
		}
		if response.RCCommit != "" {
			return "", errors.New("ops helper returned an unexpected RC commit")
		}
		return "", nil
	}
	if runErr == nil || response.Error == "" || response.RCCommit != "" {
		return "", errors.New("ops helper returned an invalid failure response")
	}
	return "", errors.New(response.Error)
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

func runClientProcess(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = clientEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	var stdout limitedBytes
	var stderr boundedBuffer
	stdout.limit = maxResponseBytes + 1
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
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
		return stdout.bytes, fmt.Errorf("ops helper canceled: %w", cause)
	}
	return stdout.bytes, err
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
