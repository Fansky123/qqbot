package consultsvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/command"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/store"
	"qqcodex/internal/tasksvc"
)

const (
	maxMessageBytes = 16 << 10
	maxMessageRunes = 4000
	maxReplyBytes   = 1 << 20
	pollInterval    = 20 * time.Millisecond
	leaseBuffer     = time.Second
	failureReply    = "咨询暂时失败，请稍后重试。"
)

var (
	errUnauthorized = errors.New("user is not authorized for this group")
	errCollision    = errors.New("consultation identity collision")
	errInvalidReply = errors.New("invalid consultation reply")
)

// Asker runs one stateless, read-only Codex consultation.
type Asker interface {
	Ask(context.Context, codex.Request) (codex.Result, error)
}

// Service owns the independent consultation lifecycle.
type Service struct {
	registry   *config.Registry
	config     config.ConsultationConfig
	db         *store.Store
	authorizer auth.Authorizer
	runner     Asker
	notifier   tasksvc.Notifier
	redactor   tasksvc.TextRedactor
}

func NewService(registry *config.Registry, consultation config.ConsultationConfig, db *store.Store, authorizer auth.Authorizer, runner Asker, notifier tasksvc.Notifier, redactor tasksvc.TextRedactor) *Service {
	return &Service{
		registry: registry, config: consultation, db: db, authorizer: authorizer,
		runner: runner, notifier: notifier, redactor: redactor,
	}
}

// ConsultationID derives the stable identifier for a OneBot group message.
func ConsultationID(groupID, messageID string) string {
	digest := sha256.Sum256([]byte(groupID + "\x00" + messageID))
	return fmt.Sprintf("Q-%X", digest[:6])
}

func (s *Service) Handle(ctx context.Context, message tasksvc.Message) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if s == nil || s.registry == nil || s.db == nil || s.runner == nil || s.notifier == nil || s.redactor == nil || s.config.Workspace == "" || s.config.TimeoutSeconds <= 0 {
		return errors.New("consultation service is not configured")
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
	projectID, workingDir, err := s.route(parsed)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	token, err := newLeaseToken()
	if err != nil {
		return fatalStore(err)
	}
	consultation := &model.Consultation{
		ID: ConsultationID(message.GroupID, message.MessageID), GroupID: message.GroupID, MessageID: message.MessageID,
		UserID: message.UserID, ProjectID: projectID, Question: parsed.Body, LeaseToken: token,
		LeaseExpiresAt: now.Add(s.timeout() + leaseBuffer), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.CreateConsultation(ctx, consultation); err == nil {
		return s.runOwned(ctx, message.GroupID, consultation, workingDir)
	} else if !errors.Is(err, store.ErrConflict) {
		return fatalStore(err)
	}

	existing, err := s.db.GetConsultationByMessage(ctx, message.GroupID, message.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return errCollision
	}
	if err != nil {
		return fatalStore(err)
	}
	if !sameIdentity(existing, consultation) {
		return errCollision
	}
	return s.awaitOrReclaim(ctx, message.GroupID, consultation, workingDir)
}

func (s *Service) route(parsed command.Command) (projectID, workingDir string, err error) {
	switch parsed.Kind {
	case command.KindConsult:
		return "", s.config.Workspace, nil
	case command.KindProjectConsult:
		project, ok := s.registry.Project(parsed.ProjectAlias)
		if !ok {
			return "", "", fmt.Errorf("unknown project alias %q", parsed.ProjectAlias)
		}
		return project.ID, project.RepoPath, nil
	default:
		return "", "", errors.New("unsupported consultation command")
	}
}

func (s *Service) awaitOrReclaim(ctx context.Context, groupID string, expected *model.Consultation, workingDir string) error {
	for {
		current, err := s.db.GetConsultationByMessage(ctx, expected.GroupID, expected.MessageID)
		if err != nil {
			return fatalStore(err)
		}
		if !sameIdentity(current, expected) {
			return errCollision
		}
		if !current.CompletedAt.IsZero() {
			return s.send(ctx, groupID, current.Reply)
		}

		now := time.Now().UTC()
		if !current.LeaseExpiresAt.After(now) {
			token, err := newLeaseToken()
			if err != nil {
				return fatalStore(err)
			}
			leaseUntil := now.Add(s.timeout() + leaseBuffer)
			claimed, err := s.db.TryReclaimConsultation(ctx, current.ID, token, leaseUntil, now)
			if err != nil {
				return fatalStore(err)
			}
			if claimed {
				current.LeaseToken = token
				current.LeaseExpiresAt = leaseUntil
				current.UpdatedAt = now
				return s.runOwned(ctx, groupID, current, workingDir)
			}
			continue
		}

		wait := time.Until(current.LeaseExpiresAt)
		if wait > pollInterval {
			wait = pollInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) runOwned(ctx context.Context, groupID string, consultation *model.Consultation, workingDir string) error {
	askCtx, cancel := context.WithTimeout(ctx, s.timeout())
	result, askErr := s.runner.Ask(askCtx, codex.Request{
		TaskID: consultation.ID, WorkingDir: workingDir,
		Prompt: codex.ConsultationPrompt(consultation.ProjectID, consultation.Question), Timeout: s.timeout(),
	})
	if askErr == nil && askCtx.Err() != nil {
		askErr = askCtx.Err()
	}
	cancel()

	reply, replyErr := s.reply(result, askErr)
	cause := errors.Join(askErr, replyErr)
	if err := s.db.CompleteConsultation(ctx, consultation.ID, consultation.LeaseToken, reply, time.Now().UTC()); errors.Is(err, store.ErrConflict) {
		return s.awaitOrReclaim(ctx, groupID, consultation, workingDir)
	} else if err != nil {
		return errors.Join(cause, fatalStore(err))
	}
	return errors.Join(cause, s.send(ctx, groupID, reply))
}

func (s *Service) reply(result codex.Result, askErr error) (string, error) {
	if askErr != nil || result.Blocked || len(result.Final) > maxReplyBytes || strings.TrimSpace(result.Final) == "" {
		if askErr != nil {
			return failureReply, nil
		}
		return failureReply, errInvalidReply
	}
	reply := tasksvc.NotificationText(s.redactor, strings.TrimSpace(result.Final))
	if strings.TrimSpace(reply) == "" {
		return failureReply, errInvalidReply
	}
	return reply, nil
}

func (s *Service) send(ctx context.Context, groupID, reply string) error {
	if err := s.notifier.Send(ctx, groupID, reply); err != nil {
		return errors.Join(tasksvc.ErrNotificationDelivery, err)
	}
	return nil
}

func (s *Service) timeout() time.Duration {
	return time.Duration(s.config.TimeoutSeconds) * time.Second
}

func sameIdentity(left, right *model.Consultation) bool {
	return left.ID == right.ID && left.GroupID == right.GroupID && left.MessageID == right.MessageID &&
		left.UserID == right.UserID && left.ProjectID == right.ProjectID && left.Question == right.Question
}

func newLeaseToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("create consultation lease token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func fatalStore(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(tasksvc.ErrFatalStore, err)
}
