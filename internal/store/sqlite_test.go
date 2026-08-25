package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"qqcodex/internal/model"
)

func TestOpenInitializesSchemaAndPragmas(t *testing.T) {
	t.Parallel()

	db, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}

	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"foreign_keys": "1",
		"busy_timeout": "5000",
	} {
		var got string
		if err := db.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatalf("read PRAGMA %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskPersistenceAndOptimisticSave(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	now := time.UnixMilli(1_787_600_123_000).UTC()
	task := testTask("task-1", model.StatusDraft, now)
	task.Version = 41

	if err := db.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if task.Version != 1 {
		t.Fatalf("created task version = %d, want 1", task.Version)
	}

	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, task) {
		t.Fatalf("reloaded task mismatch\ngot:  %#v\nwant: %#v", got, task)
	}
	if _, err := db.GetTask(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task error = %v, want ErrNotFound", err)
	}

	duplicate := *task
	duplicate.Version = 73
	if err := db.CreateTask(ctx, &duplicate); err == nil {
		t.Fatal("duplicate CreateTask succeeded")
	}
	if duplicate.Version != 73 {
		t.Fatalf("failed create changed version to %d", duplicate.Version)
	}

	saved, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved.ProjectID = "project-2"
	saved.GroupID = "group-2"
	saved.CreatorID = "creator-2"
	saved.Requirement = "updated requirement"
	saved.Plan = "updated plan"
	saved.Status = model.StatusAwaitingConfirmation
	saved.Branch = "task/task-1-updated"
	saved.Worktree = "/tmp/worktree-updated"
	saved.BaseCommit = "base-2"
	saved.TaskCommit = "task-commit-2"
	saved.RCCommit = "rc-2"
	saved.SessionID = "session-2"
	saved.Summary = "updated summary"
	saved.Failure = "updated failure"
	saved.UpdatedAt = now.Add(time.Second)
	if err := db.SaveTask(ctx, saved, saved.Version); err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 {
		t.Fatalf("saved task version = %d, want 2", saved.Version)
	}

	stale.Status = model.StatusCancelled
	staleVersion := stale.Version
	if err := db.SaveTask(ctx, stale, stale.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save error = %v, want ErrConflict", err)
	}
	if stale.Version != staleVersion {
		t.Fatalf("failed stale save changed version to %d", stale.Version)
	}

	got, err = db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("saved task mismatch\ngot:  %#v\nwant: %#v", got, saved)
	}
}

func TestListByStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	base := time.UnixMilli(1_787_600_000_000).UTC()
	tasks := []*model.Task{
		testTask("queued-later", model.StatusQueued, base.Add(2*time.Second)),
		testTask("running", model.StatusRunning, base),
		testTask("queued-first", model.StatusQueued, base.Add(time.Second)),
		testTask("queued-last", model.StatusQueued, base.Add(3*time.Second)),
	}
	for _, task := range tasks {
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.ListByStatus(ctx, model.StatusQueued, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []*model.Task{tasks[2], tasks[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListByStatus() = %#v, want %#v", got, want)
	}
	for _, limit := range []int{0, -1} {
		if _, err := db.ListByStatus(ctx, model.StatusQueued, limit); err == nil {
			t.Errorf("ListByStatus limit %d succeeded", limit)
		}
	}
}

func TestWorkflowRecordsAndMessageDeduplication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	base := time.UnixMilli(1_787_600_000_000).UTC()
	for _, id := range []string{"task-1", "task-2"} {
		if err := db.CreateTask(ctx, testTask(id, model.StatusDraft, base)); err != nil {
			t.Fatal(err)
		}
	}

	input := model.Input{
		TaskID:    "task-1",
		Kind:      "confirmation",
		UserID:    "user-1",
		GroupID:   "group-1",
		MessageID: "input-message-1",
		Body:      "confirmed with additional context",
		CreatedAt: base.Add(time.Second),
	}
	if err := db.AppendInput(ctx, input); err != nil {
		t.Fatal(err)
	}
	var gotInput model.Input
	var inputCreatedAt int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT task_id, kind, user_id, group_id, message_id, body, created_at
		FROM task_inputs`).Scan(
		&gotInput.TaskID, &gotInput.Kind, &gotInput.UserID, &gotInput.GroupID,
		&gotInput.MessageID, &gotInput.Body, &inputCreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	gotInput.CreatedAt = time.UnixMilli(inputCreatedAt).UTC()
	if !reflect.DeepEqual(gotInput, input) {
		t.Fatalf("stored input = %#v, want %#v", gotInput, input)
	}

	approvals := []model.Approval{
		{TaskID: "task-1", Kind: "merge", UserID: "user-1", GroupID: "group-1", MessageID: "approval-1", BoundCommit: "commit-1", Result: "approved", CreatedAt: base.Add(2 * time.Second)},
		{TaskID: "task-1", Kind: "merge", UserID: "user-2", GroupID: "group-1", MessageID: "approval-2", BoundCommit: "commit-1", Result: "approved", CreatedAt: base.Add(3 * time.Second)},
		{TaskID: "task-1", Kind: "merge", UserID: "user-3", GroupID: "group-1", MessageID: "approval-3", BoundCommit: "commit-old", Result: "superseded", CreatedAt: base.Add(4 * time.Second)},
		{TaskID: "task-1", Kind: "deploy", UserID: "user-4", GroupID: "group-1", MessageID: "approval-4", BoundCommit: "rc-1", Result: "approved", CreatedAt: base.Add(5 * time.Second)},
		{TaskID: "task-2", Kind: "merge", UserID: "user-5", GroupID: "group-2", MessageID: "approval-5", BoundCommit: "commit-2", Result: "approved", CreatedAt: base.Add(6 * time.Second)},
	}
	for _, approval := range approvals {
		if err := db.AddApproval(ctx, approval); err != nil {
			t.Fatal(err)
		}
	}
	duplicate := approvals[0]
	duplicate.TaskID = "task-2"
	duplicate.UserID = "different-user"
	if err := db.AddApproval(ctx, duplicate); err == nil {
		t.Fatal("duplicate approval event succeeded")
	}
	var approvalCount int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals").Scan(&approvalCount); err != nil {
		t.Fatal(err)
	}
	if approvalCount != len(approvals) {
		t.Fatalf("approval count = %d, want %d", approvalCount, len(approvals))
	}
	var gotApproval model.Approval
	var approvalCreatedAt int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT task_id, kind, user_id, group_id, message_id, bound_commit, result, created_at
		FROM approvals WHERE message_id = ?`, approvals[0].MessageID).Scan(
		&gotApproval.TaskID, &gotApproval.Kind, &gotApproval.UserID, &gotApproval.GroupID,
		&gotApproval.MessageID, &gotApproval.BoundCommit, &gotApproval.Result, &approvalCreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	gotApproval.CreatedAt = time.UnixMilli(approvalCreatedAt).UTC()
	if !reflect.DeepEqual(gotApproval, approvals[0]) {
		t.Fatalf("stored approval = %#v, want %#v", gotApproval, approvals[0])
	}

	if err := db.InvalidateApprovals(ctx, "task-1", "merge", "superseded_by_new_commit"); err != nil {
		t.Fatal(err)
	}
	wantResults := map[string]string{
		"approval-1": "superseded_by_new_commit",
		"approval-2": "superseded_by_new_commit",
		"approval-3": "superseded",
		"approval-4": "approved",
		"approval-5": "approved",
	}
	rows, err := db.db.QueryContext(ctx, "SELECT message_id, result FROM approvals")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, result string
		if err := rows.Scan(&messageID, &result); err != nil {
			t.Fatal(err)
		}
		if result != wantResults[messageID] {
			t.Errorf("approval %s result = %q, want %q", messageID, result, wantResults[messageID])
		}
		delete(wantResults, messageID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(wantResults) != 0 {
		t.Fatalf("missing approvals after invalidation: %v", wantResults)
	}

	processed, err := db.MessageProcessed(ctx, "group-1/message-1")
	if err != nil {
		t.Fatal(err)
	}
	if processed {
		t.Fatal("missing message reported as processed")
	}
	processedAt := base.Add(7 * time.Second)
	if err := db.MarkMessageProcessed(ctx, "group-1/message-1", processedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageProcessed(ctx, "group-1/message-1", processedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	processed, err = db.MessageProcessed(ctx, "group-1/message-1")
	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("marked message reported as unprocessed")
	}
	var processedCount int
	var gotProcessedAt int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(processed_at) FROM processed_messages WHERE message_key = ?`,
		"group-1/message-1").Scan(&processedCount, &gotProcessedAt); err != nil {
		t.Fatal(err)
	}
	if processedCount != 1 || gotProcessedAt != processedAt.UnixMilli() {
		t.Fatalf("processed row = count %d, timestamp %d; want 1, %d", processedCount, gotProcessedAt, processedAt.UnixMilli())
	}

	auditAt := base.Add(8 * time.Second)
	if err := db.AppendAudit(ctx, "task-1", "state_changed", "draft -> awaiting_confirmation", auditAt); err != nil {
		t.Fatal(err)
	}
	var auditTaskID, auditKind, auditDetail string
	var gotAuditAt int64
	if err := db.db.QueryRowContext(ctx, `
		SELECT task_id, kind, detail, created_at FROM audit_events`).Scan(
		&auditTaskID, &auditKind, &auditDetail, &gotAuditAt,
	); err != nil {
		t.Fatal(err)
	}
	if auditTaskID != "task-1" || auditKind != "state_changed" || auditDetail != "draft -> awaiting_confirmation" || gotAuditAt != auditAt.UnixMilli() {
		t.Fatalf("stored audit = (%q, %q, %q, %d)", auditTaskID, auditKind, auditDetail, gotAuditAt)
	}
}

func TestListCleanupCandidates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cutoff := time.UnixMilli(1_787_600_100_000).UTC()
	cases := []struct {
		id      string
		status  model.Status
		updated time.Time
	}{
		{"failed-old", model.StatusFailed, cutoff.Add(-2 * time.Second)},
		{"cancelled-old", model.StatusCancelled, cutoff.Add(-time.Second)},
		{"failed-at-cutoff", model.StatusFailed, cutoff},
		{"cancelled-new", model.StatusCancelled, cutoff.Add(time.Second)},
		{"queued-old", model.StatusQueued, cutoff.Add(-time.Hour)},
		{"running-old", model.StatusRunning, cutoff.Add(-time.Hour)},
		{"merge-conflict-old", model.StatusMergeConflict, cutoff.Add(-time.Hour)},
		{"merged-old", model.StatusMerged, cutoff.Add(-time.Hour)},
		{"deploy-failed-old", model.StatusDeployFailed, cutoff.Add(-time.Hour)},
		{"deployed-old", model.StatusDeployed, cutoff.Add(-time.Hour)},
	}
	for _, tc := range cases {
		task := testTask(tc.id, tc.status, tc.updated.Add(-time.Minute))
		task.UpdatedAt = tc.updated
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.ListCleanupCandidates(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, len(got))
	for i, task := range got {
		gotIDs[i] = task.ID
	}
	wantIDs := []string{"failed-old", "cancelled-old"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("cleanup candidate IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	originalTime := time.UnixMilli(1_700_000_000_000).UTC()
	statuses := []model.Status{
		model.StatusDraft,
		model.StatusAwaitingConfirmation,
		model.StatusQueued,
		model.StatusRunning,
		model.StatusBlocked,
		model.StatusChecking,
		model.StatusPushed,
		model.StatusAwaitingMergeApproval,
		model.StatusMerging,
		model.StatusMergeConflict,
		model.StatusMerged,
		model.StatusAwaitingDeployApproval,
		model.StatusDeploying,
		model.StatusDeployFailed,
		model.StatusDeployed,
		model.StatusFailed,
		model.StatusCancelled,
	}
	for _, status := range statuses {
		task := testTask(string(status), status, originalTime)
		task.Failure = "existing failure"
		if err := db.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE tasks SET version = 7"); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Second)
	if err := db.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)

	type recovery struct {
		status  model.Status
		failure string
	}
	changed := map[model.Status]recovery{
		model.StatusRunning:   {model.StatusFailed, "service restarted during execution"},
		model.StatusChecking:  {model.StatusFailed, "service restarted during execution"},
		model.StatusPushed:    {model.StatusAwaitingMergeApproval, "existing failure"},
		model.StatusMerging:   {model.StatusFailed, "service restarted during merge; inspect remote RC before retrying"},
		model.StatusMerged:    {model.StatusAwaitingDeployApproval, "existing failure"},
		model.StatusDeploying: {model.StatusDeployFailed, "service restarted during deploy; inspect RC before retrying"},
	}
	var recoveredAt time.Time
	for _, originalStatus := range statuses {
		got, err := db.GetTask(ctx, string(originalStatus))
		if err != nil {
			t.Fatal(err)
		}
		want, wasChanged := changed[originalStatus]
		if !wasChanged {
			if got.Status != originalStatus || got.Failure != "existing failure" || got.Version != 7 || !got.UpdatedAt.Equal(originalTime) {
				t.Errorf("unaffected %s changed: status=%s failure=%q version=%d updated=%v", originalStatus, got.Status, got.Failure, got.Version, got.UpdatedAt)
			}
			continue
		}
		if got.Status != want.status || got.Failure != want.failure || got.Version != 8 {
			t.Errorf("recovered %s = status=%s failure=%q version=%d; want status=%s failure=%q version=8", originalStatus, got.Status, got.Failure, got.Version, want.status, want.failure)
		}
		if got.UpdatedAt.Before(before) || got.UpdatedAt.After(after) {
			t.Errorf("recovered %s timestamp %v outside [%v, %v]", originalStatus, got.UpdatedAt, before, after)
		}
		if recoveredAt.IsZero() {
			recoveredAt = got.UpdatedAt
		} else if !got.UpdatedAt.Equal(recoveredAt) {
			t.Errorf("recovered %s at %v, want shared timestamp %v", originalStatus, got.UpdatedAt, recoveredAt)
		}
	}
}

func TestRecoverInterruptedRollsBackOnFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	originalTime := time.UnixMilli(1_700_000_000_000).UTC()
	for _, status := range []model.Status{model.StatusRunning, model.StatusMerging} {
		if err := db.CreateTask(ctx, testTask(string(status), status, originalTime)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(ctx, `
		CREATE TRIGGER reject_merging_recovery
		BEFORE UPDATE ON tasks
		WHEN OLD.status = 'merging'
		BEGIN
			SELECT RAISE(ABORT, 'forced recovery failure');
		END;`); err != nil {
		t.Fatal(err)
	}

	if err := db.RecoverInterrupted(ctx); err == nil {
		t.Fatal("RecoverInterrupted succeeded despite rejecting trigger")
	}
	for _, status := range []model.Status{model.StatusRunning, model.StatusMerging} {
		got, err := db.GetTask(ctx, string(status))
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status || got.Version != 1 || !got.UpdatedAt.Equal(originalTime) {
			t.Errorf("task %s changed after rollback: %#v", status, got)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	db, err := Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return db
}

func testTask(id string, status model.Status, at time.Time) *model.Task {
	return &model.Task{
		ID:          id,
		ProjectID:   "project-1",
		GroupID:     "group-1",
		CreatorID:   "creator-1",
		Requirement: "implement the requested behavior",
		Plan:        "write tests, then code",
		Status:      status,
		Branch:      "task/" + id,
		Worktree:    "/tmp/worktrees/" + id,
		BaseCommit:  "base-commit",
		TaskCommit:  "task-commit",
		RCCommit:    "rc-commit",
		SessionID:   "session-1",
		Summary:     "task summary",
		Failure:     "failure detail",
		CreatedAt:   at,
		UpdatedAt:   at,
	}
}
