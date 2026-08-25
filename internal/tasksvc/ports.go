package tasksvc

import (
	"context"
	"io"

	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/gitwork"
)

// Planner produces a structured plan for a new task.
type Planner interface {
	Plan(context.Context, codex.Request) (codex.Result, error)
}

// Runner executes or resumes one Codex session in a prepared worktree.
type Runner interface {
	Execute(context.Context, codex.Request) (codex.Result, error)
	Resume(context.Context, codex.Request) (codex.Result, error)
}

// Worktrees owns isolated task worktrees, configured checks, and commit validation.
type Worktrees interface {
	Prepare(context.Context, config.Project, string) (gitwork.Prepared, error)
	RunChecks(context.Context, config.Project, string, io.Writer) error
	ValidateCommit(context.Context, gitwork.Prepared) (string, error)
}

// Operator exposes only typed privileged operations required by task execution.
type Operator interface {
	Sync(context.Context, string) error
	PushTask(context.Context, string, string, string, string) error
}

// TaskLogs provides a redacted check-output writer and exact-value redaction.
type TaskLogs interface {
	Writer(taskID, stream string) io.Writer
	RedactText(string) string
}

// SchedulerControl wakes the worker after a task becomes runnable and stops a running task.
type SchedulerControl interface {
	Wake()
	Cancel(taskID string)
}

// Notifier sends one bounded group notification.
type Notifier interface {
	Send(context.Context, string, string) error
}

// LogReader returns a redacted, bounded task-log summary.
type LogReader interface {
	Summary(taskID string, maxRunes int) (string, error)
}

// Message is the small subset of a OneBot group message needed by the command service.
type Message struct {
	GroupID, UserID, MessageID, Text string
	Mentioned                        bool
}
