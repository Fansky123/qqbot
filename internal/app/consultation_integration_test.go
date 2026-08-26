package app

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/consultsvc"
	"qqcodex/internal/onebot"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
	"qqcodex/internal/tasksvc"
)

func TestHandleMessageRoutesConsultationsAndTasks(t *testing.T) {
	tests := []struct {
		name             string
		text             string
		mentioned        bool
		wantTasks        int
		wantConsultation int
	}{
		{name: "generic consultation", text: "怎么排查这个问题？", mentioned: true, wantConsultation: 1},
		{name: "project consultation", text: "问 [p] 这个模块做什么？", mentioned: true, wantConsultation: 1},
		{name: "create task", text: "[p] 修复构建", mentioned: true, wantTasks: 1},
		{name: "fixed task command", text: "状态 #T-012345ABCDEF", wantTasks: 1},
		{name: "malformed consultation", text: "问 [p]", mentioned: true, wantTasks: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var taskCalls, consultationCalls int
			a := App{
				Service: serviceFunc(func(context.Context, tasksvc.Message) error { taskCalls++; return nil }),
				Consultations: serviceFunc(func(context.Context, tasksvc.Message) error {
					consultationCalls++
					return nil
				}),
			}
			err := a.handleMessage(context.Background(), tasksvc.Message{Text: tt.text, Mentioned: tt.mentioned})
			if err != nil {
				t.Fatal(err)
			}
			if taskCalls != tt.wantTasks || consultationCalls != tt.wantConsultation {
				t.Fatalf("handler calls tasks=%d consultation=%d, want %d/%d", taskCalls, consultationCalls, tt.wantTasks, tt.wantConsultation)
			}
		})
	}
}

func TestHandleMessageRetriesConsultationNotificationsOnly(t *testing.T) {
	var calls int
	a := App{
		Service: serviceFunc(func(context.Context, tasksvc.Message) error {
			t.Fatal("task handler called")
			return nil
		}),
		Consultations: serviceFunc(func(context.Context, tasksvc.Message) error {
			calls++
			if calls < notificationAttempts {
				return errors.Join(tasksvc.ErrNotificationDelivery, errors.New("send failed"))
			}
			return nil
		}),
	}
	message := tasksvc.Message{Text: "帮我解释一下", Mentioned: true}
	if err := a.handleMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if calls != notificationAttempts {
		t.Fatalf("consultation calls = %d, want %d", calls, notificationAttempts)
	}

	for _, sentinel := range []error{errors.New("business failure"), tasksvc.ErrFatalStore} {
		calls = 0
		a.Consultations = serviceFunc(func(context.Context, tasksvc.Message) error {
			calls++
			return sentinel
		})
		err := a.handleMessage(context.Background(), message)
		if !errors.Is(err, sentinel) || calls != 1 {
			t.Fatalf("error = %v calls=%d, want %v once", err, calls, sentinel)
		}
	}
}

func TestRunStopsOnFatalConsultationStoreError(t *testing.T) {
	db := openStore(t, filepath.Join(t.TempDir(), "tasks.db"))
	defer closeStore(t, db)
	called := make(chan struct{})
	clientStopped := make(chan struct{})
	wrongHandler := errors.New("task handler called")
	err := (App{
		Store: db,
		Client: clientFunc(func(ctx context.Context, handler onebot.Handler) error {
			if err := handler(ctx, onebot.GroupMessage{GroupID: "100", UserID: "200", MessageID: "1", Text: "咨询", Mentioned: true}); err != nil {
				return err
			}
			<-ctx.Done()
			close(clientStopped)
			return nil
		}),
		Scheduler: blockingScheduler{},
		Service: serviceFunc(func(context.Context, tasksvc.Message) error {
			return errors.Join(tasksvc.ErrFatalStore, wrongHandler)
		}),
		Consultations: serviceFunc(func(context.Context, tasksvc.Message) error {
			close(called)
			return errors.Join(tasksvc.ErrFatalStore, errors.New("consultation database failed"))
		}),
		Groups: groupFunc(func(string) bool { return true }), MessageWorkers: 1, Logger: discardLogger(),
	}).Run(context.Background())
	waitClosed(t, called, "fatal consultation error")
	if !errors.Is(err, tasksvc.ErrFatalStore) || errors.Is(err, wrongHandler) {
		t.Fatalf("Run error = %v, want ErrFatalStore", err)
	}
	waitClosed(t, clientStopped, "fatal consultation cancellation")
}

func TestConsultationEndToEndRoutesPersistsAndReplays(t *testing.T) {
	root := t.TempDir()
	repo, _ := createGitProject(t, root)
	oneBot := newFakeOneBot(t, "10000", "consultation-e2e-token")
	defer oneBot.Close()
	cfg := config.Config{
		OneBot:       config.OneBotConfig{URL: oneBot.URL(), AccessTokenEnv: "TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(root, "tasks.db"), LogDir: filepath.Join(root, "logs"), WorktreeRoot: filepath.Join(root, "worktrees"),
		Consultation: config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 2}, MessageWorkers: 1,
		AllowedGroupIDs: []string{"100"}, EmployeeIDs: []string{"200"},
		Codex: config.CodexConfig{Binary: "/bin/true"}, OpsCommand: []string{"/bin/true"},
		Projects: []config.Project{{
			ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin",
			Checks: [][]string{{"/bin/true"}}, MaxConcurrent: 1, CodexTimeoutSeconds: 5, LogRetentionDays: 7,
		}},
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := openStore(t, cfg.DatabasePath)
	defer closeStore(t, db)
	logs, err := tasklog.Open(cfg.LogDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &onebot.Client{URL: oneBot.URL(), Token: "consultation-e2e-token", SelfID: "10000", MessageRunes: 1200}
	authorizer := auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, nil)
	asker := &recordingAsker{answer: "consultation answer"}
	consultations := consultsvc.NewService(registry, cfg.Consultation, db, authorizer, asker, client, logs)
	var taskCalls atomic.Int32
	taskCalled := make(chan struct{}, 1)
	a := App{
		Store: db, Client: client, Scheduler: blockingScheduler{}, Groups: authorizer,
		Service: serviceFunc(func(context.Context, tasksvc.Message) error {
			taskCalls.Add(1)
			taskCalled <- struct{}{}
			return nil
		}),
		Consultations: consultations, MessageWorkers: 1, Logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	oneBot.WaitReady(t)

	generic := groupEvent("101", "100", "200", "10000", "解释一下服务边界", true)
	oneBot.Send(t, generic)
	oneBot.WaitText(t, func(text string) bool { return text == "consultation answer" })
	oneBot.Send(t, generic)
	oneBot.WaitText(t, func(text string) bool { return text == "consultation answer" })
	oneBot.Send(t, groupEvent("102", "100", "200", "10000", "问 [p] 入口在哪里？", true))
	oneBot.WaitText(t, func(text string) bool { return text == "consultation answer" })
	oneBot.Send(t, groupEvent("103", "100", "200", "10000", "[p] 修改入口", true))
	waitClosed(t, taskCalled, "task route")

	calls := asker.Calls()
	if len(calls) != 2 || calls[0].WorkingDir != cfg.Consultation.Workspace || calls[1].WorkingDir != repo {
		t.Fatalf("Ask calls = %#v, want generic workspace then project repository", calls)
	}
	if taskCalls.Load() != 1 {
		t.Fatalf("task calls = %d, want 1", taskCalls.Load())
	}
	for _, id := range []string{"101", "102"} {
		consultation, err := db.GetConsultationByMessage(context.Background(), "100", id)
		if err != nil || consultation.Reply != "consultation answer" || consultation.CompletedAt.IsZero() {
			t.Fatalf("consultation %s = %#v, %v", id, consultation, err)
		}
	}
	if processed, err := db.MessageProcessed(context.Background(), "100\x00101"); err != nil || processed {
		t.Fatalf("consultation processed-message state = %v, %v", processed, err)
	}
	if _, err := db.GetTask(context.Background(), tasksvc.TaskID("100", "101")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("consultation created task: %v", err)
	}
	assertTableCount(t, cfg.DatabasePath, "tasks", 0)
	assertTableCount(t, cfg.DatabasePath, "audit_events", 0)
	assertTableCount(t, cfg.DatabasePath, "processed_messages", 0)

	cancel()
	if err := waitError(t, done, "consultation app shutdown"); err != nil {
		t.Fatal(err)
	}
}

type recordingAsker struct {
	mu     sync.Mutex
	answer string
	calls  []codex.Request
}

func (a *recordingAsker) Ask(_ context.Context, request codex.Request) (codex.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, request)
	return codex.Result{Final: a.answer}, nil
}

func (a *recordingAsker) Calls() []codex.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]codex.Request(nil), a.calls...)
}

func assertTableCount(t *testing.T, path, table string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}
