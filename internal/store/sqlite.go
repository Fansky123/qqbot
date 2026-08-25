package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"qqcodex/internal/model"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("optimistic update conflict")
	// ErrAlreadyProcessed reports that another transaction already claimed the message key.
	ErrAlreadyProcessed = errors.New("message already processed")
)

//go:embed schema.sql
var schema string

const taskColumns = `
	id, project_id, group_id, creator_id, requirement, plan, status, branch,
	worktree, base_commit, git_common_dir, task_commit, rc_commit, session_id, summary, failure,
	version, created_at, updated_at`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite store path: %w", err)
	}
	query := make(url.Values)
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "5000")
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolutePath),
		RawQuery: query.Encode(),
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite store: %w", err)
	}
	if err := ensureGitCommonDirColumn(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func ensureGitCommonDirColumn(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(tasks)")
	if err != nil {
		return fmt.Errorf("inspect task schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect task schema column: %w", err)
		}
		if name == "git_common_dir" {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close task schema inspection: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect task schema rows: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN git_common_dir TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate task Git common directory: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close sqlite store: %w", err)
	}
	return nil
}

func (s *Store) CreateTask(ctx context.Context, task *model.Task) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, project_id, group_id, creator_id, requirement, plan, status, branch,
			worktree, base_commit, git_common_dir, task_commit, rc_commit, session_id, summary, failure,
			version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		task.ID, task.ProjectID, task.GroupID, task.CreatorID, task.Requirement,
		task.Plan, task.Status, task.Branch, task.Worktree, task.BaseCommit,
		task.GitCommonDir, task.TaskCommit, task.RCCommit, task.SessionID, task.Summary, task.Failure,
		task.CreatedAt.UnixMilli(), task.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("create task %q: %w", task.ID, err)
	}
	task.Version = 1
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*model.Task, error) {
	task, err := scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get task %q: %w", id, err)
	}
	return task, nil
}

func (s *Store) SaveTask(ctx context.Context, task *model.Task, expectedVersion int64) error {
	result, err := updateTask(ctx, s.db, task, expectedVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save task %q rows affected: %w", task.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("save task %q at version %d: %w", task.ID, expectedVersion, ErrConflict)
	}
	task.Version = expectedVersion + 1
	return nil
}

// SaveTaskWithAudit atomically saves scheduler state and its audit record.
func (s *Store) SaveTaskWithAudit(ctx context.Context, task *model.Task, expectedVersion int64, auditKind, auditDetail string, at time.Time) error {
	return s.saveTaskWithAudit(ctx, task, expectedVersion, auditKind, auditDetail, at, "", "")
}

// SaveTaskWithAuditInvalidatingApprovals also invalidates approvals in the same transaction.
func (s *Store) SaveTaskWithAuditInvalidatingApprovals(ctx context.Context, task *model.Task, expectedVersion int64, auditKind, auditDetail string, at time.Time, approvalKind, approvalResult string) error {
	if approvalKind == "" || approvalResult == "" {
		return errors.New("approval kind and result are required")
	}
	return s.saveTaskWithAudit(ctx, task, expectedVersion, auditKind, auditDetail, at, approvalKind, approvalResult)
}

func (s *Store) saveTaskWithAudit(ctx context.Context, task *model.Task, expectedVersion int64, auditKind, auditDetail string, at time.Time, approvalKind, approvalResult string) error {
	if auditKind == "" || auditDetail == "" {
		return errors.New("audit kind and detail are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audited task save for %q: %w", task.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := updateTask(ctx, tx, task, expectedVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save task %q rows affected: %w", task.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("save task %q at version %d: %w", task.ID, expectedVersion, ErrConflict)
	}
	if approvalKind != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE approvals SET result = ?
			WHERE task_id = ? AND kind = ? AND result = 'approved'`, approvalResult, task.ID, approvalKind); err != nil {
			return fmt.Errorf("invalidate %q approvals for task %q: %w", approvalKind, task.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (task_id, kind, detail, created_at)
		VALUES (?, ?, ?, ?)`, task.ID, auditKind, auditDetail, at.UnixMilli()); err != nil {
		return fmt.Errorf("append audit for task %q: %w", task.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit audited task save for %q: %w", task.ID, err)
	}
	task.Version = expectedVersion + 1
	return nil
}

type taskMutationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func updateTask(ctx context.Context, execer taskMutationExecer, task *model.Task, expectedVersion int64) (sql.Result, error) {
	result, err := execer.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = ?, group_id = ?, creator_id = ?, requirement = ?, plan = ?,
			status = ?, branch = ?, worktree = ?, base_commit = ?, task_commit = ?,
			git_common_dir = ?, rc_commit = ?, session_id = ?, summary = ?, failure = ?, updated_at = ?,
			version = version + 1
		WHERE id = ? AND version = ?`,
		task.ProjectID, task.GroupID, task.CreatorID, task.Requirement, task.Plan,
		task.Status, task.Branch, task.Worktree, task.BaseCommit, task.TaskCommit,
		task.GitCommonDir, task.RCCommit, task.SessionID, task.Summary, task.Failure,
		task.UpdatedAt.UnixMilli(), task.ID, expectedVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("save task %q: %w", task.ID, err)
	}
	return result, nil
}

// CommitTaskMutation atomically saves a task and its command records.
func (s *Store) CommitTaskMutation(ctx context.Context, task *model.Task, expectedVersion int64, input model.Input, auditKind, auditDetail, messageKey string, at time.Time) error {
	return s.commitTaskMutation(ctx, task, expectedVersion, input, auditKind, auditDetail, messageKey, at, "", "")
}

// CommitTaskMutationInvalidatingApprovals also invalidates approvals in the transaction.
func (s *Store) CommitTaskMutationInvalidatingApprovals(ctx context.Context, task *model.Task, expectedVersion int64, input model.Input, auditKind, auditDetail, messageKey string, at time.Time, approvalKind, approvalResult string) error {
	return s.commitTaskMutation(ctx, task, expectedVersion, input, auditKind, auditDetail, messageKey, at, approvalKind, approvalResult)
}

func (s *Store) commitTaskMutation(ctx context.Context, task *model.Task, expectedVersion int64, input model.Input, auditKind, auditDetail, messageKey string, at time.Time, approvalKind, approvalResult string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task mutation for %q: %w", task.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := claimMessageTx(ctx, tx, messageKey, at); err != nil {
		return err
	}
	result, err := updateTask(ctx, tx, task, expectedVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save task %q rows affected: %w", task.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("save task %q at version %d: %w", task.ID, expectedVersion, ErrConflict)
	}
	if approvalKind != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE approvals SET result = ?
			WHERE task_id = ? AND kind = ? AND result = 'approved'`, approvalResult, task.ID, approvalKind); err != nil {
			return fmt.Errorf("invalidate %q approvals for task %q: %w", approvalKind, task.ID, err)
		}
	}
	if err := appendRecordsTx(ctx, tx, input, task.ID, auditKind, auditDetail, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task mutation for %q: %w", task.ID, err)
	}
	task.Version = expectedVersion + 1
	return nil
}

// CommitRecords atomically appends command records and marks the message processed.
func (s *Store) CommitRecords(ctx context.Context, input model.Input, auditKind, auditDetail, messageKey string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin command records for %q: %w", input.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := claimMessageTx(ctx, tx, messageKey, at); err != nil {
		return err
	}
	if err := appendRecordsTx(ctx, tx, input, input.TaskID, auditKind, auditDetail, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit command records for %q: %w", input.TaskID, err)
	}
	return nil
}

func claimMessageTx(ctx context.Context, tx *sql.Tx, messageKey string, at time.Time) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO processed_messages (message_key, processed_at)
		VALUES (?, ?)
		ON CONFLICT(message_key) DO NOTHING`, messageKey, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("claim message %q: %w", messageKey, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim message %q rows affected: %w", messageKey, err)
	}
	if rows != 1 {
		return fmt.Errorf("claim message %q: %w", messageKey, ErrAlreadyProcessed)
	}
	return nil
}

func appendRecordsTx(ctx context.Context, tx *sql.Tx, input model.Input, taskID, auditKind, auditDetail string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_inputs (task_id, kind, user_id, group_id, message_id, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, input.TaskID, input.Kind, input.UserID, input.GroupID, input.MessageID, input.Body, input.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("append input for task %q: %w", taskID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (task_id, kind, detail, created_at)
		VALUES (?, ?, ?, ?)`, taskID, auditKind, auditDetail, at.UnixMilli()); err != nil {
		return fmt.Errorf("append audit for task %q: %w", taskID, err)
	}
	return nil
}

func (s *Store) ListByStatus(ctx context.Context, status model.Status, limit int) ([]*model.Task, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("list tasks by status: limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE status = ?
		ORDER BY created_at, id
		LIMIT ?`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("list tasks with status %q: %w", status, err)
	}
	defer rows.Close()

	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, fmt.Errorf("list tasks with status %q: %w", status, err)
	}
	return tasks, nil
}

func (s *Store) ListCleanupCandidates(ctx context.Context, cutoff time.Time) ([]*model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE status IN (?, ?) AND updated_at < ?
		ORDER BY updated_at, id`,
		model.StatusFailed, model.StatusCancelled, cutoff.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("list cleanup candidates: %w", err)
	}
	defer rows.Close()

	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, fmt.Errorf("list cleanup candidates: %w", err)
	}
	return tasks, nil
}

func (s *Store) AppendInput(ctx context.Context, input model.Input) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_inputs (task_id, kind, user_id, group_id, message_id, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		input.TaskID, input.Kind, input.UserID, input.GroupID, input.MessageID,
		input.Body, input.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("append input for task %q: %w", input.TaskID, err)
	}
	return nil
}

func (s *Store) AddApproval(ctx context.Context, approval model.Approval) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (
			task_id, kind, user_id, group_id, message_id, bound_commit, result, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.TaskID, approval.Kind, approval.UserID, approval.GroupID,
		approval.MessageID, approval.BoundCommit, approval.Result,
		approval.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("add approval for task %q: %w", approval.TaskID, err)
	}
	return nil
}

func (s *Store) InvalidateApprovals(ctx context.Context, taskID, kind, result string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		SET result = ?
		WHERE task_id = ? AND kind = ? AND result = 'approved'`,
		result, taskID, kind,
	)
	if err != nil {
		return fmt.Errorf("invalidate %q approvals for task %q: %w", kind, taskID, err)
	}
	return nil
}

func (s *Store) MessageProcessed(ctx context.Context, key string) (bool, error) {
	var processed bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM processed_messages WHERE message_key = ?)`, key).Scan(&processed); err != nil {
		return false, fmt.Errorf("check processed message %q: %w", key, err)
	}
	return processed, nil
}

func (s *Store) MarkMessageProcessed(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO processed_messages (message_key, processed_at)
		VALUES (?, ?)
		ON CONFLICT(message_key) DO NOTHING`, key, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("mark message %q processed: %w", key, err)
	}
	return nil
}

func (s *Store) AppendAudit(ctx context.Context, taskID, kind, detail string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (task_id, kind, detail, created_at)
		VALUES (?, ?, ?, ?)`, taskID, kind, detail, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("append audit for task %q: %w", taskID, err)
	}
	return nil
}

// TryAcquireSchedulerLease atomically acquires an absent or expired task lease.
func (s *Store) TryAcquireSchedulerLease(ctx context.Context, taskID, owner string, now, expiresAt time.Time) (bool, error) {
	if taskID == "" || owner == "" || !expiresAt.After(now) {
		return false, errors.New("scheduler lease task, owner, and future expiry are required")
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO scheduler_leases (task_id, owner, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at
		WHERE scheduler_leases.expires_at <= ?`,
		taskID, owner, expiresAt.UTC().UnixMilli(), now.UTC().UnixMilli(),
	)
	if err != nil {
		return false, fmt.Errorf("acquire scheduler lease for %q: %w", taskID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acquire scheduler lease for %q rows affected: %w", taskID, err)
	}
	return rows == 1, nil
}

// RenewSchedulerLease extends only the matching owner's live claim.
func (s *Store) RenewSchedulerLease(ctx context.Context, taskID, owner string, expiresAt time.Time) (bool, error) {
	if taskID == "" || owner == "" || expiresAt.IsZero() {
		return false, errors.New("scheduler lease task, owner, and expiry are required")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE scheduler_leases SET expires_at = ?
		WHERE task_id = ? AND owner = ?`, expiresAt.UTC().UnixMilli(), taskID, owner)
	if err != nil {
		return false, fmt.Errorf("renew scheduler lease for %q: %w", taskID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew scheduler lease for %q rows affected: %w", taskID, err)
	}
	return rows == 1, nil
}

// ReleaseSchedulerLease removes only the matching owner's claim.
func (s *Store) ReleaseSchedulerLease(ctx context.Context, taskID, owner string) error {
	if taskID == "" || owner == "" {
		return errors.New("scheduler lease task and owner are required")
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM scheduler_leases WHERE task_id = ? AND owner = ?`, taskID, owner); err != nil {
		return fmt.Errorf("release scheduler lease for %q: %w", taskID, err)
	}
	return nil
}

func (s *Store) RecoverInterrupted(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin interrupted task recovery: %w", err)
	}
	defer tx.Rollback()

	recoveredAt := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = CASE status
				WHEN ? THEN ?
				WHEN ? THEN ?
				WHEN ? THEN ?
				WHEN ? THEN ?
				WHEN ? THEN ?
				WHEN ? THEN ?
			END,
			failure = CASE
				WHEN status IN (?, ?) THEN ?
				WHEN status = ? THEN ?
				WHEN status = ? THEN ?
				ELSE failure
			END,
			updated_at = ?,
			version = version + 1
		WHERE status IN (?, ?, ?, ?, ?)
		   OR (status = ? AND task_commit = '')`,
		model.StatusRunning, model.StatusFailed,
		model.StatusChecking, model.StatusFailed,
		model.StatusPushed, model.StatusAwaitingMergeApproval,
		model.StatusMerging, model.StatusFailed,
		model.StatusMerged, model.StatusAwaitingDeployApproval,
		model.StatusDeploying, model.StatusDeployFailed,
		model.StatusRunning, model.StatusChecking, "service restarted during execution",
		model.StatusMerging, "service restarted during merge; inspect remote RC before retrying",
		model.StatusDeploying, "service restarted during deploy; inspect RC before retrying",
		recoveredAt,
		model.StatusRunning, model.StatusPushed, model.StatusMerging,
		model.StatusMerged, model.StatusDeploying,
		model.StatusChecking,
		model.StatusChecking,
	)
	if err != nil {
		return fmt.Errorf("recover interrupted tasks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit interrupted task recovery: %w", err)
	}
	return nil
}

type taskScanner interface {
	Scan(...any) error
}

func scanTask(scanner taskScanner) (*model.Task, error) {
	var task model.Task
	var createdAt, updatedAt int64
	err := scanner.Scan(
		&task.ID, &task.ProjectID, &task.GroupID, &task.CreatorID,
		&task.Requirement, &task.Plan, &task.Status, &task.Branch, &task.Worktree,
		&task.BaseCommit, &task.GitCommonDir, &task.TaskCommit, &task.RCCommit, &task.SessionID,
		&task.Summary, &task.Failure, &task.Version, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	task.CreatedAt = time.UnixMilli(createdAt).UTC()
	task.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return &task, nil
}

func scanTasks(rows *sql.Rows) ([]*model.Task, error) {
	tasks := make([]*model.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}
