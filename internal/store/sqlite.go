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
)

//go:embed schema.sql
var schema string

const taskColumns = `
	id, project_id, group_id, creator_id, requirement, plan, status, branch,
	worktree, base_commit, task_commit, rc_commit, session_id, summary, failure,
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
	return &Store{db: db}, nil
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
			worktree, base_commit, task_commit, rc_commit, session_id, summary, failure,
			version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		task.ID, task.ProjectID, task.GroupID, task.CreatorID, task.Requirement,
		task.Plan, task.Status, task.Branch, task.Worktree, task.BaseCommit,
		task.TaskCommit, task.RCCommit, task.SessionID, task.Summary, task.Failure,
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
	result, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET project_id = ?, group_id = ?, creator_id = ?, requirement = ?, plan = ?,
			status = ?, branch = ?, worktree = ?, base_commit = ?, task_commit = ?,
			rc_commit = ?, session_id = ?, summary = ?, failure = ?, updated_at = ?,
			version = version + 1
		WHERE id = ? AND version = ?`,
		task.ProjectID, task.GroupID, task.CreatorID, task.Requirement, task.Plan,
		task.Status, task.Branch, task.Worktree, task.BaseCommit, task.TaskCommit,
		task.RCCommit, task.SessionID, task.Summary, task.Failure,
		task.UpdatedAt.UnixMilli(), task.ID, expectedVersion,
	)
	if err != nil {
		return fmt.Errorf("save task %q: %w", task.ID, err)
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
		WHERE status IN (?, ?, ?, ?, ?, ?)`,
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
		model.StatusRunning, model.StatusChecking, model.StatusPushed,
		model.StatusMerging, model.StatusMerged, model.StatusDeploying,
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
		&task.BaseCommit, &task.TaskCommit, &task.RCCommit, &task.SessionID,
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
