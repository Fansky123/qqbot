package consultsvc

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/store"
	"qqcodex/internal/tasksvc"
)

type fakeAsker struct {
	mu      sync.Mutex
	calls   []codex.Request
	result  codex.Result
	err     error
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (a *fakeAsker) Ask(ctx context.Context, request codex.Request) (codex.Result, error) {
	a.mu.Lock()
	a.calls = append(a.calls, request)
	a.mu.Unlock()
	if a.started != nil {
		a.once.Do(func() { close(a.started) })
	}
	if a.release != nil {
		select {
		case <-a.release:
		case <-ctx.Done():
			return codex.Result{}, ctx.Err()
		}
	}
	return a.result, a.err
}

func (a *fakeAsker) Calls() []codex.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]codex.Request(nil), a.calls...)
}

type fakeNotifier struct {
	mu       sync.Mutex
	groups   []string
	messages []string
	failNext int
}

func (n *fakeNotifier) Send(_ context.Context, groupID, message string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.groups = append(n.groups, groupID)
	n.messages = append(n.messages, message)
	if n.failNext > 0 {
		n.failNext--
		return errors.New("send failed")
	}
	return nil
}

func (n *fakeNotifier) Messages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.messages...)
}

type exactRedactor string

func (r exactRedactor) RedactText(text string) string {
	return strings.ReplaceAll(text, string(r), "[EXACT-REDACTED]")
}

func TestServiceRejectsUnauthorizedMessagesBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name    string
		message tasksvc.Message
	}{
		{name: "group", message: consultationMessage("other-group", "u1", "unauthorized-group", "hello")},
		{name: "user", message: consultationMessage("g1", "stranger", "unauthorized-user", "hello")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAsker{result: codex.Result{Final: "answer"}}
			svc, db, notifier, _ := newTestService(t, runner)

			if err := svc.Handle(context.Background(), test.message); err == nil {
				t.Fatal("unauthorized consultation succeeded")
			}
			if len(runner.Calls()) != 0 || len(notifier.Messages()) != 0 {
				t.Fatalf("unauthorized side effects: calls=%d messages=%v", len(runner.Calls()), notifier.Messages())
			}
			if _, err := db.GetConsultationByMessage(context.Background(), test.message.GroupID, test.message.MessageID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored unauthorized consultation: %v", err)
			}
		})
	}
}

func TestServiceRoutesGenericAndProjectConsultations(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		workspace func(config.Config) string
		projectID string
		question  string
	}{
		{name: "generic", text: "how should I split this task?", workspace: func(cfg config.Config) string { return cfg.Consultation.Workspace }, question: "how should I split this task?"},
		{name: "project", text: "问 [o] where is the handler?", workspace: func(cfg config.Config) string { return cfg.Projects[0].RepoPath }, projectID: "orders", question: "where is the handler?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAsker{result: codex.Result{Final: "answer"}}
			svc, _, notifier, cfg := newTestService(t, runner)
			message := consultationMessage("g1", "u1", "route-"+test.name, test.text)

			if err := svc.Handle(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			calls := runner.Calls()
			if len(calls) != 1 {
				t.Fatalf("Ask calls = %d, want 1", len(calls))
			}
			wantID := ConsultationID(message.GroupID, message.MessageID)
			if calls[0].TaskID != wantID || calls[0].WorkingDir != test.workspace(cfg) || calls[0].Prompt != codex.ConsultationPrompt(test.projectID, test.question) || calls[0].Timeout != 2*time.Second {
				t.Fatalf("Ask request = %#v", calls[0])
			}
			if got := notifier.Messages(); len(got) != 1 || got[0] != "answer" {
				t.Fatalf("notifications = %v", got)
			}
			if notifier.groups[0] != message.GroupID {
				t.Fatalf("notification group = %q, want %q", notifier.groups[0], message.GroupID)
			}
		})
	}
}

func TestServiceRejectsUnknownProjectAndUnsupportedTaskCommands(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "unknown project", text: "问 [missing] what is here?"},
		{name: "task create", text: "[orders] change the handler"},
		{name: "task status", text: "状态 #T-0123456789AB"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAsker{result: codex.Result{Final: "answer"}}
			svc, db, notifier, _ := newTestService(t, runner)
			message := consultationMessage("g1", "u1", "reject-"+test.name, test.text)

			if err := svc.Handle(context.Background(), message); err == nil {
				t.Fatal("unsupported consultation succeeded")
			}
			if len(runner.Calls()) != 0 || len(notifier.Messages()) != 0 {
				t.Fatalf("unsupported side effects: calls=%d messages=%v", len(runner.Calls()), notifier.Messages())
			}
			if _, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored rejected consultation: %v", err)
			}
		})
	}
}

func TestServiceSuccessUsesIndependentConsultationPersistence(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "answer"}}
	svc, db, _, cfg := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "stable-1", "hello")

	if got := ConsultationID(message.GroupID, message.MessageID); got != "Q-4FB00E5D7B11" {
		t.Fatalf("ConsultationID = %q", got)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	consultation, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if consultation.ID != ConsultationID(message.GroupID, message.MessageID) || consultation.ProjectID != "" || consultation.Reply != "answer" || consultation.CompletedAt.IsZero() {
		t.Fatalf("consultation = %#v", consultation)
	}
	if _, err := db.GetTask(context.Background(), consultation.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("consultation created task: %v", err)
	}
	processed, err := db.MessageProcessed(context.Background(), message.GroupID+"\x00"+message.MessageID)
	if err != nil || processed {
		t.Fatalf("consultation processed task message = %v, %v", processed, err)
	}
	for _, table := range []string{"tasks", "task_inputs", "audit_events", "processed_messages"} {
		if count := countRows(t, cfg.DatabasePath, table); count != 0 {
			t.Fatalf("%s rows = %d, want 0", table, count)
		}
	}
}

func TestServiceSanitizesBoundsAndRepairsConsultationReply(t *testing.T) {
	secret := "exact-secret"
	raw := "OPENAI_API_KEY=generic-secret Bearer bearer-secret " + secret + " bad:\xff " + strings.Repeat("界", 1400)
	runner := &fakeAsker{result: codex.Result{Final: raw}}
	svc, db, notifier, _ := newTestServiceWithRedactor(t, runner, exactRedactor(secret))
	message := consultationMessage("g1", "u1", "sanitize", "hello")

	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	got := notifier.Messages()[0]
	if !utf8.ValidString(got) || len([]rune(got)) != 1200 || !strings.HasSuffix(got, "…") {
		t.Fatalf("reply validity/bound = valid:%v runes:%d suffix:%q", utf8.ValidString(got), len([]rune(got)), got[len(got)-3:])
	}
	for _, leaked := range []string{"generic-secret", "bearer-secret", secret} {
		if strings.Contains(got, leaked) {
			t.Fatalf("reply leaked %q: %q", leaked, got)
		}
	}
	stored, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reply != got {
		t.Fatalf("stored reply differs from notification")
	}
}

func TestServiceReplaysCompletedConsultationWithoutAnotherAsk(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "durable answer"}}
	svc, _, notifier, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "duplicate", "hello")

	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls()) != 1 || len(notifier.Messages()) != 2 || notifier.Messages()[1] != "durable answer" {
		t.Fatalf("calls=%d notifications=%v", len(runner.Calls()), notifier.Messages())
	}
}

func TestServiceNotificationFailureRetryReplaysWithoutAnotherAsk(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "durable answer"}}
	svc, _, notifier, _ := newTestService(t, runner)
	notifier.failNext = 1
	message := consultationMessage("g1", "u1", "notify-retry", "hello")

	if err := svc.Handle(context.Background(), message); !errors.Is(err, tasksvc.ErrNotificationDelivery) {
		t.Fatalf("first error = %v, want ErrNotificationDelivery", err)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(runner.Calls()) != 1 || len(notifier.Messages()) != 2 {
		t.Fatalf("calls=%d notifications=%v", len(runner.Calls()), notifier.Messages())
	}
}

func TestServiceConcurrentDuplicateAcrossStoresCallsAskOnce(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "consultations.db")
	first := openTestStore(t, path)
	second := openTestStore(t, path)
	cfg := testConfig(t)
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	runner := &fakeAsker{result: codex.Result{Final: "answer"}, started: make(chan struct{}), release: release}
	notifier := &fakeNotifier{}
	authorizer := auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs)
	firstService := NewService(registry, cfg.Consultation, first, authorizer, runner, notifier, exactRedactor("never"))
	secondService := NewService(registry, cfg.Consultation, second, authorizer, runner, notifier, exactRedactor("never"))
	message := consultationMessage("g1", "u1", "concurrent", "hello")
	errs := make(chan error, 2)

	go func() { errs <- firstService.Handle(context.Background(), message) }()
	<-runner.started
	go func() { errs <- secondService.Handle(context.Background(), message) }()
	time.Sleep(80 * time.Millisecond)
	if len(runner.Calls()) != 1 {
		t.Fatalf("Ask calls while first owns lease = %d", len(runner.Calls()))
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.Calls()) != 1 {
		t.Fatalf("Ask calls = %d, want 1", len(runner.Calls()))
	}
}

func TestServiceReclaimsExpiredAttemptAndFencesStaleLease(t *testing.T) {
	runnerRelease := make(chan struct{})
	runner := &fakeAsker{result: codex.Result{Final: "new answer"}, started: make(chan struct{}), release: runnerRelease}
	svc, db, _, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "reclaim", "hello")
	now := time.Now().UTC()
	oldToken := "old-lease"
	if err := db.CreateConsultation(context.Background(), &model.Consultation{
		ID: ConsultationID(message.GroupID, message.MessageID), GroupID: message.GroupID, MessageID: message.MessageID,
		UserID: message.UserID, Question: message.Text, LeaseToken: oldToken, LeaseExpiresAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- svc.Handle(context.Background(), message) }()
	<-runner.started
	current, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if current.LeaseToken == oldToken {
		t.Fatal("expired lease token was not replaced")
	}
	if err := db.CompleteConsultation(context.Background(), current.ID, oldToken, "stale answer", time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale completion error = %v, want ErrConflict", err)
	}
	close(runnerRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServiceAskFailureStoresAndReplaysFixedReply(t *testing.T) {
	askErr := errors.New("provider exposed secret detail")
	runner := &fakeAsker{err: askErr}
	svc, db, notifier, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "ask-failure", "hello")

	if err := svc.Handle(context.Background(), message); !errors.Is(err, askErr) || errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("Ask error = %v", err)
	}
	if got := notifier.Messages(); len(got) != 1 || got[0] != failureReply {
		t.Fatalf("failure notification = %v", got)
	}
	stored, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reply != failureReply || stored.CompletedAt.IsZero() {
		t.Fatalf("stored failed consultation = %#v", stored)
	}
	if err := svc.Handle(context.Background(), message); err != nil {
		t.Fatalf("replay error = %v", err)
	}
	if len(runner.Calls()) != 1 || len(notifier.Messages()) != 2 {
		t.Fatalf("calls=%d notifications=%v", len(runner.Calls()), notifier.Messages())
	}
}

func TestServiceAskTimeoutStoresFixedReply(t *testing.T) {
	runner := &fakeAsker{err: context.DeadlineExceeded}
	svc, db, notifier, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "ask-timeout", "hello")

	if err := svc.Handle(context.Background(), message); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if got := notifier.Messages(); len(got) != 1 || got[0] != failureReply {
		t.Fatalf("timeout notification = %v", got)
	}
	stored, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reply != failureReply || stored.CompletedAt.IsZero() {
		t.Fatalf("stored timeout consultation = %#v", stored)
	}
}

func TestServiceCallerCancellationIsNotFatalStoreFailure(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeAsker{release: release}
	svc, _, _, _ := newTestService(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.Handle(ctx, consultationMessage("g1", "u1", "caller-canceled", "hello"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, tasksvc.ErrFatalStore) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestServiceInvalidRunnerRepliesUseFixedFailure(t *testing.T) {
	tests := []struct {
		name   string
		result codex.Result
	}{
		{name: "empty", result: codex.Result{Final: " \n "}},
		{name: "blocked", result: codex.Result{Final: "need access", Blocked: true}},
		{name: "oversized", result: codex.Result{Final: strings.Repeat("x", maxReplyBytes+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAsker{result: test.result}
			svc, db, notifier, _ := newTestService(t, runner)
			message := consultationMessage("g1", "u1", "invalid-"+test.name, "hello")

			if err := svc.Handle(context.Background(), message); err == nil {
				t.Fatal("invalid Ask reply returned nil error")
			}
			if got := notifier.Messages(); len(got) != 1 || got[0] != failureReply {
				t.Fatalf("invalid reply notification = %v", got)
			}
			stored, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID)
			if err != nil || stored.Reply != failureReply || stored.CompletedAt.IsZero() {
				t.Fatalf("stored invalid consultation = %#v, %v", stored, err)
			}
		})
	}
}

func TestServiceWrapsStoreFailureAsFatal(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "answer"}}
	svc, db, _, _ := newTestService(t, runner)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	err := svc.Handle(context.Background(), consultationMessage("g1", "u1", "store-failure", "hello"))
	if !errors.Is(err, tasksvc.ErrFatalStore) || len(runner.Calls()) != 0 {
		t.Fatalf("store error = %v calls=%d", err, len(runner.Calls()))
	}
}

func TestServiceRejectsOversizedMessage(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "answer"}}
	svc, db, notifier, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "huge-message", strings.Repeat("x", maxMessageBytes+1))

	if err := svc.Handle(context.Background(), message); err == nil {
		t.Fatal("oversized message succeeded")
	}
	if len(runner.Calls()) != 0 || len(notifier.Messages()) != 0 {
		t.Fatalf("oversized side effects: calls=%d notifications=%v", len(runner.Calls()), notifier.Messages())
	}
	if _, err := db.GetConsultationByMessage(context.Background(), message.GroupID, message.MessageID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stored oversized consultation: %v", err)
	}
}

func TestServiceRejectsDuplicateIdentityMismatch(t *testing.T) {
	runner := &fakeAsker{result: codex.Result{Final: "answer"}}
	svc, db, notifier, _ := newTestService(t, runner)
	message := consultationMessage("g1", "u1", "collision", "hello")
	now := time.Now().UTC()
	if err := db.CreateConsultation(context.Background(), &model.Consultation{
		ID: ConsultationID(message.GroupID, message.MessageID), GroupID: message.GroupID, MessageID: message.MessageID,
		UserID: "u2", Question: message.Text, LeaseToken: "lease", LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Handle(context.Background(), message); err == nil {
		t.Fatal("identity collision succeeded")
	}
	if len(runner.Calls()) != 0 || len(notifier.Messages()) != 0 {
		t.Fatalf("collision side effects: calls=%d notifications=%v", len(runner.Calls()), notifier.Messages())
	}
}

func newTestService(t *testing.T, runner *fakeAsker) (*Service, *store.Store, *fakeNotifier, config.Config) {
	return newTestServiceWithRedactor(t, runner, exactRedactor("never"))
}

func newTestServiceWithRedactor(t *testing.T, runner *fakeAsker, redactor tasksvc.TextRedactor) (*Service, *store.Store, *fakeNotifier, config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "consultations.db")
	db := openTestStore(t, path)
	cfg := testConfig(t)
	cfg.DatabasePath = path
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}
	return NewService(registry, cfg.Consultation, db, auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs), runner, notifier, redactor), db, notifier, cfg
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		MessageWorkers: 1,
		OneBot:         config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "bot", MessageRunes: 1200},
		DatabasePath:   filepath.Join(t.TempDir(), "db"), LogDir: t.TempDir(), WorktreeRoot: t.TempDir(),
		Consultation:    config.ConsultationConfig{Workspace: t.TempDir(), TimeoutSeconds: 2},
		AllowedGroupIDs: []string{"g1"}, EmployeeIDs: []string{"u1", "u2", "admin"}, AdminIDs: []string{"admin"},
		Codex: config.CodexConfig{Binary: "/bin/true"}, OpsCommand: []string{"/bin/true"},
		Projects: []config.Project{{
			ID: "orders", Aliases: []string{"orders", "o"}, RepoPath: t.TempDir(), BaseBranch: "main", RCBranch: "rc", Remote: "origin",
			Checks: [][]string{{"go", "test", "./..."}}, MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 1,
		}},
	}
}

func openTestStore(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func consultationMessage(groupID, userID, messageID, text string) tasksvc.Message {
	return tasksvc.Message{GroupID: groupID, UserID: userID, MessageID: messageID, Text: text, Mentioned: true}
}

func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
