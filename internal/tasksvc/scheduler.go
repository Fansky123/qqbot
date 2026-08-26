package tasksvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/gitwork"
	"qqcodex/internal/model"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
)

const (
	defaultSchedulerPollInterval = 2 * time.Second
	schedulerScanLimit           = 256
	notificationAttempts         = 2
	notificationTimeout          = 5 * time.Second
	pushLeaseTTL                 = 30 * time.Second
	pushLeaseRenewInterval       = 10 * time.Second
)

var (
	schedulerCommitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	schedulerTaskIDPattern = regexp.MustCompile(`^T-[A-F0-9]{12}$`)
	schedulerOwnerFallback atomic.Uint64
)

// Scheduler claims durable work and executes each task in an isolated worktree.
type Scheduler struct {
	registry  *config.Registry
	db        *store.Store
	runner    Runner
	worktrees Worktrees
	operator  Operator
	notifier  Notifier
	logs      TaskLogs
	logger    *slog.Logger
	poll      time.Duration
	wake      chan struct{}
	owner     string

	projectsMu sync.Mutex
	projects   map[string]*projectScheduler

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
	workers  sync.WaitGroup

	runMu   sync.Mutex
	running bool
}

type projectScheduler struct {
	slots     chan struct{}
	gitMu     sync.Mutex
	releaseMu sync.Mutex
}

func NewScheduler(
	registry *config.Registry,
	db *store.Store,
	runner Runner,
	worktrees Worktrees,
	operator Operator,
	notifier Notifier,
	logs TaskLogs,
	logger *slog.Logger,
	pollInterval time.Duration,
) *Scheduler {
	if pollInterval <= 0 {
		pollInterval = defaultSchedulerPollInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		registry: registry, db: db, runner: runner, worktrees: worktrees,
		operator: operator, notifier: notifier, logs: logs, logger: logger,
		poll: pollInterval, wake: make(chan struct{}, 1), projects: make(map[string]*projectScheduler),
		active: make(map[string]context.CancelFunc), owner: newSchedulerOwner(),
	}
}

// Run polls durable work until ctx is cancelled, then cancels and joins workers.
func (s *Scheduler) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scheduler context is required")
	}
	if err := s.validate(); err != nil {
		return err
	}
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return errors.New("scheduler is already running")
	}
	s.running = true
	s.runMu.Unlock()
	defer func() {
		s.cancelAll()
		s.workers.Wait()
		s.runMu.Lock()
		s.running = false
		s.runMu.Unlock()
	}()

	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		if err := s.scan(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("%w: scheduler scan failed", ErrFatalStore)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Wake requests an immediate non-blocking durable scan.
func (s *Scheduler) Wake() {
	if s == nil || s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Cancel propagates cancellation to an active worker. Durable status is owned by Service.
func (s *Scheduler) Cancel(taskID string) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	cancel := s.active[taskID]
	s.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Scheduler) validate() error {
	if s == nil || s.registry == nil || s.db == nil || s.runner == nil || s.worktrees == nil || s.operator == nil || s.notifier == nil || s.logs == nil || s.logger == nil {
		return errors.New("scheduler is not configured")
	}
	return nil
}

func (s *Scheduler) scan(ctx context.Context) error {
	queued, err := s.db.ListByStatus(ctx, model.StatusQueued, schedulerScanLimit)
	if err != nil {
		return err
	}
	for _, task := range queued {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		project, ok := s.registry.ProjectByID(task.ProjectID)
		if !ok {
			s.failTask(ctx, task.ID, errors.New("configured project is unavailable"))
			continue
		}
		state := s.projectState(project)
		select {
		case state.slots <- struct{}{}:
		default:
			continue
		}
		if !s.claim(ctx, task) {
			<-state.slots
			continue
		}
		s.startWorker(ctx, task, project, state)
	}
	validated, err := s.db.ListByStatus(ctx, model.StatusChecking, schedulerScanLimit)
	if err != nil {
		return err
	}
	for _, task := range validated {
		if task.TaskCommit == "" || s.isActive(task.ID) {
			continue
		}
		project, ok := s.registry.ProjectByID(task.ProjectID)
		if !ok {
			s.failTask(ctx, task.ID, errors.New("configured project is unavailable"))
			continue
		}
		state := s.projectState(project)
		select {
		case state.slots <- struct{}{}:
		default:
			continue
		}
		if !s.tryAcquirePushLease(ctx, task.ID) {
			<-state.slots
			continue
		}
		s.startProjectWorker(ctx, task.ID, state, func(workerCtx context.Context) {
			s.withAcquiredPushLease(workerCtx, task.ID, func(leaseCtx context.Context) {
				s.pushValidated(leaseCtx, task.ID, project, state)
			})
		})
	}
	pushed, err := s.db.ListByStatus(ctx, model.StatusPushed, schedulerScanLimit)
	if err != nil {
		return err
	}
	for _, task := range pushed {
		if s.isActive(task.ID) {
			continue
		}
		project, ok := s.registry.ProjectByID(task.ProjectID)
		if !ok {
			project = config.Project{ID: task.ProjectID, MaxConcurrent: 1}
		}
		state := s.projectState(project)
		select {
		case state.slots <- struct{}{}:
		default:
			continue
		}
		if !s.tryAcquirePushLease(ctx, task.ID) {
			<-state.slots
			continue
		}
		s.startProjectWorker(ctx, task.ID, state, func(workerCtx context.Context) {
			s.withAcquiredPushLease(workerCtx, task.ID, func(leaseCtx context.Context) {
				current, stopped := s.activeTask(leaseCtx, task.ID, model.StatusPushed)
				if stopped {
					return
				}
				if !ok {
					s.failTask(leaseCtx, task.ID, errors.New("configured project is unavailable"))
					return
				}
				if s.transition(leaseCtx, current, model.StatusAwaitingMergeApproval, false) {
					s.notify(leaseCtx, current.GroupID, s.completion(current, project))
				}
			})
		})
	}

	if err := s.scanApprovals(ctx, model.StatusMerging, "merge"); err != nil {
		return err
	}
	if err := s.scanApprovals(ctx, model.StatusDeploying, "deploy"); err != nil {
		return err
	}
	merged, err := s.db.ListByStatus(ctx, model.StatusMerged, schedulerScanLimit)
	if err != nil {
		return err
	}
	for _, task := range merged {
		if s.isActive(task.ID) {
			continue
		}
		if s.transition(ctx, task, model.StatusAwaitingDeployApproval, false) {
			s.notify(ctx, task.GroupID, deployApprovalNotification(task))
		}
	}
	return nil
}

func (s *Scheduler) scanApprovals(ctx context.Context, status model.Status, kind string) error {
	tasks, err := s.db.ListByStatus(ctx, status, schedulerScanLimit)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		project, available := s.registry.ProjectByID(task.ProjectID)
		if !available {
			project = config.Project{ID: task.ProjectID, MaxConcurrent: 1}
		}
		state := s.projectState(project)
		s.startApprovalWorker(ctx, task.ID, func(workerCtx context.Context) {
			state.releaseMu.Lock()
			defer state.releaseMu.Unlock()
			s.runApproval(workerCtx, task.ID, kind, project, state, available)
		})
	}
	return nil
}

func (s *Scheduler) startApprovalWorker(parent context.Context, taskID string, work func(context.Context)) {
	ctx, cancel := context.WithCancel(parent)
	s.activeMu.Lock()
	if s.active[taskID] != nil {
		s.activeMu.Unlock()
		cancel()
		return
	}
	s.active[taskID] = cancel
	s.activeMu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer func() {
			s.activeMu.Lock()
			delete(s.active, taskID)
			s.activeMu.Unlock()
		}()
		work(ctx)
	}()
}

func (s *Scheduler) runApproval(ctx context.Context, taskID, kind string, project config.Project, state *projectScheduler, available bool) {
	status := model.StatusMerging
	if kind == "deploy" {
		status = model.StatusDeploying
	}
	task, stopped := s.activeTask(ctx, taskID, status)
	if stopped {
		return
	}
	boundCommit := task.TaskCommit
	if kind == "deploy" {
		boundCommit = task.RCCommit
	}
	approval, err := s.db.ClaimApproval(ctx, taskID, kind, status, boundCommit, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("claim scheduled approval failed", "task_id", taskID, "kind", kind, "error", s.safeText(err.Error()))
		}
		return
	}
	if !schedulerCommitPattern.MatchString(boundCommit) {
		s.failApproval(ctx, task, *approval, approvalFailureStatus(kind), "failed", errors.New("approved commit is invalid"))
		return
	}
	if !available {
		s.failApproval(ctx, task, *approval, approvalFailureStatus(kind), "failed", errors.New("configured project is unavailable"))
		return
	}
	if kind == "deploy" && task.DeployKey != ops.DeployKey(project.ID, task.ID, approval.BoundCommit) {
		s.failApproval(ctx, task, *approval, model.StatusDeployFailed, "failed", errors.New("persisted deploy key does not match approved RC commit"))
		return
	}
	if kind == "merge" {
		s.runMerge(ctx, task, *approval, project, state)
		return
	}
	s.runDeploy(ctx, task, *approval, project, state)
}

func (s *Scheduler) runMerge(ctx context.Context, task *model.Task, approval model.Approval, project config.Project, state *projectScheduler) {
	state.gitMu.Lock()
	current, stopped := s.activeTask(ctx, task.ID, model.StatusMerging)
	if stopped {
		state.gitMu.Unlock()
		return
	}
	if current.TaskCommit != approval.BoundCommit {
		state.gitMu.Unlock()
		s.failApproval(ctx, current, approval, model.StatusMergeConflict, "commit_changed", errors.New("task commit changed after approval claim"))
		return
	}
	task = current
	rcCommit, err := s.operator.MergeRC(ctx, project.ID, task.ID, approval.BoundCommit)
	state.gitMu.Unlock()
	if err != nil {
		result := "failed"
		next := model.StatusFailed
		if errors.Is(err, ops.ErrTaskCommitChanged) {
			result = "commit_changed"
			next = model.StatusMergeConflict
		} else if errors.Is(err, ops.ErrMergeConflict) {
			result = "merge_conflict"
			next = model.StatusMergeConflict
		}
		s.failApproval(ctx, task, approval, next, result, err)
		return
	}
	if !schedulerCommitPattern.MatchString(rcCommit) {
		s.failApproval(ctx, task, approval, model.StatusFailed, "failed", errors.New("operator returned an invalid RC commit"))
		return
	}
	task.RCCommit = rcCommit
	task.DeployKey = ""
	if !s.finishApproval(ctx, task, approval, model.StatusMerged, "completed", nil) {
		return
	}
	if !s.transition(ctx, task, model.StatusAwaitingDeployApproval, false) {
		return
	}
	s.notify(ctx, task.GroupID, deployApprovalNotification(task))
}

func (s *Scheduler) runDeploy(ctx context.Context, task *model.Task, approval model.Approval, project config.Project, state *projectScheduler) {
	state.gitMu.Lock()
	current, stopped := s.activeTask(ctx, task.ID, model.StatusDeploying)
	if stopped {
		state.gitMu.Unlock()
		return
	}
	if current.RCCommit != approval.BoundCommit || current.DeployKey != ops.DeployKey(project.ID, task.ID, approval.BoundCommit) {
		state.gitMu.Unlock()
		s.failApproval(ctx, current, approval, model.StatusDeployFailed, "failed", errors.New("deploy target changed after approval claim"))
		return
	}
	task = current
	err := s.operator.DeployRC(ctx, project.ID, task.ID, approval.BoundCommit)
	state.gitMu.Unlock()
	if err == nil {
		if s.finishApproval(ctx, task, approval, model.StatusDeployed, "completed", nil) {
			s.notify(ctx, task.GroupID, "任务 #"+task.ID+" 已部署 RC 提交："+approval.BoundCommit)
		}
		return
	}
	if errors.Is(err, ops.ErrRCCommitChanged) {
		currentCommit := ops.ChangedCommit(err)
		if !schedulerCommitPattern.MatchString(currentCommit) {
			if s.finishApproval(ctx, task, approval, model.StatusDeployFailed, "failed", errors.New("operator returned an invalid changed RC commit")) {
				s.notify(ctx, task.GroupID, deployRetryNotification(task, "RC 部署失败：远端提交无效"))
			}
			return
		}
		task.RCCommit = currentCommit
		task.DeployKey = ""
		if s.finishApproval(ctx, task, approval, model.StatusAwaitingDeployApproval, "commit_changed", err) {
			s.notify(ctx, task.GroupID, deployRetryNotification(task, "RC 提交已变化，需要重新批准"))
		}
		return
	}
	if s.finishApproval(ctx, task, approval, model.StatusDeployFailed, "failed", err) {
		s.notify(ctx, task.GroupID, deployRetryNotification(task, "RC 部署失败："+bound(s.safeText(err.Error()))))
	}
}

func (s *Scheduler) finishApproval(ctx context.Context, task *model.Task, approval model.Approval, next model.Status, result string, cause error) bool {
	if ctx.Err() != nil || !model.CanTransition(task.Status, next) {
		return false
	}
	previous := task.Status
	version := task.Version
	task.Status = next
	task.Failure = ""
	if cause != nil {
		task.Failure = bound(s.safeText(cause.Error()))
	}
	task.UpdatedAt = time.Now().UTC()
	if err := s.db.CompleteApproval(ctx, task, version, approval, result, "scheduler_transition", transitionDetail(previous, next), task.UpdatedAt); err != nil {
		if ctx.Err() == nil {
			s.logger.Error("persist approval result failed", "task_id", task.ID, "kind", approval.Kind, "error", s.safeText(err.Error()))
		}
		return false
	}
	return true
}

func (s *Scheduler) failApproval(ctx context.Context, task *model.Task, approval model.Approval, next model.Status, result string, cause error) {
	if !s.finishApproval(ctx, task, approval, next, result, cause) {
		return
	}
	if approval.Kind == "deploy" {
		s.notify(ctx, task.GroupID, deployRetryNotification(task, "RC 部署失败："+bound(s.safeText(cause.Error()))))
		return
	}
	s.notify(ctx, task.GroupID, "任务 #"+task.ID+" 合并失败："+bound(s.safeText(cause.Error())))
}

func approvalFailureStatus(kind string) model.Status {
	if kind == "deploy" {
		return model.StatusDeployFailed
	}
	return model.StatusFailed
}

func deployApprovalNotification(task *model.Task) string {
	return bound(fmt.Sprintf("任务 #%s 已合并到 RC\nRC 提交：%s\n请回复：批准部署 #%s", task.ID, task.RCCommit, task.ID))
}

func deployRetryNotification(task *model.Task, reason string) string {
	fixed := fmt.Sprintf("任务 #%s\n原因：\nRC 提交：%s\n请回复：批准部署 #%s", task.ID, task.RCCommit, task.ID)
	budget := maxNotificationRunes - len([]rune(fixed))
	reason = truncateSection(reason, budget, "…（已省略）")
	return fmt.Sprintf("任务 #%s\n原因：%s\nRC 提交：%s\n请回复：批准部署 #%s", task.ID, reason, task.RCCommit, task.ID)
}

func (s *Scheduler) projectState(project config.Project) *projectScheduler {
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	state := s.projects[project.ID]
	if state == nil {
		state = &projectScheduler{slots: make(chan struct{}, project.MaxConcurrent)}
		s.projects[project.ID] = state
	}
	return state
}

func (s *Scheduler) claim(ctx context.Context, task *model.Task) bool {
	if task.Status != model.StatusQueued || !model.CanTransition(task.Status, model.StatusRunning) {
		return false
	}
	previous := task.Status
	version := task.Version
	task.Status = model.StatusRunning
	task.Failure = ""
	task.UpdatedAt = time.Now().UTC()
	err := s.db.SaveTaskWithAudit(ctx, task, version, "scheduler_transition", transitionDetail(previous, task.Status), task.UpdatedAt)
	if err == nil {
		return true
	}
	if !errors.Is(err, store.ErrConflict) && ctx.Err() == nil {
		s.logger.Error("claim task failed", "task_id", task.ID, "error", s.safeText(err.Error()))
	}
	return false
}

func (s *Scheduler) startWorker(parent context.Context, task *model.Task, project config.Project, state *projectScheduler) {
	s.startProjectWorker(parent, task.ID, state, func(ctx context.Context) {
		s.runTask(ctx, task.ID, project, state)
	})
}

func (s *Scheduler) startProjectWorker(parent context.Context, taskID string, state *projectScheduler, work func(context.Context)) {
	ctx, cancel := context.WithCancel(parent)
	s.activeMu.Lock()
	s.active[taskID] = cancel
	s.activeMu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer func() { <-state.slots }()
		defer func() {
			s.activeMu.Lock()
			delete(s.active, taskID)
			s.activeMu.Unlock()
		}()
		work(ctx)
	}()
}

func (s *Scheduler) runTask(ctx context.Context, taskID string, project config.Project, state *projectScheduler) {
	if task, stopped := s.activeTask(ctx, taskID, model.StatusRunning); stopped {
		return
	} else {
		s.notify(ctx, task.GroupID, "任务 #"+task.ID+" 已开始执行")
	}

	task, stopped := s.activeTask(ctx, taskID, model.StatusRunning)
	if stopped {
		return
	}
	prepared, resume, err := persistedPrepared(task)
	if err != nil {
		s.failTask(ctx, taskID, err)
		return
	}
	if !resume {
		state.gitMu.Lock()
		if current, stop := s.activeTask(ctx, taskID, model.StatusRunning); stop {
			state.gitMu.Unlock()
			return
		} else {
			task = current
		}
		syncErr := s.operator.Sync(ctx, project.ID)
		if syncErr != nil {
			state.gitMu.Unlock()
			s.failTask(ctx, taskID, fmt.Errorf("sync project: %w", syncErr))
			return
		}
		prepared, err = s.worktrees.Prepare(ctx, project, task.ID)
		state.gitMu.Unlock()
		if err != nil {
			s.failTask(ctx, taskID, fmt.Errorf("prepare worktree: %w", err))
			return
		}
		if err := validatePrepared(task.ID, prepared); err != nil {
			s.failTask(ctx, taskID, err)
			return
		}
		task.Worktree = prepared.Path
		task.Branch = prepared.Branch
		task.BaseCommit = prepared.BaseCommit
		task.GitCommonDir = prepared.GitCommonDir
		if !s.checkpoint(ctx, task, "prepared worktree") {
			return
		}
	}

	req := codex.Request{
		TaskID:       task.ID,
		WorkingDir:   prepared.Path,
		GitCommonDir: prepared.GitCommonDir,
		Prompt:       codex.ExecutionPrompt(*task, project.Checks),
		Timeout:      time.Duration(project.CodexTimeoutSeconds) * time.Second,
	}
	var result codex.Result
	if resume {
		req.SessionID = task.SessionID
		result, err = s.runner.Resume(ctx, req)
	} else {
		result, err = s.runner.Execute(ctx, req)
	}
	if err != nil {
		s.failTask(ctx, taskID, fmt.Errorf("codex execution: %w", err))
		return
	}
	if result.SessionID == "" {
		s.failTask(ctx, taskID, errors.New("codex execution returned no session ID"))
		return
	}
	task, stopped = s.activeTask(ctx, taskID, model.StatusRunning)
	if stopped {
		return
	}
	task.SessionID = result.SessionID
	task.Summary = bound(s.safeText(result.Final))
	if result.Blocked {
		reason := bound(s.safeText(result.BlockedReason))
		if reason == "" {
			reason = "Codex 请求补充任务信息"
		}
		task.Failure = reason
		if !s.transition(ctx, task, model.StatusBlocked, false) {
			return
		}
		s.notify(ctx, task.GroupID, "任务 #"+task.ID+" 暂停，需要补充信息："+reason+"。请回复：补充 #"+task.ID+" <补充内容>")
		return
	}
	if !s.checkpoint(ctx, task, "stored Codex session and summary") {
		return
	}
	if !s.transition(ctx, task, model.StatusChecking, false) {
		return
	}

	if err := s.worktrees.RunChecks(ctx, project, prepared.Path, s.logs.Writer(task.ID, "checks")); err != nil {
		s.failTask(ctx, taskID, fmt.Errorf("run checks: %w", err))
		return
	}
	if _, stopped := s.activeTask(ctx, taskID, model.StatusChecking); stopped {
		return
	}
	commit, err := s.worktrees.ValidateCommit(ctx, prepared)
	if err != nil {
		s.failTask(ctx, taskID, fmt.Errorf("validate task commit: %w", err))
		return
	}
	if !schedulerCommitPattern.MatchString(commit) {
		s.failTask(ctx, taskID, errors.New("validated task commit is invalid"))
		return
	}

	s.withPushLease(ctx, taskID, func(leaseCtx context.Context) {
		current, stopped := s.activeTask(leaseCtx, taskID, model.StatusChecking)
		if stopped {
			return
		}
		current.TaskCommit = commit
		if !s.checkpoint(leaseCtx, current, "stored validated task commit") {
			return
		}
		s.pushValidated(leaseCtx, taskID, project, state)
	})
}

func (s *Scheduler) pushValidated(ctx context.Context, taskID string, project config.Project, state *projectScheduler) {
	task, stopped := s.activeTask(ctx, taskID, model.StatusChecking)
	if stopped {
		return
	}
	prepared, resume, err := persistedPrepared(task)
	if err != nil || !resume {
		if err == nil {
			err = errors.New("validated task has incomplete execution state")
		}
		s.failTask(ctx, taskID, err)
		return
	}
	if !schedulerCommitPattern.MatchString(task.TaskCommit) {
		s.failTask(ctx, taskID, errors.New("persisted task commit is invalid"))
		return
	}

	state.gitMu.Lock()
	task, stopped = s.activeTask(ctx, taskID, model.StatusChecking)
	if stopped {
		state.gitMu.Unlock()
		return
	}
	err = s.operator.PushTask(ctx, project.ID, task.ID, prepared.Branch, task.TaskCommit)
	state.gitMu.Unlock()
	if err != nil {
		s.failTask(ctx, taskID, fmt.Errorf("push task branch: %w", err))
		return
	}
	if !s.transition(ctx, task, model.StatusPushed, true) {
		return
	}
	if !s.transition(ctx, task, model.StatusAwaitingMergeApproval, false) {
		return
	}
	s.notify(ctx, task.GroupID, s.completion(task, project))
}

func (s *Scheduler) withPushLease(parent context.Context, taskID string, work func(context.Context)) {
	if !s.tryAcquirePushLease(parent, taskID) {
		return
	}
	s.withAcquiredPushLease(parent, taskID, work)
}

func (s *Scheduler) tryAcquirePushLease(ctx context.Context, taskID string) bool {
	now := time.Now().UTC()
	acquired, err := s.db.TryAcquireSchedulerLease(ctx, taskID, s.owner, now, now.Add(pushLeaseTTL))
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("acquire task push lease failed", "task_id", taskID, "error", s.safeText(err.Error()))
		}
		return false
	}
	return acquired
}

func (s *Scheduler) withAcquiredPushLease(parent context.Context, taskID string, work func(context.Context)) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pushLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				expiresAt := time.Now().UTC().Add(pushLeaseTTL)
				renewed, renewErr := s.db.RenewSchedulerLease(ctx, taskID, s.owner, expiresAt)
				if renewErr != nil || !renewed {
					if ctx.Err() == nil {
						detail := "scheduler lease ownership was lost"
						if renewErr != nil {
							detail = renewErr.Error()
						}
						s.logger.Error("renew task push lease failed", "task_id", taskID, "error", s.safeText(detail))
					}
					cancel()
					return
				}
			}
		}
	}()
	work(ctx)
	cancel()
	<-done
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), notificationTimeout)
	if err := s.db.ReleaseSchedulerLease(releaseCtx, taskID, s.owner); err != nil {
		s.logger.Error("release task push lease failed", "task_id", taskID, "error", s.safeText(err.Error()))
	}
	releaseCancel()
}

func persistedPrepared(task *model.Task) (gitwork.Prepared, bool, error) {
	values := []string{task.Worktree, task.Branch, task.BaseCommit, task.GitCommonDir, task.SessionID}
	nonempty := 0
	for _, value := range values {
		if value != "" {
			nonempty++
		}
	}
	if nonempty == 0 {
		return gitwork.Prepared{}, false, nil
	}
	if nonempty != len(values) {
		return gitwork.Prepared{}, false, errors.New("persisted execution state is incomplete")
	}
	prepared := gitwork.Prepared{Path: task.Worktree, Branch: task.Branch, BaseCommit: task.BaseCommit, GitCommonDir: task.GitCommonDir}
	if err := validatePrepared(task.ID, prepared); err != nil {
		return gitwork.Prepared{}, false, err
	}
	return prepared, true, nil
}

func validatePrepared(taskID string, prepared gitwork.Prepared) error {
	if !schedulerTaskIDPattern.MatchString(taskID) {
		return errors.New("persisted task ID is invalid")
	}
	if !filepath.IsAbs(prepared.Path) || filepath.Clean(prepared.Path) != prepared.Path || filepath.Base(prepared.Path) != taskID {
		return errors.New("persisted worktree path is invalid")
	}
	if prepared.Branch != "codex/"+taskID {
		return errors.New("persisted task branch is invalid")
	}
	if !schedulerCommitPattern.MatchString(prepared.BaseCommit) {
		return errors.New("persisted base commit is invalid")
	}
	if !filepath.IsAbs(prepared.GitCommonDir) || filepath.Clean(prepared.GitCommonDir) != prepared.GitCommonDir {
		return errors.New("persisted Git common directory is invalid")
	}
	return nil
}

func (s *Scheduler) checkpoint(ctx context.Context, task *model.Task, detail string) bool {
	if ctx.Err() != nil {
		return false
	}
	version := task.Version
	task.UpdatedAt = time.Now().UTC()
	if err := s.db.SaveTaskWithAudit(ctx, task, version, "scheduler_checkpoint", detail, task.UpdatedAt); err != nil {
		if !s.stopped(ctx, task.ID) {
			s.failTask(ctx, task.ID, fmt.Errorf("persist scheduler checkpoint: %w", err))
		}
		return false
	}
	return true
}

func (s *Scheduler) transition(ctx context.Context, task *model.Task, next model.Status, invalidateMerge bool) bool {
	if ctx.Err() != nil || !model.CanTransition(task.Status, next) {
		return false
	}
	previous := task.Status
	version := task.Version
	task.Status = next
	task.UpdatedAt = time.Now().UTC()
	var err error
	if invalidateMerge {
		err = s.db.SaveTaskWithAuditInvalidatingApprovals(ctx, task, version, "scheduler_transition", transitionDetail(previous, next), task.UpdatedAt, "merge", "superseded_by_new_commit")
	} else {
		err = s.db.SaveTaskWithAudit(ctx, task, version, "scheduler_transition", transitionDetail(previous, next), task.UpdatedAt)
	}
	if err != nil {
		if !s.stopped(ctx, task.ID) {
			s.logger.Error("persist scheduler transition failed", "task_id", task.ID, "error", s.safeText(err.Error()))
		}
		return false
	}
	return true
}

func (s *Scheduler) failTask(ctx context.Context, taskID string, cause error) {
	if ctx.Err() != nil {
		return
	}
	task, err := s.db.GetTask(ctx, taskID)
	if err != nil || task.Status == model.StatusCancelled {
		return
	}
	if !model.CanTransition(task.Status, model.StatusFailed) {
		return
	}
	previous := task.Status
	version := task.Version
	failure := bound(s.safeText(cause.Error()))
	task.Status = model.StatusFailed
	task.Failure = failure
	task.UpdatedAt = time.Now().UTC()
	if err := s.db.SaveTaskWithAudit(ctx, task, version, "scheduler_transition", transitionDetail(previous, task.Status), task.UpdatedAt); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			s.logger.Error("persist task failure failed", "task_id", task.ID, "error", s.safeText(err.Error()))
		}
		return
	}
	s.notify(ctx, task.GroupID, "任务 #"+task.ID+" 执行失败："+failure)
}

func (s *Scheduler) activeTask(ctx context.Context, taskID string, status model.Status) (*model.Task, bool) {
	if ctx.Err() != nil {
		return nil, true
	}
	task, err := s.db.GetTask(ctx, taskID)
	if err != nil {
		s.logger.Error("reload scheduled task failed", "task_id", taskID, "error", s.safeText(err.Error()))
		return nil, true
	}
	return task, task.Status != status
}

func (s *Scheduler) stopped(ctx context.Context, taskID string) bool {
	if ctx.Err() != nil {
		return true
	}
	task, err := s.db.GetTask(ctx, taskID)
	return err == nil && task.Status == model.StatusCancelled
}

func (s *Scheduler) completion(task *model.Task, project config.Project) string {
	checks := make([]string, 0, len(project.Checks))
	for _, check := range project.Checks {
		checks = append(checks, strings.Join(check, " ")+" （通过）")
	}
	checksText := s.safeText(strings.Join(checks, "、"))
	summary := s.safeText(task.Summary)
	const omitted = "…（已省略）"
	fixed := fmt.Sprintf("任务 #%s 已推送\n分支：%s\n提交：%s\n检查：\n摘要：\n请回复：批准合并 #%s", task.ID, task.Branch, task.TaskCommit, task.ID)
	remaining := maxNotificationRunes - len([]rune(fixed))
	if remaining < 0 {
		remaining = 0
	}
	checksBudget := remaining / 2
	summaryBudget := remaining - checksBudget
	checksText = truncateSection(checksText, checksBudget, omitted)
	summary = truncateSection(summary, summaryBudget, omitted)
	return fmt.Sprintf("任务 #%s 已推送\n分支：%s\n提交：%s\n检查：%s\n摘要：%s\n请回复：批准合并 #%s",
		task.ID, task.Branch, task.TaskCommit, checksText, summary, task.ID)
}

func (s *Scheduler) notify(ctx context.Context, groupID, message string) {
	message = bound(s.safeText(message))
	var err error
	for range notificationAttempts {
		attemptCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
		err = s.notifier.Send(attemptCtx, groupID, message)
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}
	}
	s.logger.Error("scheduler notification failed", "error", s.safeText(err.Error()))
}

func (s *Scheduler) safeText(value string) string {
	return sanitizeText(s.logs, value)
}

func (s *Scheduler) cancelAll() {
	s.activeMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *Scheduler) isActive(taskID string) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.active[taskID] != nil
}

func newSchedulerOwner() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return hex.EncodeToString(token[:])
	}
	return fmt.Sprintf("fallback-%d-%d", time.Now().UTC().UnixNano(), schedulerOwnerFallback.Add(1))
}

func transitionDetail(previous, next model.Status) string {
	return string(previous) + " -> " + string(next)
}
