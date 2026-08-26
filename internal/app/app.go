package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"qqcodex/internal/onebot"
	"qqcodex/internal/store"
	"qqcodex/internal/tasksvc"
)

const (
	notificationAttempts = 3
	notificationBackoff  = 100 * time.Millisecond
)

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

// App coordinates the process-wide runtime owner, message workers, and task scheduler.
type App struct {
	Store     *store.Store
	Client    Client
	Scheduler Scheduler
	Service   Service
	Groups    GroupAuthorizer
	Logger    *slog.Logger

	MessageWorkers int
}

type loopResult struct {
	name string
	err  error
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
		runErr = errors.Join(runErr, lock.Close())
	}()
	if err := a.Store.RecoverInterrupted(ctx, lock); err != nil {
		return fmt.Errorf("recover interrupted tasks: %w", err)
	}
	return a.runLoops(ctx)
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
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}

	messages := make(chan tasksvc.Message, a.MessageWorkers*2)
	var workers sync.WaitGroup
	for range a.MessageWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.messageWorker(ctx, logger, messages)
		}()
	}

	results := make(chan loopResult, 2)
	go func() { results <- loopResult{name: "scheduler", err: a.Scheduler.Run(ctx)} }()
	go func() {
		err := a.Client.Run(ctx, func(handlerCtx context.Context, message onebot.GroupMessage) error {
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
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		results <- loopResult{name: "onebot", err: err}
	}()

	first := loopResult{}
	parentCanceled := false
	select {
	case <-parent.Done():
		parentCanceled = true
	case first = <-results:
	}
	cancel()

	// Client handlers can no longer enqueue after the client loop has joined.
	remaining := 2
	if first.name != "" {
		remaining--
	}
	for range remaining {
		<-results
	}
	close(messages)
	workers.Wait()

	if parentCanceled {
		return nil
	}
	if first.err == nil {
		return fmt.Errorf("%s loop stopped unexpectedly", first.name)
	}
	return fmt.Errorf("%s loop failed: %w", first.name, first.err)
}

func (a App) messageWorker(ctx context.Context, logger *slog.Logger, messages <-chan tasksvc.Message) {
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-messages:
			if !ok {
				return
			}
			if err := a.handleMessage(ctx, message); err != nil && ctx.Err() == nil {
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
