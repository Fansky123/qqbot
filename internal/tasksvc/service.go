package tasksvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/command"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/store"
)

const (
	maxNotificationRunes = 1200
	maxMessageBytes      = 16 << 10
	maxMessageRunes      = 4000
	maxPlanBytes         = 64 << 10
	maxPlanFieldRunes    = 4000
	maxSessionIDBytes    = 128
	maxRequirementBytes  = 256 << 10
)

var notificationSecretAssignment = regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD)\s*=\s*[^\s,;]+`)
var notificationBearer = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)

var (
	errUnauthorized = errors.New("user is not authorized for this group")
	errTaskAccess   = errors.New("user is not authorized for this task")
)

// Service translates authenticated group commands into durable task transitions.
type Service struct {
	registry   *config.Registry
	db         *store.Store
	authorizer auth.Authorizer
	planner    Planner
	scheduler  SchedulerControl
	notifier   Notifier
	logs       LogReader
	redactor   textRedactor
	locks      keyedLocks
}

type textRedactor interface {
	RedactText(string) string
}

func NewService(registry *config.Registry, db *store.Store, authorizer auth.Authorizer, planner Planner, scheduler SchedulerControl, notifier Notifier, logs LogReader) *Service {
	redactor, _ := logs.(textRedactor)
	return &Service{
		registry:   registry,
		db:         db,
		authorizer: authorizer,
		planner:    planner,
		scheduler:  scheduler,
		notifier:   notifier,
		logs:       logs,
		redactor:   redactor,
	}
}

// TaskID derives the stable task identifier for a OneBot group message.
func TaskID(groupID, messageID string) string {
	digest := sha256.Sum256([]byte(groupID + "\x00" + messageID))
	return fmt.Sprintf("T-%X", digest[:6])
}

func (s *Service) Handle(ctx context.Context, message Message) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if s == nil || s.registry == nil || s.db == nil || s.planner == nil || s.scheduler == nil || s.notifier == nil || s.logs == nil || s.redactor == nil {
		return errors.New("task service is not configured")
	}
	if message.GroupID == "" || message.UserID == "" || message.MessageID == "" {
		return errors.New("message group, user, and ID are required")
	}
	if !s.authorizer.AllowedGroup(message.GroupID) || s.authorizer.Role(message.UserID) == auth.RoleNone {
		return errUnauthorized
	}
	if len(message.Text) > maxMessageBytes || len([]rune(message.Text)) > maxMessageRunes {
		return errors.New("message is too large")
	}
	parsed, err := command.Parse(message.Text, message.Mentioned)
	if err != nil {
		return err
	}
	key := message.GroupID + "\x00" + message.MessageID
	unlock := s.locks.lock(key)
	defer unlock()
	taskKey := parsed.TaskID
	if parsed.Kind == command.KindCreate {
		taskKey = TaskID(message.GroupID, message.MessageID)
	}
	unlockTask := s.locks.lock("task\x00" + taskKey)
	defer unlockTask()

	processed, err := s.db.MessageProcessed(ctx, key)
	if err != nil {
		return err
	}
	if processed {
		return s.replayNotification(ctx, message, parsed)
	}

	switch parsed.Kind {
	case command.KindCreate:
		return s.create(ctx, message, parsed, key)
	case command.KindConfirm:
		return s.confirm(ctx, message, parsed, key)
	case command.KindSupplement:
		return s.supplement(ctx, message, parsed, key)
	case command.KindCancel:
		return s.cancel(ctx, message, parsed, key)
	case command.KindStatus:
		return s.status(ctx, message, parsed, key)
	case command.KindLog:
		return s.log(ctx, message, parsed, key)
	case command.KindApproveMerge:
		return errors.New("merge approval is not available yet")
	case command.KindApproveDeploy:
		return errors.New("deploy approval is not available yet")
	default:
		return errors.New("unsupported command")
	}
}

func (s *Service) create(ctx context.Context, message Message, parsed command.Command, key string) error {
	project, ok := s.registry.Project(parsed.ProjectAlias)
	if !ok {
		return fmt.Errorf("unknown project alias %q", parsed.ProjectAlias)
	}
	id := TaskID(message.GroupID, message.MessageID)
	now := time.Now().UTC()
	task := &model.Task{
		ID: id, ProjectID: project.ID, GroupID: message.GroupID, CreatorID: message.UserID,
		Requirement: parsed.Body, Status: model.StatusDraft, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.CreateTask(ctx, task); err != nil {
		if existing, getErr := s.db.GetTask(ctx, id); getErr == nil {
			if existing.GroupID != message.GroupID {
				return errors.New("task identifier collision")
			}
			if existing.CreatorID != message.UserID {
				return errTaskAccess
			}
			if existing.Status == model.StatusDraft {
				if existing.ProjectID != project.ID || existing.Requirement != parsed.Body {
					return errors.New("draft task does not match replayed create")
				}
				task = existing
			} else {
				if recordErr := s.commitRecords(ctx, message, key, existing.ID, "create", "existing task"); recordErr != nil {
					return recordErr
				}
				return s.notifyExisting(ctx, message.GroupID, existing.ID)
			}
		} else {
			return err
		}
	}

	result, planErr := s.planner.Plan(ctx, codex.Request{
		TaskID: id, WorkingDir: project.RepoPath,
		Prompt:  codex.PlanningPrompt(project.ID, parsed.Body, project.Checks),
		Timeout: time.Duration(project.CodexTimeoutSeconds) * time.Second,
	})
	if planErr != nil {
		if err := s.transition(task, model.StatusFailed); err != nil {
			return errors.Join(planErr, err)
		}
		task.Failure = "planning failed"
		if recordErr := s.commitMutation(ctx, message, key, task, task.Version, "create", "planning failed"); recordErr != nil {
			return errors.Join(planErr, recordErr)
		}
		_ = s.send(ctx, message.GroupID, "任务 #"+id+" 规划失败，请查看本地日志")
		return planErr
	}

	if len(result.Final) > maxPlanBytes || len(result.SessionID) > maxSessionIDBytes {
		err := errors.New("planning result is too large")
		if transitionErr := s.transition(task, model.StatusFailed); transitionErr != nil {
			return errors.Join(err, transitionErr)
		}
		task.Failure = "invalid planning result"
		if recordErr := s.commitMutation(ctx, message, key, task, task.Version, "create", "invalid planning result"); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return err
	}
	plan, err := codex.ParsePlan(result.Final)
	if err != nil {
		if transitionErr := s.transition(task, model.StatusFailed); transitionErr != nil {
			return errors.Join(err, transitionErr)
		}
		task.Failure = "invalid planning result"
		if recordErr := s.commitMutation(ctx, message, key, task, task.Version, "create", "invalid planning result"); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		_ = s.send(ctx, message.GroupID, "任务 #"+id+" 规划结果无效，请查看本地日志")
		return err
	}
	if err := validatePlan(plan); err != nil {
		if transitionErr := s.transition(task, model.StatusFailed); transitionErr != nil {
			return errors.Join(err, transitionErr)
		}
		task.Failure = "invalid planning result"
		if recordErr := s.commitMutation(ctx, message, key, task, task.Version, "create", "invalid planning result"); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return err
	}
	plan = s.sanitizePlan(plan)

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode task plan: %w", err)
	}
	task.Plan = string(planJSON)
	task.Summary = bound(plan.Summary)
	if err := s.transition(task, model.StatusAwaitingConfirmation); err != nil {
		return err
	}
	if err := s.commitMutation(ctx, message, key, task, task.Version, "create", "awaiting confirmation"); err != nil {
		return err
	}
	return s.send(ctx, message.GroupID, s.confirmation(task, plan))
}

func (s *Service) confirm(ctx context.Context, message Message, parsed command.Command, key string) error {
	task, err := s.loadOperable(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	if task.Status != model.StatusAwaitingConfirmation {
		return fmt.Errorf("task %s cannot be confirmed from %s", task.ID, task.Status)
	}
	if task.CreatorID != message.UserID {
		return errors.New("only the task creator can confirm")
	}
	version := task.Version
	if err := s.transition(task, model.StatusQueued); err != nil {
		return err
	}
	if err := s.commitMutation(ctx, message, key, task, version, "confirmation", "queued"); err != nil {
		return err
	}
	s.scheduler.Wake()
	return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已确认并排队")
}

func (s *Service) supplement(ctx context.Context, message Message, parsed command.Command, key string) error {
	task, err := s.loadOperable(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	if task.Status != model.StatusBlocked && task.Status != model.StatusAwaitingMergeApproval {
		return fmt.Errorf("task %s cannot accept supplement from %s", task.ID, task.Status)
	}
	task.Requirement = strings.TrimSpace(task.Requirement + "\n\n补充：" + parsed.Body)
	if len(task.Requirement) > maxRequirementBytes {
		return errors.New("task requirement is too large")
	}
	wake := false
	detail := "supplemented"
	if task.Status == model.StatusBlocked || task.Status == model.StatusAwaitingMergeApproval {
		if err := s.transition(task, model.StatusQueued); err != nil {
			return err
		}
		task.TaskCommit, task.RCCommit = "", ""
		wake = true
		detail = "supplemented and queued"
	}
	task.UpdatedAt = time.Now().UTC()
	version := task.Version
	if err := s.commitSupplementMutation(ctx, message, key, task, version, detail); err != nil {
		return err
	}
	if wake {
		s.scheduler.Wake()
	}
	return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已记录补充内容")
}

func (s *Service) cancel(ctx context.Context, message Message, parsed command.Command, key string) error {
	task, err := s.loadOperable(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	if task.Status == model.StatusMerged || task.Status == model.StatusAwaitingDeployApproval || task.Status == model.StatusDeploying || task.Status == model.StatusDeployed || task.Status == model.StatusDeployFailed {
		return fmt.Errorf("task %s cannot be cancelled after merge", task.ID)
	}
	if !model.CanTransition(task.Status, model.StatusCancelled) {
		return fmt.Errorf("task %s cannot be cancelled from %s", task.ID, task.Status)
	}
	running := task.Status == model.StatusRunning || task.Status == model.StatusChecking
	version := task.Version
	if err := s.transition(task, model.StatusCancelled); err != nil {
		return err
	}
	if err := s.commitMutation(ctx, message, key, task, version, "cancel", "cancelled"); err != nil {
		return err
	}
	if running {
		s.scheduler.Cancel(task.ID)
	}
	return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已取消")
}

func (s *Service) status(ctx context.Context, message Message, parsed command.Command, key string) error {
	task, err := s.loadOperable(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	if err := s.commitRecords(ctx, message, key, task.ID, "status", "status viewed"); err != nil {
		return err
	}
	return s.send(ctx, message.GroupID, statusText(task))
}

func (s *Service) log(ctx context.Context, message Message, parsed command.Command, key string) error {
	task, err := s.loadOperable(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	text, err := s.logs.Summary(task.ID, maxNotificationRunes-80)
	if err != nil {
		return err
	}
	if err := s.commitRecords(ctx, message, key, task.ID, "log", "log viewed"); err != nil {
		return err
	}
	return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 日志摘要：\n"+text)
}

func (s *Service) loadOperable(ctx context.Context, message Message, id string) (*model.Task, error) {
	task, err := s.loadTask(ctx, message, id)
	if err != nil {
		return nil, err
	}
	if !s.authorizer.CanOperate(message.UserID, task.CreatorID) {
		return nil, errTaskAccess
	}
	return task, nil
}

func (s *Service) loadTask(ctx context.Context, message Message, id string) (*model.Task, error) {
	task, err := s.db.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if task.GroupID != message.GroupID {
		return nil, errTaskAccess
	}
	return task, nil
}

func (s *Service) transition(task *model.Task, next model.Status) error {
	if !model.CanTransition(task.Status, next) {
		return fmt.Errorf("invalid task transition %s -> %s", task.Status, next)
	}
	task.Status = next
	task.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *Service) commitMutation(ctx context.Context, message Message, key string, task *model.Task, version int64, kind, detail string) error {
	now := time.Now().UTC()
	input := model.Input{TaskID: task.ID, Kind: kind, UserID: message.UserID, GroupID: message.GroupID, MessageID: message.MessageID, Body: message.Text, CreatedAt: now}
	return s.db.CommitTaskMutation(ctx, task, version, input, kind, bound(detail), key, now)
}

func (s *Service) commitSupplementMutation(ctx context.Context, message Message, key string, task *model.Task, version int64, detail string) error {
	now := time.Now().UTC()
	input := model.Input{TaskID: task.ID, Kind: "supplement", UserID: message.UserID, GroupID: message.GroupID, MessageID: message.MessageID, Body: message.Text, CreatedAt: now}
	return s.db.CommitTaskMutationInvalidatingApprovals(ctx, task, version, input, "supplement", bound(detail), key, now, "merge", "superseded_by_new_commit")
}

func (s *Service) commitRecords(ctx context.Context, message Message, key, taskID, kind, detail string) error {
	now := time.Now().UTC()
	input := model.Input{TaskID: taskID, Kind: kind, UserID: message.UserID, GroupID: message.GroupID, MessageID: message.MessageID, Body: message.Text, CreatedAt: now}
	return s.db.CommitRecords(ctx, input, kind, bound(detail), key, now)
}

func (s *Service) notifyExisting(ctx context.Context, groupID, id string) error {
	task, err := s.db.GetTask(ctx, id)
	if err != nil {
		return err
	}
	return s.send(ctx, groupID, "任务 #"+task.ID+" 已存在，当前状态："+string(task.Status))
}

func (s *Service) replayNotification(ctx context.Context, message Message, parsed command.Command) error {
	id := parsed.TaskID
	if parsed.Kind == command.KindCreate {
		id = TaskID(message.GroupID, message.MessageID)
	}
	task, err := s.loadOperable(ctx, message, id)
	if err != nil {
		return err
	}
	switch parsed.Kind {
	case command.KindCreate:
		if task.Status == model.StatusAwaitingConfirmation {
			var plan codex.Plan
			if err := json.Unmarshal([]byte(task.Plan), &plan); err != nil {
				return fmt.Errorf("decode stored task plan: %w", err)
			}
			return s.send(ctx, message.GroupID, s.confirmation(task, plan))
		}
		return s.notifyExisting(ctx, message.GroupID, task.ID)
	case command.KindConfirm:
		return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已确认并排队")
	case command.KindSupplement:
		return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已记录补充内容")
	case command.KindCancel:
		return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已取消")
	case command.KindStatus:
		return s.send(ctx, message.GroupID, statusText(task))
	case command.KindLog:
		text, err := s.logs.Summary(task.ID, maxNotificationRunes-80)
		if err != nil {
			return err
		}
		return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 日志摘要：\n"+text)
	default:
		return nil
	}
}

func validatePlan(plan codex.Plan) error {
	fields := append([]string{plan.Summary}, plan.Scope...)
	fields = append(fields, plan.Checks...)
	fields = append(fields, plan.Risks...)
	if len(fields) > 257 {
		return errors.New("planning result has too many fields")
	}
	for _, field := range fields {
		if len(field) > maxPlanBytes || len([]rune(field)) > maxPlanFieldRunes {
			return errors.New("planning result field is too large")
		}
	}
	return nil
}

func (s *Service) confirmation(task *model.Task, plan codex.Plan) string {
	const omitted = "…（已省略）"
	fixed := fmt.Sprintf("任务 #%s 规划完成\n项目：\n摘要：\n范围：\n检查：\n风险：\n请回复：确认 #%s", task.ID, task.ID)
	remaining := maxNotificationRunes - len([]rune(fixed))
	if remaining < 0 {
		remaining = 0
	}
	values := []string{
		s.sanitize(task.ProjectID),
		s.sanitize(plan.Summary),
		s.sanitize(strings.Join(plan.Scope, "、")),
		s.sanitize(strings.Join(plan.Checks, "、")),
		s.sanitize(strings.Join(plan.Risks, "、")),
	}
	budgets := make([]int, len(values))
	for i := range budgets {
		budgets[i] = remaining / len(values)
		if i < remaining%len(values) {
			budgets[i]++
		}
		values[i] = truncateSection(values[i], budgets[i], omitted)
	}
	return fmt.Sprintf("任务 #%s 规划完成\n项目：%s\n摘要：%s\n范围：%s\n检查：%s\n风险：%s\n请回复：确认 #%s", task.ID, values[0], values[1], values[2], values[3], values[4], task.ID)
}

func statusText(task *model.Task) string {
	parts := []string{"任务 #" + task.ID, "项目：" + task.ProjectID, "状态：" + string(task.Status)}
	if task.Summary != "" {
		parts = append(parts, "摘要："+task.Summary)
	}
	if task.TaskCommit != "" {
		parts = append(parts, "任务提交："+task.TaskCommit)
	}
	if task.RCCommit != "" {
		parts = append(parts, "RC 提交："+task.RCCommit)
	}
	if task.Failure != "" {
		parts = append(parts, "失败原因："+task.Failure)
	}
	return strings.Join(parts, "\n")
}

func (s *Service) send(ctx context.Context, groupID, text string) error {
	return s.notifier.Send(ctx, groupID, s.boundNotification(text))
}

func (s *Service) boundNotification(text string) string {
	return bound(s.sanitize(text))
}

func (s *Service) sanitize(text string) string {
	text = s.redactor.RedactText(text)
	text = notificationSecretAssignment.ReplaceAllString(text, "[REDACTED]")
	return notificationBearer.ReplaceAllString(text, "Bearer [REDACTED]")
}

func (s *Service) sanitizePlan(plan codex.Plan) codex.Plan {
	plan.Summary = s.sanitize(plan.Summary)
	for _, fields := range [][]string{plan.Scope, plan.Checks, plan.Risks} {
		for i := range fields {
			fields[i] = s.sanitize(fields[i])
		}
	}
	return plan
}

func truncateSection(text string, budget int, marker string) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= budget {
		return text
	}
	markerRunes := []rune(marker)
	if len(markerRunes) >= budget {
		return string(markerRunes[:budget])
	}
	return string(runes[:budget-len(markerRunes)]) + marker
}

func bound(text string) string {
	runes := []rune(text)
	if len(runes) > maxNotificationRunes {
		return string(runes[:maxNotificationRunes-1]) + "…"
	}
	return text
}

type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func (l *keyedLocks) lock(key string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*keyLock)
	}
	entry := l.m[key]
	if entry == nil {
		entry = &keyLock{}
		l.m[key] = entry
	}
	entry.refs++
	l.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.m, key)
		}
		l.mu.Unlock()
	}
}
