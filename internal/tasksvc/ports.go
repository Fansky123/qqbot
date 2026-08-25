package tasksvc

import (
	"context"

	"qqcodex/internal/codex"
)

// Planner produces a structured plan for a new task.
type Planner interface {
	Plan(context.Context, codex.Request) (codex.Result, error)
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
