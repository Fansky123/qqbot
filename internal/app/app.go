package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"qqcodex/internal/config"
	"qqcodex/internal/onebot"
	"qqcodex/internal/store"
	"qqcodex/internal/tasksvc"
)

const (
	notificationAttempts   = 3
	notificationBackoff    = 100 * time.Millisecond
	defaultShutdownTimeout = 30 * time.Second
)

// ErrShutdownTimeout means at least one application component ignored graceful cancellation.
var ErrShutdownTimeout = errors.New("application shutdown timed out")

var retainedRuntimeLocks struct {
	sync.Mutex
	locks []*store.RuntimeLock
}

type Client interface {
	Run(context.Context, onebot.Handler) error
}

type Scheduler interface {
	Run(context.Context) error
}

type Service interface {
	Handle(context.Context, tasksvc.Message) error
}

type GroupAuthorizer interface {
	AllowedGroup(string) bool
}

type ProjectLookup interface {
	ProjectByID(string) (config.Project, bool)
}

type WorktreeRemover interface {
	Remove(context.Context, string, string) error
}

type TaskLogRemover interface {
	Remove(string) error
}

// App coordinates the process-wide runtime owner, message workers, and task scheduler.
type App struct {
	Store     *store.Store
	Client    Client
	Scheduler Scheduler
	Service   Service
	Groups    GroupAuthorizer
	Logger    *slog.Logger
	Projects  ProjectLookup
	Worktrees WorktreeRemover
	Logs      TaskLogRemover
	Now       func() time.Time

	MessageWorkers int
	// ShutdownTimeout bounds graceful draining after parent cancellation.
	// A non-positive value uses the default shutdown timeout.
	ShutdownTimeout time.Duration
}

type loopResult struct {
	name string
	err  error
}

type fatalState struct {
	mu     sync.Mutex
	err    error
	signal chan struct{}
	cancel context.CancelFunc
}

func newFatalState(cancel context.CancelFunc) *fatalState {
	return &fatalState{signal: make(chan struct{}), cancel: cancel}
}

func (s *fatalState) report(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = err
	s.cancel()
	close(s.signal)
}

func (s *fatalState) get() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fatalState) timeoutError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return errors.Join(s.err, ErrShutdownTimeout)
	}
	return ErrShutdownTimeout
}

func (a App) Run(ctx context.Context) (runErr error) {
	if err := a.validate(ctx); err != nil {
		return err
	}
	lock, err := a.Store.AcquireRuntimeLock()
	if err != nil {
		return fmt.Errorf("acquire application runtime lock: %w", err)
	}
	defer func() {
		if errors.Is(runErr, ErrShutdownTimeout) {
			// A component may still access durable state. Keep the process-wide fence
			// until process exit instead of allowing another runtime to take over.
			retainedRuntimeLocks.Lock()
			retainedRuntimeLocks.locks = append(retainedRuntimeLocks.locks, lock)
			retainedRuntimeLocks.Unlock()
			return
		}
		runErr = errors.Join(runErr, lock.Close())
	}()
	if err := a.Store.RecoverInterrupted(ctx, lock); err != nil {
		return fmt.Errorf("recover interrupted tasks: %w", err)
	}
	return a.runLoops(ctx)
}

// CleanupExpired removes expired local artifacts while preserving task records and remote branches.
func (a App) CleanupExpired(ctx context.Context) (runErr error) {
	if ctx == nil {
		return errors.New("application context is required")
	}
	if a.Store == nil || a.Projects == nil || a.Worktrees == nil || a.Logs == nil {
		return errors.New("cleanup dependencies are required")
	}
	lock, err := a.Store.AcquireRuntimeLock()
	if err != nil {
		return fmt.Errorf("acquire application runtime lock: %w", err)
	}
	defer func() { runErr = errors.Join(runErr, lock.Close()) }()

	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now().UTC()
	}
	tasks, err := a.Store.ListCleanupCandidates(ctx, now)
	if err != nil {
		return fmt.Errorf("list cleanup candidates: %w", err)
	}
	var cleanupErr error
	for _, task := range tasks {
		project, ok := a.Projects.ProjectByID(task.ProjectID)
		if !ok || project.LogRetentionDays <= 0 {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup task %s: project retention is unavailable", task.ID))
			continue
		}
		if !task.UpdatedAt.Before(now.AddDate(0, 0, -project.LogRetentionDays)) {
			continue
		}
		if task.Worktree != "" {
			if err := a.Worktrees.Remove(ctx, project.RepoPath, task.Worktree); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup task %s worktree: %w", task.ID, err))
				continue
			}
		}
		if err := a.Logs.Remove(task.ID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup task %s log: %w", task.ID, err))
		}
	}
	return cleanupErr
}

func (a App) validate(ctx context.Context) error {
	if ctx == nil {
		return errors.New("application context is required")
	}
	if a.Store == nil || a.Client == nil || a.Scheduler == nil || a.Service == nil || a.Groups == nil {
		return errors.New("application dependencies are required")
	}
	if a.MessageWorkers <= 0 {
		return errors.New("application message workers must be positive")
	}
	return nil
}

func (a App) runLoops(parent context.Context) error {
	loopCtx, cancelLoops := context.WithCancel(parent)
	defer cancelLoops()
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(parent))
	defer cancelWorkers()
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}

	messages := make(chan tasksvc.Message, a.MessageWorkers*2)
	var workers sync.WaitGroup
	fatal := newFatalState(cancelWorkers)
	for range a.MessageWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.messageWorker(workerCtx, logger, messages, fatal.report)
		}()
	}

	results := make(chan loopResult, 2)
	go func() {
		err := a.Scheduler.Run(loopCtx)
		if errors.Is(err, tasksvc.ErrFatalStore) {
			fatal.report(err)
		}
		results <- loopResult{name: "scheduler", err: err}
	}()
	go func() {
		err := a.Client.Run(loopCtx, func(handlerCtx context.Context, message onebot.GroupMessage) error {
			if !a.Groups.AllowedGroup(string(message.GroupID)) {
				return nil
			}
			immutable := tasksvc.Message{
				GroupID: string(message.GroupID), UserID: string(message.UserID), MessageID: string(message.MessageID),
				Text: message.Text, Mentioned: message.Mentioned,
			}
			select {
			case messages <- immutable:
				return nil
			case <-handlerCtx.Done():
				return handlerCtx.Err()
			case <-loopCtx.Done():
				return loopCtx.Err()
			}
		})
		if errors.Is(err, tasksvc.ErrFatalStore) {
			fatal.report(err)
		}
		results <- loopResult{name: "onebot", err: err}
	}()

	first := loopResult{}
	select {
	case <-parent.Done():
		cancelLoops()
	case <-fatal.signal:
		cancelLoops()
	case first = <-results:
		cancelLoops()
	}
	if errors.Is(first.err, tasksvc.ErrFatalStore) {
		fatal.report(first.err)
	}
	shutdownTimeout := a.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	shutdownTimer := time.NewTimer(shutdownTimeout)
	defer shutdownTimer.Stop()
	shutdownC := shutdownTimer.C

	// Stop intake before closing the queue; the client handler can no longer enqueue after both loops join.
	remaining := 2
	if first.name != "" {
		remaining--
	}
	for range remaining {
		select {
		case <-results:
		case <-shutdownC:
			cancelWorkers()
			return fatal.timeoutError()
		}
	}
	close(messages)
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	for workersDone != nil {
		select {
		case <-workersDone:
			workersDone = nil
		case <-shutdownC:
			cancelWorkers()
			return fatal.timeoutError()
		}
	}

	fatalErr := fatal.get()
	if fatalErr != nil {
		return fatalErr
	}
	if parent.Err() != nil {
		return nil
	}
	if first.err == nil {
		return fmt.Errorf("%s loop stopped unexpectedly", first.name)
	}
	return fmt.Errorf("%s loop failed: %w", first.name, first.err)
}

func (a App) messageWorker(ctx context.Context, logger *slog.Logger, messages <-chan tasksvc.Message, reportFatal func(error)) {
	for {
		if ctx.Err() != nil {
			return
		}
		var message tasksvc.Message
		var ok bool
		select {
		case <-ctx.Done():
			return
		case message, ok = <-messages:
			if !ok {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := a.handleMessage(ctx, message); err != nil {
			if errors.Is(err, tasksvc.ErrFatalStore) {
				reportFatal(err)
				return
			}
			if ctx.Err() == nil {
				logger.Warn("message handling failed",
					"class", messageErrorClass(err),
					"group_id", message.GroupID,
					"user_id", message.UserID,
					"message_id", message.MessageID,
				)
			}
		}
	}
}

func (a App) handleMessage(ctx context.Context, message tasksvc.Message) error {
	for attempt := 1; attempt <= notificationAttempts; attempt++ {
		err := a.Service.Handle(ctx, message)
		if err == nil || !errors.Is(err, tasksvc.ErrNotificationDelivery) || attempt == notificationAttempts {
			return err
		}
		timer := time.NewTimer(notificationBackoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func messageErrorClass(err error) string {
	switch {
	case errors.Is(err, tasksvc.ErrNotificationDelivery):
		return "notification_delivery"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	default:
		return "business"
	}
}
