package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/gitwork"
	"qqcodex/internal/model"
	"qqcodex/internal/onebot"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
	"qqcodex/internal/tasksvc"
)

func TestEndToEnd(t *testing.T) {
	root := t.TempDir()
	repo, remote := createGitProject(t, root)
	worktreeRoot := filepath.Join(root, "worktrees")
	logRoot := filepath.Join(root, "logs")
	executionCount := filepath.Join(root, "executions")
	codexBinary := createFakeCodex(t, root)
	t.Setenv("CODEX_API_KEY", "codex-api-key-for-e2e")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	t.Setenv("QQCODEX_EXEC_COUNT", executionCount)
	if err := os.MkdirAll(os.Getenv("HOME"), 0o700); err != nil {
		t.Fatal(err)
	}

	oneBot := newFakeOneBot(t, "10000", "onebot-token-for-e2e")
	defer oneBot.Close()
	cfg := config.Config{
		OneBot:       config.OneBotConfig{URL: oneBot.URL(), AccessTokenEnv: "NAPCAT_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(root, "tasks.db"), LogDir: logRoot, WorktreeRoot: worktreeRoot, Consultation: config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90}, MessageWorkers: 2,
		AllowedGroupIDs: []string{"100"}, EmployeeIDs: []string{"200", "201"}, AdminIDs: []string{"201"},
		Codex:      config.CodexConfig{Binary: codexBinary, EnvironmentKeep: []string{"QQCODEX_EXEC_COUNT"}},
		OpsCommand: []string{"fake-ops"},
		Projects: []config.Project{{
			ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin",
			Checks: [][]string{{"git", "status", "--porcelain"}}, DeployAction: "deploy-rc",
			MaxConcurrent: 1, CodexTimeoutSeconds: 10, LogRetentionDays: 7,
		}},
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := openStore(t, cfg.DatabasePath)
	defer closeStore(t, db)
	logs, err := tasklog.Open(logRoot, []string{"onebot-token-for-e2e", "codex-api-key-for-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &codex.Runner{Binary: codexBinary, KeepEnv: cfg.Codex.EnvironmentKeep, LogDir: logRoot, Log: logs}
	worktrees := &gitwork.Manager{Root: worktreeRoot}
	operator := &e2eOperator{repo: repo, remote: remote}
	client := &onebot.Client{URL: oneBot.URL(), Token: "onebot-token-for-e2e", SelfID: "10000", MessageRunes: 1200}
	authorizer := auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs)
	scheduler := tasksvc.NewScheduler(registry, db, runner, worktrees, operator, client, logs, discardLogger(), 10*time.Millisecond)
	service := tasksvc.NewService(registry, db, authorizer, runner, scheduler, client, logs)
	application := App{
		Store: db, Client: client, Scheduler: scheduler, Service: service, Consultations: noopMessageService, Groups: authorizer,
		MessageWorkers: cfg.MessageWorkers, Logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()
	oneBot.WaitReady(t)

	const groupID = "100"
	taskID := tasksvc.TaskID(groupID, "1")
	oneBot.Send(t, groupEvent("1", groupID, "200", "10000", "[p] update e2e file", true))
	oneBot.WaitText(t, func(text string) bool {
		return strings.Contains(text, "任务 #"+taskID+" 规划完成") && strings.Contains(text, "确认 #"+taskID)
	})

	confirm := groupEvent("2", groupID, "200", "10000", "确认 #"+taskID, false)
	oneBot.Send(t, confirm)
	oneBot.Send(t, confirm)
	waitTaskStatus(t, db, taskID, model.StatusAwaitingMergeApproval)
	oneBot.WaitText(t, func(text string) bool { return strings.Contains(text, "批准合并 #"+taskID) })

	oneBot.Send(t, groupEvent("3", groupID, "201", "10000", "批准合并 #"+taskID, false))
	waitTaskStatus(t, db, taskID, model.StatusAwaitingDeployApproval)
	oneBot.WaitText(t, func(text string) bool { return strings.Contains(text, "批准部署 #"+taskID) })

	oneBot.Send(t, groupEvent("4", groupID, "201", "10000", "批准部署 #"+taskID, false))
	waitTaskStatus(t, db, taskID, model.StatusDeployed)

	task, err := db.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.TaskCommit == "" || task.RCCommit == "" || task.SessionID != "e2e-session" || task.Status != model.StatusDeployed {
		t.Fatalf("final task = %#v", task)
	}
	if operator.pushes.Load() != 1 || operator.merges.Load() != 1 || operator.deploys.Load() != 1 {
		t.Fatalf("external calls push=%d merge=%d deploy=%d, want once each", operator.pushes.Load(), operator.merges.Load(), operator.deploys.Load())
	}
	countData, err := os.ReadFile(executionCount)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(countData), "execute\n"); got != 1 {
		t.Fatalf("Codex executions = %d, want 1", got)
	}
	remoteCommit := gitOutput(t, repo, "rev-parse", "refs/remotes/origin/codex/"+taskID)
	if remoteCommit != task.TaskCommit {
		t.Fatalf("remote task commit = %q, want %q", remoteCommit, task.TaskCommit)
	}

	cancel()
	if err := waitError(t, done, "e2e shutdown"); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecoversBeforeLoopsAndHoldsRuntimeLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tasks.db")
	db := openStore(t, dbPath)
	defer closeStore(t, db)
	secondDB := openStore(t, dbPath)
	defer closeStore(t, secondDB)

	task := &model.Task{
		ID: "T-000000000001", ProjectID: "project", GroupID: "100", CreatorID: "200",
		Requirement: "test", Status: model.StatusRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	clientStarted := make(chan struct{})
	schedulerStarted := make(chan struct{})
	first := App{
		Store: db, Client: blockingClient{started: clientStarted}, Scheduler: blockingScheduler{started: schedulerStarted},
		Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	waitClosed(t, clientStarted, "client start")
	waitClosed(t, schedulerStarted, "scheduler start")

	recovered, err := db.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.StatusFailed {
		t.Fatalf("recovered status = %q, want %q", recovered.Status, model.StatusFailed)
	}

	secondClient := &countingClient{}
	secondScheduler := &countingScheduler{}
	err = (App{
		Store: secondDB, Client: secondClient, Scheduler: secondScheduler,
		Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	if !errors.Is(err, store.ErrRuntimeLocked) {
		t.Fatalf("second Run error = %v, want ErrRuntimeLocked", err)
	}
	if secondClient.calls.Load() != 0 || secondScheduler.calls.Load() != 0 {
		t.Fatalf("loops started before lock: client=%d scheduler=%d", secondClient.calls.Load(), secondScheduler.calls.Load())
	}

	cancel()
	if err := waitError(t, done, "first app shutdown"); err != nil {
		t.Fatal(err)
	}
}

func TestRunDoesNotStartLoopsWhenRecoveryFails(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	client := &countingClient{}
	scheduler := &countingScheduler{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (App{
		Store: db, Client: client, Scheduler: scheduler,
		Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want canceled recovery", err)
	}
	if client.calls.Load() != 0 || scheduler.calls.Load() != 0 {
		t.Fatalf("loops started after failed recovery: client=%d scheduler=%d", client.calls.Load(), scheduler.calls.Load())
	}
	lock, err := db.AcquireRuntimeLock()
	if err != nil {
		t.Fatalf("runtime lock remained held after recovery failure: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunFiltersGroupsAndRetriesOnlyNotificationDelivery(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)

	allowed := onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "300", Text: "hello", Mentioned: true}
	disallowed := allowed
	disallowed.GroupID = "999"
	disallowed.MessageID = "301"
	var mu sync.Mutex
	var got []tasksvc.Message
	thirdAttempt := make(chan struct{})
	service := serviceFunc(func(_ context.Context, message tasksvc.Message) error {
		mu.Lock()
		got = append(got, message)
		attempt := len(got)
		mu.Unlock()
		if attempt < 3 {
			return errors.Join(tasksvc.ErrNotificationDelivery, errors.New("send failed"))
		}
		close(thirdAttempt)
		return nil
	})
	client := emittingClient{messages: []onebot.GroupMessage{disallowed, allowed}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client, Scheduler: blockingScheduler{}, Service: noopMessageService, Consultations: service,
			Groups:         groupFunc(func(groupID string) bool { return groupID == "100" }),
			MessageWorkers: 1, Logger: discardLogger(),
		}).Run(ctx)
	}()
	waitClosed(t, thirdAttempt, "notification retry")
	cancel()
	if err := waitError(t, done, "app shutdown"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("service calls = %d, want 3", len(got))
	}
	want := tasksvc.Message{GroupID: "100", UserID: "200", MessageID: "300", Text: "hello", Mentioned: true}
	for i, message := range got {
		if message != want {
			t.Fatalf("attempt %d message = %#v, want immutable %#v", i+1, message, want)
		}
	}
}

func TestRunDoesNotRetryBusinessErrorsOrLogMessageText(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)

	const secretText = "message-full-secret"
	var calls atomic.Int32
	called := make(chan struct{})
	service := serviceFunc(func(context.Context, tasksvc.Message) error {
		calls.Add(1)
		close(called)
		return errors.New("invalid command contains " + secretText)
	})
	var logs strings.Builder
	logWritten := make(chan struct{})
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{writer: &logs, wrote: logWritten}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: emittingClient{messages: []onebot.GroupMessage{{GroupID: "100", UserID: "200", MessageID: "300", Text: secretText}}},
			Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService, Groups: groupFunc(func(string) bool { return true }),
			MessageWorkers: 1, Logger: logger,
		}).Run(ctx)
	}()
	waitClosed(t, called, "business error")
	waitClosed(t, logWritten, "business error log")
	cancel()
	if err := waitError(t, done, "app shutdown"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("service calls = %d, want 1", calls.Load())
	}
	if strings.Contains(logs.String(), secretText) {
		t.Fatalf("logs contain message/error text: %s", logs.String())
	}
}

func TestRunBoundsMessageWorkers(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)

	const workerCount = 2
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 8)
	service := serviceFunc(func(ctx context.Context, _ tasksvc.Message) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	messages := make([]onebot.GroupMessage, 8)
	for i := range messages {
		messages[i] = onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: onebot.ID(string(rune('1' + i)))}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: emittingClient{messages: messages}, Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: workerCount, Logger: discardLogger(),
		}).Run(ctx)
	}()
	for range workerCount {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	if got := maximum.Load(); got != workerCount {
		t.Fatalf("maximum workers = %d, want %d", got, workerCount)
	}
	close(release)
	cancel()
	if err := waitError(t, done, "worker shutdown"); err != nil {
		t.Fatal(err)
	}
}

func TestRunTreatsEarlySchedulerReturnAsFatal(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)

	clientStopped := make(chan struct{})
	err := (App{
		Store:     db,
		Client:    clientFunc(func(ctx context.Context, _ onebot.Handler) error { <-ctx.Done(); close(clientStopped); return nil }),
		Scheduler: schedulerFunc(func(context.Context) error { return nil }),
		Service:   serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scheduler") {
		t.Fatalf("Run error = %v, want scheduler fatal error", err)
	}
	waitClosed(t, clientStopped, "client cancellation")
}

func TestRunTreatsEarlyOneBotReturnAsFatal(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)

	schedulerStopped := make(chan struct{})
	err := (App{
		Store:     db,
		Client:    clientFunc(func(context.Context, onebot.Handler) error { return errors.New("invalid onebot configuration") }),
		Scheduler: schedulerFunc(func(ctx context.Context) error { <-ctx.Done(); close(schedulerStopped); return nil }),
		Service:   serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "onebot") {
		t.Fatalf("Run error = %v, want OneBot fatal error", err)
	}
	waitClosed(t, schedulerStopped, "scheduler cancellation")
}

func TestRunDrainsAcceptedMessagesAfterCancellation(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	const total = 8
	accepted := make(chan struct{}, total)
	client := emittingClient{messages: make([]onebot.GroupMessage, total), accepted: accepted}
	for i := range client.messages {
		client.messages[i] = onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: onebot.ID(fmt.Sprint(i + 1))}
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	service := serviceFunc(func(_ context.Context, message tasksvc.Message) error {
		mu.Lock()
		seen[message.MessageID]++
		mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client, Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
		}).Run(ctx)
	}()
	for range total {
		waitClosed(t, accepted, "accepted message")
	}
	cancel()
	if err := waitError(t, done, "drained app shutdown"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Fatalf("processed IDs = %d, want %d (%v)", len(seen), total, seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("message %s processed %d times", id, count)
		}
	}
}

func TestRunStopsOnFatalStoreError(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	called := make(chan struct{})
	clientStopped := make(chan struct{})
	service := serviceFunc(func(context.Context, tasksvc.Message) error {
		close(called)
		return errors.Join(tasksvc.ErrFatalStore, errors.New("database failed"))
	})
	err := (App{
		Store: db,
		Client: clientFunc(func(ctx context.Context, handler onebot.Handler) error {
			if err := handler(ctx, onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "1"}); err != nil {
				return err
			}
			<-ctx.Done()
			close(clientStopped)
			return nil
		}),
		Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService, Groups: groupFunc(func(string) bool { return true }),
		MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	waitClosed(t, called, "fatal service error")
	if err == nil || !errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("Run error = %v, want ErrFatalStore", err)
	}
	waitClosed(t, clientStopped, "fatal client cancellation")
}

func TestRunReturnsFatalStoreDiscoveredWhileDrainingAfterCancellation(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	accepted := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	service := serviceFunc(func(context.Context, tasksvc.Message) error {
		if calls.Add(1) == 1 {
			<-releaseFirst
			return nil
		}
		return errors.Join(tasksvc.ErrFatalStore, errors.New("drain storage failed"))
	})
	client := emittingClient{
		messages: []onebot.GroupMessage{
			{GroupID: "100", UserID: "200", MessageID: "1"},
			{GroupID: "100", UserID: "200", MessageID: "2"},
		},
		accepted: accepted,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client, Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
		}).Run(ctx)
	}()
	waitClosed(t, accepted, "first accepted message")
	waitClosed(t, accepted, "second accepted message")
	cancel()
	close(releaseFirst)
	err := waitError(t, done, "fatal drain shutdown")
	if !errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("Run error = %v, want drain ErrFatalStore", err)
	}
}

func TestRunReturnsFatalStoreFromJoinedScheduler(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	err := (App{
		Store: db,
		Client: clientFunc(func(context.Context, onebot.Handler) error {
			return nil
		}),
		Scheduler: schedulerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return errors.Join(tasksvc.ErrFatalStore, errors.New("scheduler store failed"))
		}),
		Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	if !errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("Run error = %v, want joined scheduler ErrFatalStore", err)
	}
}

func TestRunTreatsParentCancellationAsNormalWhenLoopResultWinsSelect(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	returnClient := make(chan struct{})
	schedulerCanceled := make(chan struct{})
	releaseScheduler := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db,
			Client: clientFunc(func(context.Context, onebot.Handler) error {
				<-returnClient
				return nil
			}),
			Scheduler: schedulerFunc(func(ctx context.Context) error {
				<-ctx.Done()
				close(schedulerCanceled)
				<-releaseScheduler
				return nil
			}),
			Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
		}).Run(ctx)
	}()
	close(returnClient)
	waitClosed(t, schedulerCanceled, "scheduler loop cancellation")
	cancel()
	close(releaseScheduler)
	if err := waitError(t, done, "parent cancellation race"); err != nil {
		t.Fatalf("Run after parent cancellation = %v, want nil", err)
	}
}

func TestRunShutdownTimeoutCancelsInFlightWorker(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	accepted := make(chan struct{}, 1)
	started := make(chan struct{})
	canceled := make(chan struct{})
	client := emittingClient{messages: []onebot.GroupMessage{{GroupID: "100", UserID: "200", MessageID: "1"}}, accepted: accepted}
	service := serviceFunc(func(ctx context.Context, _ tasksvc.Message) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client, Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
			ShutdownTimeout: 40 * time.Millisecond,
		}).Run(ctx)
	}()
	waitClosed(t, accepted, "accepted message")
	waitClosed(t, started, "worker start")
	startedShutdown := time.Now()
	cancel()
	err := waitError(t, done, "bounded shutdown")
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run error = %v, want shutdown timeout", err)
	}
	if elapsed := time.Since(startedShutdown); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
	waitClosed(t, canceled, "worker cancellation")
}

func TestRunShutdownTimeoutReturnsWhenServiceIgnoresCancellationAndRetainsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db := openStore(t, path)
	accepted := make(chan struct{}, 1)
	started := make(chan struct{})
	client := emittingClient{messages: []onebot.GroupMessage{{GroupID: "100", UserID: "200", MessageID: "1"}}, accepted: accepted}
	service := serviceFunc(func(context.Context, tasksvc.Message) error {
		close(started)
		select {}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client, Scheduler: blockingScheduler{}, Service: service, Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
			ShutdownTimeout: 30 * time.Millisecond,
		}).Run(ctx)
	}()
	waitClosed(t, accepted, "accepted message")
	waitClosed(t, started, "blocking service start")
	startedShutdown := time.Now()
	cancel()
	err := waitError(t, done, "bounded service shutdown")
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run error = %v, want shutdown timeout", err)
	}
	if elapsed := time.Since(startedShutdown); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded service shutdown took %s", elapsed)
	}
	closeStore(t, db)
	assertRuntimeLockRetained(t, path)
}

func TestRunShutdownTimeoutReturnsWhenLoopsIgnoreCancellationAndRetainsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db := openStore(t, path)
	defer closeStore(t, db)
	clientStarted := make(chan struct{})
	schedulerStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db,
			Client: clientFunc(func(context.Context, onebot.Handler) error {
				close(clientStarted)
				select {}
			}),
			Scheduler: schedulerFunc(func(context.Context) error {
				close(schedulerStarted)
				select {}
			}),
			Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
			Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
			ShutdownTimeout: 30 * time.Millisecond,
		}).Run(ctx)
	}()
	waitClosed(t, clientStarted, "blocking client start")
	waitClosed(t, schedulerStarted, "blocking scheduler start")
	startedShutdown := time.Now()
	cancel()
	err := waitError(t, done, "bounded loop shutdown")
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run error = %v, want shutdown timeout", err)
	}
	if elapsed := time.Since(startedShutdown); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded loop shutdown took %s", elapsed)
	}
	assertRuntimeLockRetained(t, path)
}

func TestRunFatalStoreCancelsOtherWorkerBeforeSlowLoopsExit(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	allAccepted := make(chan struct{})
	secondStarted := make(chan struct{})
	secondCanceled := make(chan struct{})
	queuedProcessed := make(chan struct{})
	loopsCanceled := make(chan struct{}, 2)
	releaseLoops := make(chan struct{})
	client := clientFunc(func(ctx context.Context, handler onebot.Handler) error {
		if err := handler(ctx, onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "fatal"}); err != nil {
			return err
		}
		if err := handler(ctx, onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "second"}); err != nil {
			return err
		}
		if err := handler(ctx, onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "queued"}); err != nil {
			return err
		}
		close(allAccepted)
		<-ctx.Done()
		loopsCanceled <- struct{}{}
		<-releaseLoops
		return nil
	})
	service := serviceFunc(func(ctx context.Context, message tasksvc.Message) error {
		if message.MessageID == "fatal" {
			<-secondStarted
			<-allAccepted
			return errors.Join(tasksvc.ErrFatalStore, errors.New("store failed"))
		}
		if message.MessageID == "queued" {
			close(queuedProcessed)
			return nil
		}
		close(secondStarted)
		<-ctx.Done()
		close(secondCanceled)
		return ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		done <- (App{
			Store: db, Client: client,
			Scheduler: schedulerFunc(func(ctx context.Context) error {
				<-ctx.Done()
				loopsCanceled <- struct{}{}
				<-releaseLoops
				return nil
			}),
			Service: service, Consultations: noopMessageService, Groups: groupFunc(func(string) bool { return true }),
			MessageWorkers: 2, Logger: discardLogger(), ShutdownTimeout: time.Second,
		}).Run(context.Background())
	}()
	waitClosed(t, secondStarted, "second worker start")
	for range 2 {
		select {
		case <-loopsCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("loops did not observe fatal cancellation")
		}
	}
	waitClosed(t, secondCanceled, "other worker immediate cancellation")
	close(releaseLoops)
	if err := waitError(t, done, "fatal shutdown"); !errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("Run error = %v, want fatal store", err)
	}
	select {
	case <-queuedProcessed:
		t.Fatal("queued message was processed after fatal store error")
	default:
	}
}

func TestMessageWorkerFatalCancelsIdleWorkerBeforeQueuedDispatch(t *testing.T) {
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	fatal := newFatalState(cancelWorkers)
	messages := make(chan tasksvc.Message, 2)
	queuedProcessed := make(chan struct{})
	service := serviceFunc(func(_ context.Context, message tasksvc.Message) error {
		if message.MessageID == "fatal" {
			return errors.Join(tasksvc.ErrFatalStore, errors.New("store failed"))
		}
		close(queuedProcessed)
		return nil
	})
	a := App{Service: service, Consultations: noopMessageService}
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.messageWorker(workerCtx, discardLogger(), messages, fatal.report)
		}()
	}
	messages <- tasksvc.Message{MessageID: "fatal"}
	waitClosed(t, fatal.signal, "synchronous fatal report")
	if workerCtx.Err() == nil {
		t.Fatal("fatal was signaled before worker cancellation")
	}
	// The other worker is idle and the queued message becomes ready only after
	// fatal reporting has synchronously canceled the shared worker context.
	messages <- tasksvc.Message{MessageID: "queued"}
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	waitClosed(t, workersDone, "fatal workers")
	select {
	case <-queuedProcessed:
		t.Fatal("idle worker dispatched a queued message after fatal reporting")
	default:
	}
}

func TestRunShutdownTimeoutPreservesReadySchedulerFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db := openStore(t, path)
	releaseClient := make(chan struct{})
	err := (App{
		Store: db,
		Client: clientFunc(func(context.Context, onebot.Handler) error {
			<-releaseClient
			return nil
		}),
		Scheduler: schedulerFunc(func(context.Context) error {
			return errors.Join(tasksvc.ErrFatalStore, errors.New("scheduler store failed"))
		}),
		Service: serviceFunc(func(context.Context, tasksvc.Message) error { return nil }), Consultations: noopMessageService,
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
		ShutdownTimeout: 30 * time.Millisecond,
	}).Run(context.Background())
	if !errors.Is(err, tasksvc.ErrFatalStore) || !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run error = %v, want fatal store joined with shutdown timeout", err)
	}
	close(releaseClient)
	closeStore(t, db)
	assertRuntimeLockRetained(t, path)
}

func TestCleanupExpiredRemovesOnlyOldFailedAndCancelledLocalArtifacts(t *testing.T) {
	root := t.TempDir()
	repo, remote := createGitProject(t, root)
	worktreeRoot := filepath.Join(root, "worktrees")
	logRoot := filepath.Join(root, "logs")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	project := config.Project{
		ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin",
		Checks: [][]string{{"git", "status"}}, DeployAction: "deploy", MaxConcurrent: 1,
		CodexTimeoutSeconds: 30, LogRetentionDays: 7,
	}
	registry, err := config.NewRegistry(config.Config{
		OneBot:       config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "TOKEN", SelfID: "1", MessageRunes: 100},
		DatabasePath: filepath.Join(root, "tasks.db"), LogDir: logRoot, WorktreeRoot: worktreeRoot, Consultation: config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90}, MessageWorkers: 1,
		AllowedGroupIDs: []string{"1"}, EmployeeIDs: []string{"2"}, AdminIDs: []string{"2"},
		Codex: config.CodexConfig{Binary: "unused"}, OpsCommand: []string{"unused"}, Projects: []config.Project{project},
	})
	if err != nil {
		t.Fatal(err)
	}
	db := openStore(t, filepath.Join(root, "tasks.db"))
	defer closeStore(t, db)
	logs, err := tasklog.Open(logRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	worktrees := &gitwork.Manager{Root: worktreeRoot}
	cases := []struct {
		id      string
		status  model.Status
		updated time.Time
		cleaned bool
	}{
		{"T-000000000101", model.StatusFailed, now.Add(-8 * 24 * time.Hour), true},
		{"T-000000000102", model.StatusCancelled, now.Add(-9 * 24 * time.Hour), true},
		{"T-000000000103", model.StatusFailed, now.Add(-6 * 24 * time.Hour), false},
		{"T-000000000104", model.StatusQueued, now.Add(-30 * 24 * time.Hour), false},
		{"T-000000000105", model.StatusMergeConflict, now.Add(-30 * 24 * time.Hour), false},
		{"T-000000000106", model.StatusDeployed, now.Add(-30 * 24 * time.Hour), false},
	}
	artifacts := make(map[string]gitwork.Prepared, len(cases))
	for _, tc := range cases {
		prepared, err := worktrees.Prepare(context.Background(), project, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[tc.id] = prepared
		task := &model.Task{
			ID: tc.id, ProjectID: project.ID, GroupID: "1", CreatorID: "2", Requirement: "cleanup",
			Status: tc.status, Branch: prepared.Branch, Worktree: prepared.Path, BaseCommit: prepared.BaseCommit,
			GitCommonDir: prepared.GitCommonDir, CreatedAt: tc.updated.Add(-time.Hour), UpdatedAt: tc.updated,
		}
		if err := db.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		if err := logs.Append(tc.id, "test", []byte("artifact")); err != nil {
			t.Fatal(err)
		}
	}
	oldFailed := artifacts[cases[0].id]
	if err := db.AppendAudit(context.Background(), cases[0].id, "cleanup_test", "retained", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, repo, "git", "push", "origin", oldFailed.Branch)
	remoteBefore := gitOutput(t, repo, "ls-remote", "--refs", remote, "refs/heads/"+oldFailed.Branch)

	err = (App{Store: db, Projects: registry, Worktrees: worktrees, Logs: logs, Now: func() time.Time { return now }}).CleanupExpired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		prepared := artifacts[tc.id]
		_, worktreeErr := os.Stat(prepared.Path)
		_, logErr := os.Stat(filepath.Join(logRoot, tc.id+".log"))
		if tc.cleaned {
			if !errors.Is(worktreeErr, os.ErrNotExist) || !errors.Is(logErr, os.ErrNotExist) {
				t.Errorf("cleaned task %s artifacts: worktree=%v log=%v", tc.id, worktreeErr, logErr)
			}
		} else if worktreeErr != nil || logErr != nil {
			t.Errorf("retained task %s artifacts: worktree=%v log=%v", tc.id, worktreeErr, logErr)
		}
		if _, err := db.GetTask(context.Background(), tc.id); err != nil {
			t.Errorf("task row %s was removed: %v", tc.id, err)
		}
	}
	if remoteAfter := gitOutput(t, repo, "ls-remote", "--refs", remote, "refs/heads/"+oldFailed.Branch); remoteAfter != remoteBefore {
		t.Fatalf("cleanup changed remote task branch: before=%q after=%q", remoteBefore, remoteAfter)
	}
	rawDB, err := sql.Open("sqlite", filepath.Join(root, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var auditCount int
	if err := rawDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE task_id = ?`, cases[0].id).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("retained audit rows = %d, want 1", auditCount)
	}
}

func TestCleanupExpiredKeepsLogOnWorktreeFailureAndRetriesAfterLogFailure(t *testing.T) {
	root := t.TempDir()
	repo, _ := createGitProject(t, root)
	project := config.Project{ID: "project", RepoPath: repo, LogRetentionDays: 7}
	now := time.Now().UTC()
	db := openStore(t, filepath.Join(root, "tasks.db"))
	defer closeStore(t, db)
	task := &model.Task{
		ID: "T-000000000201", ProjectID: project.ID, GroupID: "1", CreatorID: "2", Requirement: "cleanup",
		Status: model.StatusFailed, Worktree: filepath.Join(root, "worktrees", "T-000000000201"),
		CreatedAt: now.Add(-9 * 24 * time.Hour), UpdatedAt: now.Add(-8 * 24 * time.Hour),
	}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	logCalls := 0
	app := App{
		Store: db, Projects: projectLookupFunc(func(string) (config.Project, bool) { return project, true }),
		Worktrees: worktreeRemoveFunc(func(context.Context, string, string) error { return errors.New("remove failed") }),
		Logs:      taskLogRemoveFunc(func(string) error { logCalls++; return nil }), Now: func() time.Time { return now },
	}
	if err := app.CleanupExpired(context.Background()); err == nil || !strings.Contains(err.Error(), task.ID) {
		t.Fatalf("CleanupExpired error = %v, want task ID", err)
	}
	if logCalls != 0 {
		t.Fatalf("log removal calls = %d, want 0 after worktree failure", logCalls)
	}

	worktreeRoot := filepath.Join(root, "retry-worktrees")
	manager := &gitwork.Manager{Root: worktreeRoot}
	project.BaseBranch, project.RCBranch, project.Remote = "main", "rc", "origin"
	prepared, err := manager.Prepare(context.Background(), project, "T-000000000202")
	if err != nil {
		t.Fatal(err)
	}
	retryTask := *task
	retryTask.ID = "T-000000000202"
	retryTask.Worktree = prepared.Path
	retryDB := openStore(t, filepath.Join(root, "retry.db"))
	defer closeStore(t, retryDB)
	if err := retryDB.CreateTask(context.Background(), &retryTask); err != nil {
		t.Fatal(err)
	}
	removeLogCalls := 0
	retry := App{
		Store: retryDB, Projects: projectLookupFunc(func(string) (config.Project, bool) { return project, true }), Worktrees: manager,
		Logs: taskLogRemoveFunc(func(string) error {
			removeLogCalls++
			if removeLogCalls == 1 {
				return errors.New("log unavailable")
			}
			return nil
		}),
		Now: func() time.Time { return now },
	}
	if err := retry.CleanupExpired(context.Background()); err == nil || !strings.Contains(err.Error(), retryTask.ID) {
		t.Fatalf("first retry cleanup error = %v", err)
	}
	if _, err := os.Stat(prepared.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first cleanup retained worktree: %v", err)
	}
	if err := retry.CleanupExpired(context.Background()); err != nil {
		t.Fatalf("second retry cleanup = %v", err)
	}
	if removeLogCalls != 2 {
		t.Fatalf("log removal calls = %d, want 2", removeLogCalls)
	}
}

func TestCleanupExpiredStopsBeforeNextLogAfterCancellation(t *testing.T) {
	now := time.Now().UTC()
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	project := config.Project{ID: "project", RepoPath: "/unused", LogRetentionDays: 7}
	for i, id := range []string{"T-000000000301", "T-000000000302"} {
		updated := now.Add(time.Duration(-9+i) * 24 * time.Hour)
		if err := db.CreateTask(context.Background(), &model.Task{
			ID: id, ProjectID: project.ID, GroupID: "1", CreatorID: "2", Requirement: "cleanup",
			Status: model.StatusFailed, CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var logCalls []string
	err := (App{
		Store: db, Projects: projectLookupFunc(func(string) (config.Project, bool) { return project, true }),
		Worktrees: worktreeRemoveFunc(func(context.Context, string, string) error { t.Fatal("empty worktree was removed"); return nil }),
		Logs: taskLogRemoveFunc(func(taskID string) error {
			logCalls = append(logCalls, taskID)
			cancel()
			return nil
		}),
		Now: func() time.Time { return now },
	}).CleanupExpired(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CleanupExpired error = %v, want context canceled", err)
	}
	if want := []string{"T-000000000301"}; !slices.Equal(logCalls, want) {
		t.Fatalf("log calls = %v, want %v", logCalls, want)
	}
}

func TestCleanupExpiredStopsAfterWorktreeReturnsCancellation(t *testing.T) {
	now := time.Now().UTC()
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	project := config.Project{ID: "project", RepoPath: "/unused", LogRetentionDays: 7}
	for i, id := range []string{"T-000000000311", "T-000000000312"} {
		updated := now.Add(time.Duration(-9+i) * 24 * time.Hour)
		if err := db.CreateTask(context.Background(), &model.Task{
			ID: id, ProjectID: project.ID, GroupID: "1", CreatorID: "2", Requirement: "cleanup",
			Status: model.StatusFailed, Worktree: "/unused/" + id,
			CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var worktreeCalls []string
	logCalls := 0
	err := (App{
		Store: db, Projects: projectLookupFunc(func(string) (config.Project, bool) { return project, true }),
		Worktrees: worktreeRemoveFunc(func(_ context.Context, _ string, worktree string) error {
			worktreeCalls = append(worktreeCalls, worktree)
			cancel()
			return context.Canceled
		}),
		Logs: taskLogRemoveFunc(func(string) error { logCalls++; return nil }),
		Now:  func() time.Time { return now },
	}).CleanupExpired(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CleanupExpired error = %v, want context canceled", err)
	}
	if want := []string{"/unused/T-000000000311"}; !slices.Equal(worktreeCalls, want) {
		t.Fatalf("worktree calls = %v, want %v", worktreeCalls, want)
	}
	if logCalls != 0 {
		t.Fatalf("log calls = %d, want 0", logCalls)
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) || !slices.Equal(cleanupErr.TaskIDs(), []string{"T-000000000311"}) {
		t.Fatalf("cleanup error = %#v", cleanupErr)
	}
}

func TestCleanupErrorIsSafeSortedDeduplicatedAndImmutable(t *testing.T) {
	cause := errors.New("secret failure at /private/company/repository")
	err := newCleanupError([]string{
		"T-000000000322", "../../private", "T-000000000321", "T-000000000322",
	}, cause)
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error type = %T, want *CleanupError", err)
	}
	if got, want := cleanupErr.Error(), "cleanup failed for tasks: T-000000000321, T-000000000322"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if strings.Contains(cleanupErr.Error(), "secret") || strings.Contains(cleanupErr.Error(), "private") || !errors.Is(cleanupErr, cause) {
		t.Fatalf("unsafe or unwrapped cleanup error: %v", cleanupErr)
	}
	ids := cleanupErr.TaskIDs()
	ids[0] = "changed"
	if got := cleanupErr.TaskIDs(); !slices.Equal(got, []string{"T-000000000321", "T-000000000322"}) {
		t.Fatalf("TaskIDs mutated through caller: %v", got)
	}

	contextOnly := newCleanupError(nil, context.Canceled)
	var contextCleanup *CleanupError
	if !errors.Is(contextOnly, context.Canceled) || !errors.As(contextOnly, &contextCleanup) || len(contextCleanup.TaskIDs()) != 0 || contextCleanup.Error() != "cleanup failed" {
		t.Fatalf("context-only cleanup error = %#v", contextOnly)
	}
}

func assertRuntimeLockRetained(t *testing.T, path string) {
	t.Helper()
	second := openStore(t, path)
	defer closeStore(t, second)
	if lock, err := second.AcquireRuntimeLock(); !errors.Is(err, store.ErrRuntimeLocked) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("runtime lock after timeout = %v, want ErrRuntimeLocked", err)
	}
}

type fakeOneBot struct {
	server   *httptest.Server
	selfID   string
	token    string
	events   chan json.RawMessage
	messages chan string
	ready    chan struct{}
	readyOne sync.Once
}

func newFakeOneBot(t *testing.T, selfID, token string) *fakeOneBot {
	t.Helper()
	fake := &fakeOneBot{
		selfID: selfID, token: token, events: make(chan json.RawMessage, 32),
		messages: make(chan string, 64), ready: make(chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	return fake
}

func (f *fakeOneBot) URL() string { return "ws" + strings.TrimPrefix(f.server.URL, "http") }

func (f *fakeOneBot) Close() { f.server.Close() }

func (f *fakeOneBot) WaitReady(t *testing.T) { waitClosed(t, f.ready, "OneBot connection") }

func (f *fakeOneBot) Send(t *testing.T, event json.RawMessage) {
	t.Helper()
	select {
	case f.events <- event:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending OneBot event")
	}
}

func (f *fakeOneBot) WaitText(t *testing.T, match func(string) bool) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case text := <-f.messages:
			if match(text) {
				return text
			}
		case <-timer.C:
			t.Fatal("timed out waiting for OneBot notification")
			return ""
		}
	}
}

func (f *fakeOneBot) serve(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(response, request, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	f.readyOne.Do(func() { close(f.ready) })

	requests := make(chan onebot.ActionRequest)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var action onebot.ActionRequest
			if err := wsjson.Read(request.Context(), conn, &action); err != nil {
				return
			}
			select {
			case requests <- action:
			case <-request.Context().Done():
				return
			}
		}
	}()
	for {
		select {
		case raw := <-f.events:
			if err := wsjson.Write(request.Context(), conn, raw); err != nil {
				return
			}
		case action := <-requests:
			text := actionText(action)
			select {
			case f.messages <- text:
			case <-request.Context().Done():
				return
			}
			result := onebot.ActionResponse{Status: "ok", RetCode: 0, Echo: action.Echo}
			if err := wsjson.Write(request.Context(), conn, result); err != nil {
				return
			}
		case <-readerDone:
			return
		case <-request.Context().Done():
			return
		}
	}
}

func actionText(action onebot.ActionRequest) string {
	var result strings.Builder
	for _, segment := range action.Params.Message {
		if segment.Type != "text" {
			continue
		}
		var text string
		if json.Unmarshal(segment.Data["text"], &text) == nil {
			result.WriteString(text)
		}
	}
	return result.String()
}

func groupEvent(messageID, groupID, userID, selfID, text string, mentioned bool) json.RawMessage {
	segments := []map[string]any{}
	if mentioned {
		segments = append(segments, map[string]any{"type": "at", "data": map[string]string{"qq": selfID}})
	}
	segments = append(segments, map[string]any{"type": "text", "data": map[string]string{"text": text}})
	event := map[string]any{
		"post_type": "message", "message_type": "group", "message_id": messageID,
		"group_id": groupID, "user_id": userID, "self_id": selfID, "message": segments,
	}
	data, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return data
}

type e2eOperator struct {
	repo, remote            string
	pushes, merges, deploys atomic.Int32
}

func (o *e2eOperator) Sync(ctx context.Context, _ string) error {
	return runGitCommand(ctx, o.repo, "fetch", "origin")
}

func (o *e2eOperator) PushTask(ctx context.Context, _, taskID, branch, commit string) error {
	if gitCommandOutput(ctx, o.repo, "rev-parse", branch) != commit {
		return errors.New("task commit does not match branch")
	}
	if err := runGitCommand(ctx, o.repo, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return err
	}
	if err := runGitCommand(ctx, o.repo, "fetch", "origin"); err != nil {
		return err
	}
	o.pushes.Add(1)
	return nil
}

func (o *e2eOperator) MergeRC(ctx context.Context, _, taskID, commit string) (string, error) {
	root, err := os.MkdirTemp("", "qqcodex-e2e-merge-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := runCommand(ctx, "", "git", "clone", o.remote, root); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"config", "user.name", "QQ Codex E2E"},
		{"config", "user.email", "qqcodex-e2e@example.invalid"},
		{"checkout", "-b", "rc", "origin/rc"},
		{"merge", "--no-ff", commit, "-m", "merge " + taskID},
		{"push", "origin", "refs/heads/rc:refs/heads/rc"},
	} {
		if err := runGitCommand(ctx, root, args...); err != nil {
			return "", err
		}
	}
	rcCommit := gitCommandOutput(ctx, root, "rev-parse", "HEAD")
	if len(rcCommit) != 40 {
		return "", errors.New("invalid merged commit")
	}
	o.merges.Add(1)
	return rcCommit, nil
}

func (o *e2eOperator) DeployRC(context.Context, string, string, string) error {
	o.deploys.Add(1)
	return nil
}

func createGitProject(t *testing.T, root string) (string, string) {
	t.Helper()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runTestCommand(t, "", "git", "init", "--bare", remote)
	runTestCommand(t, "", "git", "init", "-b", "main", repo)
	runTestCommand(t, repo, "git", "config", "user.name", "QQ Codex E2E")
	runTestCommand(t, repo, "git", "config", "user.email", "qqcodex-e2e@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, repo, "git", "add", "README.md")
	runTestCommand(t, repo, "git", "commit", "-m", "base")
	runTestCommand(t, repo, "git", "remote", "add", "origin", remote)
	runTestCommand(t, repo, "git", "push", "-u", "origin", "main")
	runTestCommand(t, repo, "git", "branch", "rc")
	runTestCommand(t, repo, "git", "push", "origin", "rc")
	return repo, remote
}

func createFakeCodex(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "fake-codex")
	script := `#!/bin/sh
set -eu
last=''
schema=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; last="$1" ;;
    --output-schema) shift; schema="$1" ;;
  esac
  shift
done
if [ -n "$schema" ]; then
  final='{"summary":"e2e plan","scope":["repository"],"checks":["git status"],"risks":[]}'
  session='planning-session'
else
  printf 'changed by fake codex\n' > qqcodex-e2e.txt
  git add qqcodex-e2e.txt
  git -c user.name='QQ Codex E2E' -c user.email='qqcodex-e2e@example.invalid' commit -m 'implement e2e task' >/dev/null 2>&1
  printf 'execute\n' >> "$QQCODEX_EXEC_COUNT"
  final='implementation complete'
  session='e2e-session'
fi
printf '%s' "$final" > "$last"
printf '{"type":"thread.started","thread_id":"%s"}\n' "$session"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitTaskStatus(t *testing.T, db *store.Store, taskID string, want model.Status) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var last model.Status
	for {
		task, err := db.GetTask(context.Background(), taskID)
		if err == nil {
			last = task.Status
			if task.Status == want {
				return
			}
			if task.Status == model.StatusFailed || task.Status == model.StatusMergeConflict || task.Status == model.StatusDeployFailed {
				t.Fatalf("task reached %q while waiting for %q: %s", task.Status, want, task.Failure)
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for task %s status %q; last=%q", taskID, want, last)
		}
	}
}

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func runTestCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	if err := runCommand(context.Background(), dir, name, args...); err != nil {
		t.Fatal(err)
	}
}

func runGitCommand(ctx context.Context, repo string, args ...string) error {
	return runCommand(ctx, repo, "git", args...)
}

func gitCommandOutput(ctx context.Context, repo string, args ...string) string {
	cmdArgs := append([]string{"-C", repo}, args...)
	output, err := exec.CommandContext(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

type serviceFunc func(context.Context, tasksvc.Message) error

func (f serviceFunc) Handle(ctx context.Context, message tasksvc.Message) error {
	return f(ctx, message)
}

var noopMessageService = serviceFunc(func(context.Context, tasksvc.Message) error { return nil })

type groupFunc func(string) bool

func (f groupFunc) AllowedGroup(groupID string) bool { return f(groupID) }

type projectLookupFunc func(string) (config.Project, bool)

func (f projectLookupFunc) ProjectByID(projectID string) (config.Project, bool) {
	return f(projectID)
}

type worktreeRemoveFunc func(context.Context, string, string) error

func (f worktreeRemoveFunc) Remove(ctx context.Context, repoPath, worktree string) error {
	return f(ctx, repoPath, worktree)
}

type taskLogRemoveFunc func(string) error

func (f taskLogRemoveFunc) Remove(taskID string) error { return f(taskID) }

type clientFunc func(context.Context, onebot.Handler) error

func (f clientFunc) Run(ctx context.Context, handler onebot.Handler) error { return f(ctx, handler) }

type schedulerFunc func(context.Context) error

func (f schedulerFunc) Run(ctx context.Context) error { return f(ctx) }

type blockingClient struct{ started chan struct{} }

func (c blockingClient) Run(ctx context.Context, _ onebot.Handler) error {
	if c.started != nil {
		close(c.started)
	}
	<-ctx.Done()
	return nil
}

type blockingScheduler struct{ started chan struct{} }

func (s blockingScheduler) Run(ctx context.Context) error {
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

type countingClient struct{ calls atomic.Int32 }

func (c *countingClient) Run(ctx context.Context, _ onebot.Handler) error {
	c.calls.Add(1)
	<-ctx.Done()
	return nil
}

type countingScheduler struct{ calls atomic.Int32 }

func (s *countingScheduler) Run(ctx context.Context) error {
	s.calls.Add(1)
	<-ctx.Done()
	return nil
}

type emittingClient struct {
	messages []onebot.GroupMessage
	accepted chan<- struct{}
}

func (c emittingClient) Run(ctx context.Context, handler onebot.Handler) error {
	for _, message := range c.messages {
		if err := handler(ctx, message); err != nil {
			return err
		}
		if c.accepted != nil {
			c.accepted <- struct{}{}
		}
	}
	<-ctx.Done()
	return nil
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
	wrote  chan struct{}
	once   sync.Once
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(data)
	if w.wrote != nil {
		w.once.Do(func() { close(w.wrote) })
	}
	return n, err
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func closeStore(t *testing.T, db *store.Store) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Errorf("close store: %v", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitClosed(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitError(t *testing.T, ch <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
