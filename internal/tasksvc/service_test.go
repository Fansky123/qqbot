package tasksvc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/store"
)

type fakePlanner struct {
	mu     sync.Mutex
	calls  int
	block  <-chan struct{}
	result codex.Result
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
	return p.result, nil
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
}

func (n *fakeNotifier) Send(_ context.Context, _ string, message string) error {
	n.mu.Lock()
	n.messages = append(n.messages, message)
	n.mu.Unlock()
	return nil
}

func (n *fakeNotifier) Last() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.messages) == 0 {
		return ""
	}
	return n.messages[len(n.messages)-1]
}

type fakeLogs struct{ text string }

func (l fakeLogs) Summary(_ string, maxRunes int) (string, error) {
	runes := []rune(l.text)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes), nil
}

func testService(t *testing.T, planner Planner) (*Service, *store.Store, *fakeScheduler, *fakeNotifier) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	projectRoot := t.TempDir()
	cfg := config.Config{
		MessageWorkers: 1,
		OneBot:         config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "bot", MessageRunes: 1200},
		DatabasePath:   filepath.Join(t.TempDir(), "db"), LogDir: t.TempDir(), WorktreeRoot: t.TempDir(),
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
	return NewService(registry, db, auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs), planner, scheduler, notifier, fakeLogs{"OPENAI_API_KEY=raw-secret NAPCAT_ACCESS_TOKEN=raw-token " + strings.Repeat("secret ", 1000)}), db, scheduler, notifier
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
	if err := svc.Handle(ctx, msg("m4", "admin", "补充 #"+id+" admin context")); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != model.StatusAwaitingConfirmation || !strings.Contains(task.Requirement, "admin context") {
		t.Fatalf("supplement task=%#v", task)
	}
	if err := svc.Handle(ctx, msg("m5", "admin", "确认 #"+id)); err != nil {
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
	if err := svc.Handle(ctx, msg("m2", "u1", "状态 #"+id)); err != nil {
		t.Fatal(err)
	}
	if len([]rune(notifier.Last())) > maxNotificationRunes {
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

func TestTaskID(t *testing.T) {
	if got := TaskID("g1", "m1"); len(got) != 14 || !strings.HasPrefix(got, "T-") {
		t.Fatalf("TaskID = %q", got)
	}
	if got := TaskID("g1", "m1"); got != TaskID("g1", "m1") {
		t.Fatal("TaskID is not deterministic")
	}
}
