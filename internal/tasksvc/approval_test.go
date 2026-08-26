package tasksvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"qqcodex/internal/model"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
)

const approvalRCCommit = "cccccccccccccccccccccccccccccccccccccccc"

func TestMergeApprovalRejectsEmployeeWithoutProcessing(t *testing.T) {
	svc, db, scheduler, _ := testService(t, &fakePlanner{result: planResult()})
	task := approvalTask(t, db, "T-000000001101", model.StatusAwaitingMergeApproval, schedulerTaskCommit, "")
	message := msg("employee-merge", "u1", "批准合并 #"+task.ID)

	if err := svc.Handle(context.Background(), message); err == nil {
		t.Fatal("employee merge approval succeeded")
	}
	got := mustApprovalTask(t, db, task.ID)
	if got.Status != model.StatusAwaitingMergeApproval {
		t.Fatalf("task status = %s, want %s", got.Status, model.StatusAwaitingMergeApproval)
	}
	processed, err := db.MessageProcessed(context.Background(), message.GroupID+"\x00"+message.MessageID)
	if err != nil || processed {
		t.Fatalf("employee approval processed = %v, %v", processed, err)
	}
	if _, err := db.LatestApproval(context.Background(), task.ID, "merge"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("employee approval row error = %v, want ErrNotFound", err)
	}
	if scheduler.wake != 0 {
		t.Fatalf("employee approval wakes = %d", scheduler.wake)
	}
}

func TestMergeApprovalPersistsImmutableCommitAndDeduplicates(t *testing.T) {
	svc, db, scheduler, notifier := testService(t, &fakePlanner{result: planResult()})
	task := approvalTask(t, db, "T-000000001102", model.StatusAwaitingMergeApproval, schedulerTaskCommit, "")
	message := msg("admin-merge", "admin", "批准合并 #"+task.ID)

	notifier.FailNext()
	if err := svc.Handle(context.Background(), message); !errors.Is(err, ErrNotificationDelivery) {
		t.Fatalf("first approval error = %v, want notification delivery", err)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatalf("approval replay: %v", err)
	}

	got := mustApprovalTask(t, db, task.ID)
	if got.Status != model.StatusMerging || got.TaskCommit != schedulerTaskCommit {
		t.Fatalf("approved task = %#v", got)
	}
	approval, err := db.LatestApproval(context.Background(), task.ID, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if approval.UserID != "admin" || approval.MessageID != message.MessageID || approval.BoundCommit != schedulerTaskCommit || approval.Result != "approved" {
		t.Fatalf("merge approval = %#v", approval)
	}
	if scheduler.wake != 1 {
		t.Fatalf("merge approval wakes = %d, want 1", scheduler.wake)
	}
}

func TestDeployApprovalBindsRCCommitAndAllowsNewRetryMessage(t *testing.T) {
	for _, status := range []model.Status{model.StatusAwaitingDeployApproval, model.StatusDeployFailed} {
		t.Run(string(status), func(t *testing.T) {
			svc, db, scheduler, _ := testService(t, &fakePlanner{result: planResult()})
			task := approvalTask(t, db, "T-000000001103", status, schedulerTaskCommit, approvalRCCommit)
			message := msg("admin-deploy-"+string(status), "admin", "批准部署 #"+task.ID)

			if err := svc.Handle(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			got := mustApprovalTask(t, db, task.ID)
			if got.Status != model.StatusDeploying || got.RCCommit != approvalRCCommit || got.DeployKey != ops.DeployKey("orders", task.ID, approvalRCCommit) {
				t.Fatalf("approved task = %#v", got)
			}
			approval, err := db.LatestApproval(context.Background(), task.ID, "deploy")
			if err != nil {
				t.Fatal(err)
			}
			if approval.BoundCommit != approvalRCCommit || approval.Result != "approved" {
				t.Fatalf("deploy approval = %#v", approval)
			}
			if scheduler.wake != 1 {
				t.Fatalf("deploy approval wakes = %d", scheduler.wake)
			}
		})
	}
}

func TestDeployApprovalRejectsMissingRCCommit(t *testing.T) {
	svc, db, scheduler, _ := testService(t, &fakePlanner{result: planResult()})
	task := approvalTask(t, db, "T-000000001104", model.StatusAwaitingDeployApproval, schedulerTaskCommit, "")
	message := msg("admin-deploy-empty", "admin", "批准部署 #"+task.ID)

	if err := svc.Handle(context.Background(), message); err == nil {
		t.Fatal("deploy approval without RC commit succeeded")
	}
	if got := mustApprovalTask(t, db, task.ID); got.Status != model.StatusAwaitingDeployApproval {
		t.Fatalf("task status = %s", got.Status)
	}
	processed, err := db.MessageProcessed(context.Background(), "g1\x00"+message.MessageID)
	if err != nil || processed {
		t.Fatalf("invalid deploy approval processed = %v, %v", processed, err)
	}
	if scheduler.wake != 0 {
		t.Fatalf("invalid deploy approval wakes = %d", scheduler.wake)
	}
}

func TestDeployApprovalReplayUsesOriginalBoundCommit(t *testing.T) {
	svc, db, _, notifier := testService(t, &fakePlanner{result: planResult()})
	task := approvalTask(t, db, "T-000000001106", model.StatusAwaitingDeployApproval, schedulerTaskCommit, approvalRCCommit)
	message := msg("deploy-replay-bound", "admin", "批准部署 #"+task.ID)
	notifier.FailNext()
	if err := svc.Handle(context.Background(), message); !errors.Is(err, ErrNotificationDelivery) {
		t.Fatalf("first deploy approval error = %v", err)
	}
	changed := mustApprovalTask(t, db, task.ID)
	changed.Status = model.StatusAwaitingDeployApproval
	changed.RCCommit = schedulerNewRCCommit
	changed.DeployKey = ""
	if err := db.SaveTask(context.Background(), changed, changed.Version); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if got := notifier.Last(); !strings.Contains(got, approvalRCCommit) || strings.Contains(got, schedulerNewRCCommit) {
		t.Fatalf("approval replay = %q, want original bound commit", got)
	}
}

func approvalTask(t *testing.T, db *store.Store, id string, status model.Status, taskCommit, rcCommit string) *model.Task {
	t.Helper()
	now := time.Now().UTC()
	task := &model.Task{
		ID: id, ProjectID: "orders", GroupID: "g1", CreatorID: "u1", Requirement: "task",
		Status: status, Branch: "codex/" + id, TaskCommit: taskCommit, RCCommit: rcCommit,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func mustApprovalTask(t *testing.T, db *store.Store, id string) *model.Task {
	t.Helper()
	task, err := db.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestApprovalNotificationsStayBoundedAndRedacted(t *testing.T) {
	secret := "company-secret-value"
	logs := fakeExactLogs{secret: secret}
	svc, db, _, notifier := testServiceWithLogs(t, &fakePlanner{result: planResult()}, logs)
	task := approvalTask(t, db, "T-000000001105", model.StatusAwaitingMergeApproval, schedulerTaskCommit, "")
	task.Summary = strings.Repeat("x", maxNotificationRunes) + secret
	if err := db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(context.Background(), msg("admin-redacted", "admin", "批准合并 #"+task.ID)); err != nil {
		t.Fatal(err)
	}
	if got := notifier.Last(); len([]rune(got)) > maxNotificationRunes || strings.Contains(got, secret) {
		t.Fatalf("approval notification is unsafe: %q", got)
	}
}

type fakeExactLogs struct{ secret string }

func (l fakeExactLogs) Summary(string, int) (string, error) { return "", nil }
func (l fakeExactLogs) RedactText(text string) string {
	return strings.ReplaceAll(text, l.secret, "[REDACTED]")
}
