package tasksvc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
)

type fakePlanner struct {
	mu     sync.Mutex
	calls  int
	block  <-chan struct{}
	result codex.Result
	err    error
}

func (p *fakePlanner) Plan(ctx context.Context, req codex.Request) (codex.Result, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return codex.Result{}, ctx.Err()
		}
	}
	return p.result, p.err
}

func (p *fakePlanner) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fakeScheduler struct {
	mu      sync.Mutex
	wake    int
	cancels []string
}

func (s *fakeScheduler) Wake() {
	s.mu.Lock()
	s.wake++
	s.mu.Unlock()
}

func (s *fakeScheduler) Cancel(taskID string) {
	s.mu.Lock()
	s.cancels = append(s.cancels, taskID)
	s.mu.Unlock()
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []string
	failNext int
}

func (n *fakeNotifier) Send(_ context.Context, _ string, message string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, message)
	if n.failNext > 0 {
		n.failNext--
		return errors.New("send failed")
	}
	return nil
}

func (n *fakeNotifier) FailNext() {
	n.mu.Lock()
	n.failNext++
	n.mu.Unlock()
}

func (n *fakeNotifier) Last() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.messages) == 0 {
		return ""
	}
	return n.messages[len(n.messages)-1]
}

func (n *fakeNotifier) Messages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.messages...)
}

type fakeLogs struct{ text string }

func (l fakeLogs) Summary(_ string, maxRunes int) (string, error) {
	runes := []rune(l.text)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes), nil
}

func (l fakeLogs) RedactText(text string) string { return strings.ToValidUTF8(text, "\uFFFD") }

type failingLogs struct{ err error }

func (l failingLogs) Summary(string, int) (string, error) { return "", l.err }
func (l failingLogs) RedactText(text string) string       { return text }

func TestServiceWrapsTaskLogFailureAsFatalStore(t *testing.T) {
	svc, db, _, _ := testServiceWithLogs(t, &fakePlanner{result: planResult()}, failingLogs{err: errors.New("log read failed")})
	ctx := context.Background()
	create := msg("m-fatal", "u1", "[orders] fatal log read")
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID(create.GroupID, create.MessageID)
	err := svc.Handle(ctx, msg("m-log", "u1", "日志 #"+id))
	if !errors.Is(err, ErrFatalStore) {
		t.Fatalf("log error = %v, want ErrFatalStore", err)
	}
	_ = db
}

func TestServiceWrapsSQLiteFailureAsFatalStore(t *testing.T) {
	svc, db, _, _ := testService(t, &fakePlanner{result: planResult()})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	err := svc.Handle(context.Background(), msg("m-db-fatal", "u1", "[orders] database failure"))
	if !errors.Is(err, ErrFatalStore) {
		t.Fatalf("database error = %v, want ErrFatalStore", err)
	}
}

func TestServiceRejectsUnknownUserWithoutSideEffects(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, scheduler, notifier := testService(t, planner)
	message := msg("unknown-user", "not-allowed", "[orders] must not run")

	if err := svc.Handle(context.Background(), message); !errors.Is(err, errUnauthorized) {
		t.Fatalf("unknown user error = %v, want errUnauthorized", err)
	}
	if planner.Calls() != 0 || scheduler.wake != 0 || len(scheduler.cancels) != 0 || len(notifier.Messages()) != 0 {
		t.Fatalf("unknown user side effects: planner=%d wake=%d cancels=%v notifications=%v",
			planner.Calls(), scheduler.wake, scheduler.cancels, notifier.Messages())
	}
	if _, err := db.GetTask(context.Background(), TaskID(message.GroupID, message.MessageID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user task error = %v, want ErrNotFound", err)
	}
	processed, err := db.MessageProcessed(context.Background(), message.GroupID+"\x00"+message.MessageID)
	if err != nil || processed {
		t.Fatalf("unknown user processed = %v, %v", processed, err)
	}
}

func testService(t *testing.T, planner Planner) (*Service, *store.Store, *fakeScheduler, *fakeNotifier) {
	return testServiceWithLogs(t, planner, fakeLogs{"OPENAI_API_KEY=raw-secret NAPCAT_ACCESS_TOKEN=raw-token " + strings.Repeat("secret ", 1000)})
}

func testServiceWithLogs(t *testing.T, planner Planner, logs LogReader) (*Service, *store.Store, *fakeScheduler, *fakeNotifier) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	projectRoot := t.TempDir()
	cfg := config.Config{
		MessageWorkers: 1,
		OneBot:         config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath:   filepath.Join(t.TempDir(), "db"), LogDir: t.TempDir(), WorktreeRoot: t.TempDir(), Consultation: config.ConsultationConfig{Workspace: t.TempDir(), TimeoutSeconds: 90},
		AllowedGroupIDs: []string{"g1"}, EmployeeIDs: []string{"u1", "u2", "admin"}, AdminIDs: []string{"admin"},
		Codex: config.CodexConfig{Binary: "/bin/true"}, OpsCommand: []string{"/bin/true"},
		Projects: []config.Project{{ID: "orders", Aliases: []string{"orders", "o"}, RepoPath: projectRoot, BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}}, MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 1}},
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &fakeScheduler{}
	notifier := &fakeNotifier{}
	return NewService(registry, db, auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs), planner, scheduler, notifier, logs), db, scheduler, notifier
}

func planResult() codex.Result {
	plan, _ := json.Marshal(codex.Plan{Summary: "fix duplicate submit", Scope: []string{"handler"}, Checks: []string{"go test ./..."}, Risks: []string{"migration"}})
	return codex.Result{Final: string(plan), SessionID: "plan-session"}
}

func msg(id, user, text string) Message {
	return Message{GroupID: "g1", UserID: user, MessageID: id, Text: text, Mentioned: strings.HasPrefix(text, "[")}
}

func TestServiceCreateConfirmReplayAndConcurrentDedup(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, scheduler, notifier := testService(t, planner)
	ctx := context.Background()
	create := msg("m1", "u1", "[orders] fix duplicate submit")
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID(create.GroupID, create.MessageID)
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusAwaitingConfirmation || planner.Calls() != 1 {
		t.Fatalf("task=%#v planner calls=%d", task, planner.Calls())
	}
	if !strings.Contains(notifier.Last(), "确认 #"+id) || !strings.Contains(notifier.Last(), "fix duplicate submit") {
		t.Fatalf("confirmation = %q", notifier.Last())
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	if planner.Calls() != 1 {
		t.Fatalf("replay planner calls = %d", planner.Calls())
	}
	confirm := msg("m2", "u1", "确认 #"+id)
	if err := svc.Handle(ctx, confirm); err != nil {
		t.Fatal(err)
	}
	task, err = db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusQueued || scheduler.wake != 1 {
		t.Fatalf("confirmed task=%#v wake=%d", task, scheduler.wake)
	}

	concurrentPlanner := &fakePlanner{result: planResult()}
	concurrentSvc, concurrentDB, _, _ := testService(t, concurrentPlanner)
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := concurrentSvc.Handle(ctx, create); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 || concurrentPlanner.Calls() != 1 {
		t.Fatalf("concurrent failures=%d planner calls=%d", failures.Load(), concurrentPlanner.Calls())
	}
	if _, err := concurrentDB.GetTask(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestServiceDeduplicatesAcrossStoreConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	firstDB, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstDB.Close() })
	secondDB, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	projectRoot := t.TempDir()
	cfg := config.Config{
		MessageWorkers: 1,
		OneBot:         config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath:   path, LogDir: t.TempDir(), WorktreeRoot: t.TempDir(), Consultation: config.ConsultationConfig{Workspace: t.TempDir(), TimeoutSeconds: 90},
		AllowedGroupIDs: []string{"g1"}, EmployeeIDs: []string{"u1"},
		Codex: config.CodexConfig{Binary: "/bin/true"}, OpsCommand: []string{"/bin/true"},
		Projects: []config.Project{{ID: "orders", Aliases: []string{"orders"}, RepoPath: projectRoot, BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}}, MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 1}},
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	planningGate := make(chan struct{})
	planner := &fakePlanner{result: planResult(), block: planningGate}
	scheduler := &fakeScheduler{}
	logs := fakeLogs{}
	services := []*Service{
		NewService(registry, firstDB, auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, nil), planner, scheduler, &fakeNotifier{}, logs),
		NewService(registry, secondDB, auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, nil), planner, scheduler, &fakeNotifier{}, logs),
	}
	create := msg("multi-create", "u1", "[orders] task")
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = services[0].Handle(ctx, create)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for planner.Calls() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if planner.Calls() != 1 {
		t.Fatal("first service did not enter planner")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[1] = services[1].Handle(ctx, create)
	}()
	time.Sleep(50 * time.Millisecond)
	if planner.Calls() != 1 {
		t.Fatalf("second service duplicated planner call: %d", planner.Calls())
	}
	close(planningGate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("multi-service create: %v", err)
		}
	}
	if planner.Calls() != 1 {
		t.Fatalf("multi-service planner calls = %d", planner.Calls())
	}

	id := TaskID("g1", "multi-create")
	confirm := msg("multi-confirm", "u1", "确认 #"+id)
	start := make(chan struct{})
	for i := range services {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = services[i].Handle(ctx, confirm)
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("multi-service confirm: %v", err)
		}
	}
	if scheduler.wake != 1 {
		t.Fatalf("multi-service scheduler wake = %d", scheduler.wake)
	}
	task, err := firstDB.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusQueued {
		t.Fatalf("multi-service task = %#v", task)
	}
	inspection, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspection.Close() })
	queries := map[string]string{
		"processed_messages": "SELECT COUNT(*) FROM processed_messages",
		"task_inputs":        "SELECT COUNT(*) FROM task_inputs",
		"audit_events":       "SELECT COUNT(*) FROM audit_events",
	}
	for table, query := range queries {
		var got int
		if err := inspection.QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Errorf("multi-service %s rows = %d, want 2", table, got)
		}
	}
}

func TestServiceCreateResumesMatchingDraft(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, _, _ := testService(t, planner)
	ctx := context.Background()
	create := msg("draft-replay", "u1", "[orders] task")
	now := time.Now().UTC().Add(-time.Minute)
	draft := &model.Task{ID: TaskID("g1", "draft-replay"), ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "task", Status: model.StatusDraft, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateTask(ctx, draft); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusAwaitingConfirmation || planner.Calls() != 1 {
		t.Fatalf("draft replay task=%#v planner calls=%d", got, planner.Calls())
	}
	if processed, err := db.MessageProcessed(ctx, "g1\x00draft-replay"); err != nil || !processed {
		t.Fatalf("draft replay processed=%v err=%v", processed, err)
	}
}

func TestServiceOwnershipSupplementCancelAndAdmin(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, scheduler, _ := testService(t, planner)
	ctx := context.Background()
	create := msg("m1", "u1", "[orders] task")
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "m1")
	if err := svc.Handle(ctx, msg("m2", "u2", "补充 #"+id+" no")); err == nil {
		t.Fatal("non-owner supplement succeeded")
	}
	if err := svc.Handle(ctx, msg("m3", "u2", "取消 #"+id)); err == nil {
		t.Fatal("non-owner cancel succeeded")
	}
	if err := svc.Handle(ctx, msg("m4", "admin", "补充 #"+id+" admin context")); err == nil {
		t.Fatal("supplement awaiting confirmation succeeded")
	}
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusAwaitingConfirmation || strings.Contains(task.Requirement, "admin context") {
		t.Fatalf("supplement task=%#v", task)
	}
	if err := svc.Handle(ctx, msg("m5", "admin", "确认 #"+id)); err == nil {
		t.Fatal("admin confirmed another user's task")
	}
	if err := svc.Handle(ctx, msg("m5-owner", "u1", "确认 #"+id)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("m6", "admin", "取消 #"+id)); err != nil {
		t.Fatal(err)
	}
	if task, err = db.GetTask(ctx, id); err != nil || task.Status != model.StatusCancelled {
		t.Fatalf("cancel task=%#v err=%v", task, err)
	}
	if len(scheduler.cancels) != 0 {
		t.Fatalf("unexpected scheduler cancels: %#v", scheduler.cancels)
	}

	for i, status := range []model.Status{model.StatusBlocked, model.StatusAwaitingMergeApproval, model.StatusRunning} {
		taskID := fmt.Sprintf("T-%012X", i+1)
		task := &model.Task{ID: taskID, ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "task", Status: status, TaskCommit: "task-commit", RCCommit: "rc-commit", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := svc.Handle(ctx, msg("sup-"+string(status), "u1", "补充 #"+taskID+" more")); status == model.StatusRunning {
			if err == nil {
				t.Fatal("running supplement succeeded")
			}
		} else if err != nil {
			t.Fatalf("supplement from %s: %v", status, err)
		} else {
			updated, getErr := db.GetTask(ctx, taskID)
			if getErr != nil || updated.Status != model.StatusQueued || updated.TaskCommit != "" || updated.RCCommit != "" {
				t.Fatalf("supplement from %s updated=%#v err=%v", status, updated, getErr)
			}
		}
	}
	runningID := "T-0000000000F1"
	runningTask := &model.Task{ID: runningID, ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "running", Status: model.StatusRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateTask(ctx, runningTask); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("cancel-running", "u1", "取消 #"+runningID)); err != nil {
		t.Fatal(err)
	}
	if len(scheduler.cancels) != 1 || scheduler.cancels[0] != runningID {
		t.Fatalf("running cancellation calls = %#v", scheduler.cancels)
	}

	merged := &model.Task{ID: "T-" + strings.Repeat("B", 12), ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "task", Status: model.StatusMerged, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := db.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("cancel-merged", "u1", "取消 #"+merged.ID)); err == nil {
		t.Fatal("cancel after merge succeeded")
	}
}

func TestServiceStatusAndLogAreBounded(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, _, notifier := testService(t, planner)
	ctx := context.Background()
	if err := svc.Handle(ctx, msg("m1", "u1", "[orders] task")); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "m1")
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task.Summary = string([]byte{'x', 0xff, 'y'})
	if err := db.SaveTask(ctx, task, task.Version); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("m2", "u1", "状态 #"+id)); err != nil {
		t.Fatal(err)
	}
	if len([]rune(notifier.Last())) > maxNotificationRunes || !utf8.ValidString(notifier.Last()) {
		t.Fatalf("status length = %d", len([]rune(notifier.Last())))
	}
	if err := svc.Handle(ctx, msg("m3", "u1", "日志 #"+id)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notifier.Last(), "NAPCAT_ACCESS_TOKEN") || strings.Contains(notifier.Last(), "OPENAI_API_KEY") {
		t.Fatalf("log leaked environment name: %q", notifier.Last())
	}
	if _, err := db.GetTask(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRetriesNotificationsWithoutRepeatingEffects(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, db, scheduler, notifier := testService(t, planner)
	ctx := context.Background()
	create := msg("retry-create", "u1", "[orders] task")
	notifier.FailNext()
	if err := svc.Handle(ctx, create); !errors.Is(err, ErrNotificationDelivery) {
		t.Fatalf("create notification error = %v", err)
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	if planner.Calls() != 1 {
		t.Fatalf("planner calls = %d", planner.Calls())
	}
	id := TaskID("g1", "retry-create")

	confirm := msg("retry-confirm", "u1", "确认 #"+id)
	notifier.FailNext()
	if err := svc.Handle(ctx, confirm); err == nil {
		t.Fatal("confirm notification failure was not returned")
	}
	if err := svc.Handle(ctx, confirm); err != nil {
		t.Fatal(err)
	}
	if scheduler.wake != 1 {
		t.Fatalf("confirm replay wakes = %d", scheduler.wake)
	}

	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = model.StatusBlocked
	if err := db.SaveTask(ctx, task, task.Version); err != nil {
		t.Fatal(err)
	}
	supplement := msg("retry-supplement", "admin", "补充 #"+id+" more")
	notifier.FailNext()
	if err := svc.Handle(ctx, supplement); err == nil {
		t.Fatal("supplement notification failure was not returned")
	}
	if err := svc.Handle(ctx, supplement); err != nil {
		t.Fatal(err)
	}
	if scheduler.wake != 2 {
		t.Fatalf("supplement replay wakes = %d", scheduler.wake)
	}

	task, err = db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = model.StatusRunning
	if err := db.SaveTask(ctx, task, task.Version); err != nil {
		t.Fatal(err)
	}
	cancel := msg("retry-cancel", "admin", "取消 #"+id)
	notifier.FailNext()
	if err := svc.Handle(ctx, cancel); err == nil {
		t.Fatal("cancel notification failure was not returned")
	}
	if err := svc.Handle(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	if len(scheduler.cancels) != 1 {
		t.Fatalf("cancel replay calls = %#v", scheduler.cancels)
	}

	for _, request := range []Message{
		msg("retry-status", "u1", "状态 #"+id),
		msg("retry-log", "u1", "日志 #"+id),
	} {
		notifier.FailNext()
		if err := svc.Handle(ctx, request); err == nil {
			t.Fatalf("%s notification failure was not returned", request.Text)
		}
		if err := svc.Handle(ctx, request); err != nil {
			t.Fatalf("replay %s: %v", request.Text, err)
		}
	}
}

func TestServiceRetriesFailedPlanningNotification(t *testing.T) {
	planningErr := errors.New("planner unavailable")
	planner := &fakePlanner{err: planningErr}
	svc, db, _, notifier := testService(t, planner)
	ctx := context.Background()
	create := msg("failed-plan-notify", "u1", "[orders] task")
	notifier.FailNext()
	err := svc.Handle(ctx, create)
	if !errors.Is(err, planningErr) || !errors.Is(err, ErrNotificationDelivery) {
		t.Fatalf("planning notification error = %v", err)
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	if planner.Calls() != 1 {
		t.Fatalf("planning failure replay calls = %d", planner.Calls())
	}
	task, err := db.GetTask(ctx, TaskID("g1", "failed-plan-notify"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusFailed || !strings.Contains(notifier.Last(), "failed") {
		t.Fatalf("failed planning replay task=%#v notification=%q", task, notifier.Last())
	}
}

func TestServiceMutationRollsBackWithCommandRecords(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	_, db, _, _ := testService(t, planner)
	ctx := context.Background()
	now := time.Now().UTC()
	task := &model.Task{ID: "T-123456789ABC", ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "task", Status: model.StatusAwaitingConfirmation, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status = model.StatusQueued
	key := "g1\x00atomic"
	badInput := model.Input{TaskID: "missing", Kind: "confirmation", UserID: "u1", GroupID: "g1", MessageID: "atomic", Body: "confirm", CreatedAt: now}
	if err := db.CommitTaskMutation(ctx, task, task.Version, badInput, "confirmation", "queued", key, now); err == nil {
		t.Fatal("mutation with invalid input succeeded")
	}
	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusAwaitingConfirmation || got.Version != 1 {
		t.Fatalf("rolled back task = %#v", got)
	}
	if processed, err := db.MessageProcessed(ctx, key); err != nil || processed {
		t.Fatalf("processed after rollback = %v, %v", processed, err)
	}
	goodInput := badInput
	goodInput.TaskID = task.ID
	if err := db.CommitTaskMutation(ctx, task, task.Version, goodInput, "confirmation", "queued", key, now); err != nil {
		t.Fatal(err)
	}
	if processed, err := db.MessageProcessed(ctx, key); err != nil || !processed {
		t.Fatalf("processed after retry = %v, %v", processed, err)
	}
}

func TestServiceRejectsOversizedInputsAndPlans(t *testing.T) {
	ctx := context.Background()
	planner := &fakePlanner{result: planResult()}
	svc, db, _, _ := testService(t, planner)
	message := msg("huge-message", "u1", "[orders] "+strings.Repeat("x", maxMessageBytes))
	if err := svc.Handle(ctx, message); err == nil {
		t.Fatal("oversized message succeeded")
	}
	if planner.Calls() != 0 {
		t.Fatalf("oversized message planner calls = %d", planner.Calls())
	}
	if _, err := db.GetTask(ctx, TaskID("g1", "huge-message")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oversized message stored task: %v", err)
	}

	hugePlan := codex.Plan{Summary: strings.Repeat("x", maxPlanBytes), Scope: []string{}, Checks: []string{}, Risks: []string{}}
	encoded, err := json.Marshal(hugePlan)
	if err != nil {
		t.Fatal(err)
	}
	hugePlanner := &fakePlanner{result: codex.Result{Final: string(encoded)}}
	hugeSvc, hugeDB, _, _ := testService(t, hugePlanner)
	create := msg("huge-plan", "u1", "[orders] task")
	if err := hugeSvc.Handle(ctx, create); err == nil {
		t.Fatal("oversized plan succeeded")
	}
	task, err := hugeDB.GetTask(ctx, TaskID("g1", "huge-plan"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusFailed || task.Plan != "" {
		t.Fatalf("oversized plan task = %#v", task)
	}
	var nilContext context.Context
	if err := svc.Handle(nilContext, msg("nil-context", "u1", "[orders] task")); err == nil {
		t.Fatal("nil context succeeded")
	}
}

func TestServiceConfirmationPreservesSectionsAndCommand(t *testing.T) {
	plan := codex.Plan{
		Summary: strings.Repeat("摘", 1300),
		Scope:   []string{strings.Repeat("范围", 300), strings.Repeat("二", 300)},
		Checks:  []string{strings.Repeat("检查", 300), strings.Repeat("三", 300)},
		Risks:   []string{strings.Repeat("风险", 300), strings.Repeat("四", 300)},
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planner := &fakePlanner{result: codex.Result{Final: string(encoded)}}
	svc, _, _, notifier := testService(t, planner)
	create := msg("long-plan", "u1", "[orders] task")
	if err := svc.Handle(context.Background(), create); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "long-plan")
	got := notifier.Last()
	for _, required := range []string{"任务 #" + id, "项目：orders", "摘要：", "范围：", "检查：", "风险：", "请回复：确认 #" + id} {
		if !strings.Contains(got, required) {
			t.Errorf("confirmation lacks %q: %q", required, got)
		}
	}
	if !strings.HasSuffix(got, "请回复：确认 #"+id) {
		t.Errorf("confirmation suffix = %q", got)
	}
	if len([]rune(got)) > maxNotificationRunes || !utf8.ValidString(got) {
		t.Fatalf("confirmation runes=%d valid=%v", len([]rune(got)), utf8.ValidString(got))
	}
	longProjectTask := &model.Task{ID: id, ProjectID: strings.Repeat("project", 1000)}
	got = svc.boundNotification(svc.confirmation(longProjectTask, plan))
	if !strings.Contains(got, "项目：") || !strings.HasSuffix(got, "请回复：确认 #"+id) || len([]rune(got)) > maxNotificationRunes {
		t.Fatalf("long-project confirmation lost structure: runes=%d suffix=%v", len([]rune(got)), strings.HasSuffix(got, "请回复：确认 #"+id))
	}
}

func TestServiceRedactsExactSecretsFromEveryNotificationPath(t *testing.T) {
	secret := "quote\\line\ncontrol\tvalue"
	encodedSecret, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	escapedSecret := string(encodedSecret[1 : len(encodedSecret)-1])
	logs, err := tasklog.Open(t.TempDir(), []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	plan := codex.Plan{
		Summary: strings.Repeat("x", 1190) + " plain " + secret,
		Scope:   []string{"escaped " + escapedSecret},
		Checks:  []string{"check " + secret},
		Risks:   []string{"risk " + escapedSecret},
	}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planner := &fakePlanner{result: codex.Result{Final: string(encodedPlan)}}
	svc, db, _, notifier := testServiceWithLogs(t, planner, logs)
	ctx := context.Background()
	create := msg("exact-secret", "u1", "[orders] task")
	notifier.FailNext()
	if err := svc.Handle(ctx, create); err == nil {
		t.Fatal("initial notification failure was not returned")
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "exact-secret")
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(task.Plan, secret) || strings.Contains(task.Plan, escapedSecret) || strings.Contains(task.Summary, secret) || strings.Contains(task.Summary, escapedSecret) {
		t.Fatalf("stored task leaked exact secret: plan=%q summary=%q", task.Plan, task.Summary)
	}
	if err := logs.Append(id, "test", []byte("raw log "+secret+" escaped "+escapedSecret)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("secret-status", "u1", "状态 #"+id)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("secret-log", "u1", "日志 #"+id)); err != nil {
		t.Fatal(err)
	}
	for i, message := range notifier.Messages() {
		if strings.Contains(message, secret) || strings.Contains(message, escapedSecret) {
			t.Fatalf("notification %d leaked exact secret: %q", i, message)
		}
	}
}

func TestServiceRedactsMarkerContainingSecretAcrossReplayAndStatus(t *testing.T) {
	secret := "quote\"[REDACTED]\\suffix"
	encodedSecret, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	escapedSecret := string(encodedSecret[1 : len(encodedSecret)-1])
	logs, err := tasklog.Open(t.TempDir(), []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	plan := codex.Plan{Summary: secret, Scope: []string{escapedSecret}, Checks: []string{}, Risks: []string{}}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planner := &fakePlanner{result: codex.Result{Final: string(encodedPlan)}}
	svc, db, _, notifier := testServiceWithLogs(t, planner, logs)
	ctx := context.Background()
	create := msg("marker-secret", "u1", "[orders] task")
	notifier.FailNext()
	if err := svc.Handle(ctx, create); err == nil {
		t.Fatal("initial notification failure was not returned")
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "marker-secret")
	if err := svc.Handle(ctx, msg("marker-status", "u1", "状态 #"+id)); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{task.Plan, task.Summary} {
		if strings.Contains(stored, secret) || strings.Contains(stored, escapedSecret) {
			t.Fatalf("stored task leaked marker-containing secret: %q", stored)
		}
	}
	for i, notification := range notifier.Messages() {
		if strings.Contains(notification, secret) || strings.Contains(notification, escapedSecret) || !utf8.ValidString(notification) {
			t.Fatalf("notification %d leaked marker-containing secret: %q", i, notification)
		}
	}
}

func TestServiceGenericThenExactRedactionIsStableAcrossAllOutputs(t *testing.T) {
	secrets := []string{"REDACTED", "generic-R-secret"}
	logs, err := tasklog.Open(t.TempDir(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	input := "FOO_PASSWORD=value Bearer bearer-value REDACTED generic-R-secret"
	plan := codex.Plan{Summary: input, Scope: []string{input}, Checks: []string{input}, Risks: []string{input}}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planner := &fakePlanner{result: codex.Result{Final: string(encodedPlan)}}
	svc, db, _, notifier := testServiceWithLogs(t, planner, logs)
	once := svc.sanitize(input)
	twice := svc.sanitize(once)
	if once != twice || !utf8.ValidString(once) {
		t.Fatalf("sanitize not stable: once=%q twice=%q", once, twice)
	}
	for _, secret := range secrets {
		if strings.Contains(once, secret) {
			t.Fatalf("sanitize leaked %q: %q", secret, once)
		}
	}
	ctx := context.Background()
	create := msg("generic-exact-order", "u1", "[orders] task")
	notifier.FailNext()
	if err := svc.Handle(ctx, create); !errors.Is(err, ErrNotificationDelivery) {
		t.Fatalf("initial notification error = %v", err)
	}
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	id := TaskID("g1", "generic-exact-order")
	if err := logs.Append(id, "test", []byte(input)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("generic-exact-status", "u1", "状态 #"+id)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(ctx, msg("generic-exact-log", "u1", "日志 #"+id)); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var storedPlan codex.Plan
	if err := json.Unmarshal([]byte(task.Plan), &storedPlan); err != nil {
		t.Fatal(err)
	}
	storedFields := append([]string{storedPlan.Summary}, storedPlan.Scope...)
	storedFields = append(storedFields, storedPlan.Checks...)
	storedFields = append(storedFields, storedPlan.Risks...)
	storedFields = append(storedFields, task.Summary)
	for fieldIndex, output := range storedFields {
		for _, secret := range secrets {
			if strings.Contains(output, secret) {
				t.Fatalf("stored field %d leaked %q: %q", fieldIndex, secret, output)
			}
		}
	}
	for i, notification := range notifier.Messages() {
		if !utf8.ValidString(notification) {
			t.Fatalf("notification %d invalid UTF-8", i)
		}
		for _, secret := range secrets {
			if strings.Contains(notification, secret) {
				t.Fatalf("notification %d leaked %q: %q", i, secret, notification)
			}
		}
	}
}

func TestServiceAcceptsPlanWithinBoundsAfterExactRedaction(t *testing.T) {
	logs, err := tasklog.Open(t.TempDir(), []string{"AAAAAAAA"})
	if err != nil {
		t.Fatal(err)
	}
	field := strings.Repeat("AAAAAAAA", 500)
	plan := codex.Plan{Summary: field, Scope: []string{field, field, field, field, field}, Checks: []string{}, Risks: []string{}}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(encodedPlan) >= maxPlanBytes {
		t.Fatalf("raw test plan unexpectedly exceeds limit: %d", len(encodedPlan))
	}
	planner := &fakePlanner{result: codex.Result{Final: string(encodedPlan)}}
	svc, db, _, _ := testServiceWithLogs(t, planner, logs)
	ctx := context.Background()
	create := msg("expanded-plan", "u1", "[orders] task")
	if err := svc.Handle(ctx, create); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(ctx, TaskID("g1", "expanded-plan"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusAwaitingConfirmation || strings.Contains(task.Plan, "AAAAAAAA") || strings.Contains(task.Summary, "AAAAAAAA") {
		t.Fatalf("redacted task = %#v", task)
	}
	if processed, err := db.MessageProcessed(ctx, "g1\x00expanded-plan"); err != nil || !processed {
		t.Fatalf("expanded plan processed=%v err=%v", processed, err)
	}
}

type unredactedLogs struct{}

func (unredactedLogs) Summary(string, int) (string, error) { return "", nil }

func TestServiceFailsClosedWithoutExactRedactor(t *testing.T) {
	planner := &fakePlanner{result: planResult()}
	svc, _, _, _ := testServiceWithLogs(t, planner, unredactedLogs{})
	if err := svc.Handle(context.Background(), msg("no-redactor", "u1", "[orders] task")); err == nil {
		t.Fatal("service handled message without exact redactor")
	}
	if planner.Calls() != 0 {
		t.Fatalf("planner calls = %d", planner.Calls())
	}
}

func TestTaskID(t *testing.T) {
	if got := TaskID("g1", "m1"); len(got) != 14 || !strings.HasPrefix(got, "T-") {
		t.Fatalf("TaskID = %q", got)
	}
	if got := TaskID("g1", "m1"); got != TaskID("g1", "m1") {
		t.Fatal("TaskID is not deterministic")
	}
}
