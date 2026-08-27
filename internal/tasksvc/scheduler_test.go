package tasksvc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/gitwork"
	"qqcodex/internal/model"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
)

const (
	schedulerBaseCommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	schedulerTaskCommit  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	schedulerNewRCCommit = "dddddddddddddddddddddddddddddddddddddddd"
)

type schedulerFixture struct {
	registry  *config.Registry
	db        *store.Store
	dbPath    string
	runner    *schedulerRunner
	worktrees *schedulerWorktrees
	operator  *schedulerOperator
	notifier  *schedulerNotifier
	logs      *schedulerLogs
	meta      *metadataProbe
	eventsLog *schedulerEvents
}

type schedulerEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *schedulerEvents) add(value string) {
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *schedulerEvents) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type schedulerRunner struct {
	mu           sync.Mutex
	projects     map[string]string
	releases     map[string]<-chan struct{}
	started      chan string
	finished     chan string
	executeReqs  []codex.Request
	resumeReqs   []codex.Request
	resultByTask map[string]codex.Result
	errByTask    map[string]error
	running      map[string]int
	maxRunning   map[string]int
	events       *schedulerEvents
}

func (r *schedulerRunner) Execute(ctx context.Context, req codex.Request) (codex.Result, error) {
	r.events.add("execute")
	r.mu.Lock()
	r.executeReqs = append(r.executeReqs, req)
	r.mu.Unlock()
	return r.run(ctx, req)
}

func (r *schedulerRunner) Resume(ctx context.Context, req codex.Request) (codex.Result, error) {
	r.events.add("resume")
	r.mu.Lock()
	r.resumeReqs = append(r.resumeReqs, req)
	r.mu.Unlock()
	return r.run(ctx, req)
}

func (r *schedulerRunner) run(ctx context.Context, req codex.Request) (codex.Result, error) {
	r.mu.Lock()
	project := r.projects[req.TaskID]
	r.running[project]++
	if r.running[project] > r.maxRunning[project] {
		r.maxRunning[project] = r.running[project]
	}
	release := r.releases[req.TaskID]
	result := r.resultByTask[req.TaskID]
	err := r.errByTask[req.TaskID]
	r.mu.Unlock()

	if r.started != nil {
		r.started <- req.TaskID
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil && result.SessionID == "" {
		result = codex.Result{SessionID: "session-" + req.TaskID, Final: "summary " + req.TaskID}
	}
	r.mu.Lock()
	r.running[project]--
	r.mu.Unlock()
	if r.finished != nil {
		r.finished <- req.TaskID
	}
	return result, err
}

func (r *schedulerRunner) calls() (execute, resume []codex.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]codex.Request(nil), r.executeReqs...), append([]codex.Request(nil), r.resumeReqs...)
}

func (r *schedulerRunner) max(project string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxRunning[project]
}

type metadataProbe struct {
	mu        sync.Mutex
	pending   map[string]bool
	active    map[string]int
	maxActive map[string]int
	violation error
}

func (p *metadataProbe) sync(project string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[project] || p.active[project] != 0 {
		p.violation = fmt.Errorf("metadata operation overlapped before sync for %s", project)
	}
	p.pending[project] = true
	p.active[project]++
	if p.active[project] > p.maxActive[project] {
		p.maxActive[project] = p.active[project]
	}
	p.active[project]--
}

func (p *metadataProbe) prepare(project string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pending[project] || p.active[project] != 0 {
		p.violation = fmt.Errorf("sync and prepare were not one critical section for %s", project)
	}
	p.active[project]++
	if p.active[project] > p.maxActive[project] {
		p.maxActive[project] = p.active[project]
	}
	p.active[project]--
	p.pending[project] = false
}

func (p *metadataProbe) push(project string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[project] || p.active[project] != 0 {
		p.violation = fmt.Errorf("push overlapped metadata preparation for %s", project)
	}
	p.active[project]++
	if p.active[project] > p.maxActive[project] {
		p.maxActive[project] = p.active[project]
	}
	p.active[project]--
}

func (p *metadataProbe) result() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxActive["p1"], p.violation
}

type schedulerWorktrees struct {
	mu          sync.Mutex
	meta        *metadataProbe
	events      *schedulerEvents
	checkStart  chan string
	checkBlock  map[string]<-chan struct{}
	prepareErr  map[string]error
	checkErr    map[string]error
	validateErr map[string]error
	prepared    map[string]gitwork.Prepared
	prepareCall map[string]int
}

func (w *schedulerWorktrees) Prepare(_ context.Context, project config.Project, taskID string) (gitwork.Prepared, error) {
	w.meta.prepare(project.ID)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepareCall[taskID]++
	w.events.add("prepare")
	if err := w.prepareErr[taskID]; err != nil {
		return gitwork.Prepared{}, err
	}
	prepared := gitwork.Prepared{
		Path:         filepath.Join("/worktrees", taskID),
		Branch:       "codex/" + taskID,
		BaseCommit:   schedulerBaseCommit,
		GitCommonDir: filepath.Join("/git", taskID),
	}
	w.prepared[taskID] = prepared
	return prepared, nil
}

func (w *schedulerWorktrees) RunChecks(ctx context.Context, _ config.Project, worktree string, output io.Writer) error {
	taskID := filepath.Base(worktree)
	w.mu.Lock()
	w.events.add("checks")
	block := w.checkBlock[taskID]
	err := w.checkErr[taskID]
	w.mu.Unlock()
	if _, writeErr := io.WriteString(output, "check output "+taskID); writeErr != nil {
		return writeErr
	}
	if w.checkStart != nil {
		w.checkStart <- taskID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (w *schedulerWorktrees) ValidateCommit(_ context.Context, prepared gitwork.Prepared) (string, error) {
	taskID := filepath.Base(prepared.Path)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events.add("validate")
	if err := w.validateErr[taskID]; err != nil {
		return "", err
	}
	return schedulerTaskCommit, nil
}

type schedulerOperator struct {
	mu          sync.Mutex
	meta        *metadataProbe
	events      *schedulerEvents
	syncErr     map[string]error
	pushErr     map[string]error
	pushCalls   map[string]int
	pushBlock   map[string]<-chan struct{}
	pushStart   chan string
	afterPush   func(string)
	mergeErr    map[string]error
	mergeRC     map[string]string
	mergeCall   map[string]int
	mergeBlock  map[string]<-chan struct{}
	mergeStart  chan string
	deployErr   map[string]error
	deployCall  map[string]int
	deployBlock map[string]<-chan struct{}
	deployStart chan string
}

func (o *schedulerOperator) Sync(_ context.Context, projectID string) error {
	o.meta.sync(projectID)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events.add("sync")
	return o.syncErr[projectID]
}

func (o *schedulerOperator) PushTask(ctx context.Context, projectID, taskID, branch, commit string) error {
	o.meta.push(projectID)
	o.mu.Lock()
	o.events.add("push")
	o.pushCalls[taskID]++
	block := o.pushBlock[taskID]
	err := o.pushErr[taskID]
	afterPush := o.afterPush
	o.mu.Unlock()
	if branch != "codex/"+taskID || commit != schedulerTaskCommit {
		return fmt.Errorf("unexpected push %s %s", branch, commit)
	}
	if o.pushStart != nil {
		o.pushStart <- taskID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if afterPush != nil {
		afterPush(taskID)
	}
	return err
}

func (o *schedulerOperator) pushes(taskID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pushCalls[taskID]
}

func (o *schedulerOperator) MergeRC(ctx context.Context, projectID, taskID, commit string) (string, error) {
	o.mu.Lock()
	o.mergeCall[taskID]++
	block := o.mergeBlock[taskID]
	start := o.mergeStart
	err := o.mergeErr[taskID]
	rcCommit := o.mergeRC[taskID]
	o.mu.Unlock()
	if projectID == "" || commit != schedulerTaskCommit {
		return "", errors.New("unexpected merge input")
	}
	if start != nil {
		start <- taskID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	return rcCommit, nil
}

func (o *schedulerOperator) DeployRC(ctx context.Context, projectID, taskID, commit string) error {
	o.mu.Lock()
	o.deployCall[taskID]++
	block := o.deployBlock[taskID]
	start := o.deployStart
	err := o.deployErr[taskID]
	o.mu.Unlock()
	if projectID == "" || commit != approvalRCCommit {
		return errors.New("unexpected deploy input")
	}
	if start != nil {
		start <- taskID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (o *schedulerOperator) approvalCalls(taskID string) (merge, deploy int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.mergeCall[taskID], o.deployCall[taskID]
}

type schedulerNotifier struct {
	mu       sync.Mutex
	events   *schedulerEvents
	messages []string
	err      error
}

func (n *schedulerNotifier) Send(_ context.Context, _ string, message string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, message)
	if strings.Contains(message, "批准合并") && n.events != nil {
		n.events.add("notify")
	}
	return n.err
}

func (n *schedulerNotifier) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.messages...)
}

type schedulerLogs struct {
	mu      sync.Mutex
	secret  string
	streams map[string]string
}

func (l *schedulerLogs) Writer(taskID, stream string) io.Writer {
	return schedulerLogWriter{logs: l, key: taskID + ":" + stream}
}

func (l *schedulerLogs) RedactText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	return strings.ReplaceAll(text, l.secret, "[REDACTED]")
}

func (l *schedulerLogs) text(taskID, stream string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streams[taskID+":"+stream]
}

type schedulerLogWriter struct {
	logs *schedulerLogs
	key  string
}

func (w schedulerLogWriter) Write(data []byte) (int, error) {
	w.logs.mu.Lock()
	w.logs.streams[w.key] += string(data)
	w.logs.mu.Unlock()
	return len(data), nil
}

func TestSchedulerSuccessPersistsExactSequenceAndOutput(t *testing.T) {
	fixture := newSchedulerFixture(t, 2)
	taskID := "T-000000000001"
	fixture.createTask(t, taskID, "p1")

	cancel, done := fixture.run(t)
	defer cancel()
	fixture.scheduler(t).Wake()
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	cancel()
	waitRun(t, done)

	task := mustTask(t, fixture.db, taskID)
	if task.Worktree != filepath.Join("/worktrees", taskID) || task.Branch != "codex/"+taskID || task.BaseCommit != schedulerBaseCommit || task.GitCommonDir != filepath.Join("/git", taskID) {
		t.Fatalf("persisted preparation = %#v", task)
	}
	if task.SessionID != "session-"+taskID || task.TaskCommit != schedulerTaskCommit {
		t.Fatalf("persisted execution result = %#v", task)
	}
	if got := fixture.logs.text(taskID, "checks"); !strings.Contains(got, "check output "+taskID) {
		t.Fatalf("checks log = %q", got)
	}
	wantCalls := []string{"sync", "prepare", "execute", "checks", "validate", "push", "notify"}
	if got := fixture.events(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("call order = %v, want %v", got, wantCalls)
	}
	wantTransitions := []string{
		"queued -> running",
		"running -> checking",
		"checking -> pushed",
		"pushed -> awaiting_merge_approval",
	}
	if got := schedulerTransitions(t, fixture.dbPath, taskID); !reflect.DeepEqual(got, wantTransitions) {
		t.Fatalf("transitions = %v, want %v", got, wantTransitions)
	}
	messages := strings.Join(fixture.notifier.all(), "\n")
	for _, want := range []string{task.Branch, task.TaskCommit, "go test ./...", task.Summary, "批准合并 #" + taskID} {
		if !strings.Contains(messages, want) {
			t.Errorf("notifications %q lack %q", messages, want)
		}
	}
}

func TestSchedulerConcurrencyAndProjectGitSerialization(t *testing.T) {
	fixture := newSchedulerFixture(t, 2)
	releases := make(map[string]chan struct{})
	projects := map[string]string{
		"T-000000000011": "p1",
		"T-000000000012": "p1",
		"T-000000000013": "p1",
		"T-000000000014": "p2",
	}
	for taskID, projectID := range projects {
		fixture.createTask(t, taskID, projectID)
		releases[taskID] = make(chan struct{})
		fixture.runner.releases[taskID] = releases[taskID]
	}

	cancel, done := fixture.run(t)
	defer cancel()
	started := map[string]bool{}
	for len(started) < 3 {
		select {
		case taskID := <-fixture.runner.started:
			started[taskID] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("initial runners = %v, want two p1 and one p2", started)
		}
	}
	p1Started := 0
	for taskID := range started {
		if projects[taskID] == "p1" {
			p1Started++
		}
	}
	if p1Started != 2 || !started["T-000000000014"] {
		t.Fatalf("initial runners = %v", started)
	}
	select {
	case taskID := <-fixture.runner.started:
		t.Fatalf("third same-project task started before a slot was free: %s", taskID)
	case <-time.After(100 * time.Millisecond):
	}
	for taskID := range started {
		if projects[taskID] == "p1" {
			close(releases[taskID])
			break
		}
	}
	var third string
	select {
	case third = <-fixture.runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("third same-project task did not start after a slot was free")
	}
	if projects[third] != "p1" || started[third] {
		t.Fatalf("unexpected fourth runner %q", third)
	}
	for taskID, release := range releases {
		if !isClosed(release) {
			close(release)
		}
		waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	}
	cancel()
	waitRun(t, done)

	if got := fixture.runner.max("p1"); got != 2 {
		t.Fatalf("p1 max Codex concurrency = %d, want 2", got)
	}
	if got := fixture.runner.max("p2"); got != 1 {
		t.Fatalf("p2 max Codex concurrency = %d, want 1", got)
	}
	if max, err := fixture.meta.result(); err != nil || max != 1 {
		t.Fatalf("metadata serialization max=%d error=%v", max, err)
	}
}

func TestSchedulerOptimisticClaimAcrossInstancesAndWakeStorm(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000021"
	fixture.createTask(t, taskID, "p1")
	release := make(chan struct{})
	fixture.runner.releases[taskID] = release

	secondDB, err := store.Open(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	first := fixture.scheduler(t)
	second := NewScheduler(fixture.registry, secondDB, fixture.runner, fixture.worktrees, fixture.operator, fixture.notifier, fixture.logs, slog.Default(), 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := runScheduler(first, ctx)
	secondDone := runScheduler(second, ctx)
	for range 50 {
		first.Wake()
		second.Wake()
	}
	select {
	case got := <-fixture.runner.started:
		if got != taskID {
			t.Fatalf("started task = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task was not claimed")
	}
	select {
	case duplicate := <-fixture.runner.started:
		t.Fatalf("task was claimed twice: %s", duplicate)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	for range 20 {
		first.Wake()
		second.Wake()
	}
	time.Sleep(50 * time.Millisecond)
	execute, resume := fixture.runner.calls()
	if len(execute)+len(resume) != 1 || fixture.operator.pushes(taskID) != 1 {
		t.Fatalf("execute=%d resume=%d pushes=%d", len(execute), len(resume), fixture.operator.pushes(taskID))
	}
	cancel()
	waitRun(t, firstDone)
	waitRun(t, secondDone)
}

func TestSchedulerResumeUsesPersistedGitCommonDirAndCumulativeRequirement(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000031"
	fixture.createTask(t, taskID, "p1")
	task := mustTask(t, fixture.db, taskID)
	task.Worktree = filepath.Join("/worktrees", taskID)
	task.Branch = "codex/" + taskID
	task.BaseCommit = schedulerBaseCommit
	task.GitCommonDir = filepath.Join("/git", taskID)
	task.SessionID = "session-existing"
	task.Requirement = "original requirement\n\n补充：new acceptance detail"
	if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}

	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	cancel()
	waitRun(t, done)
	execute, resume := fixture.runner.calls()
	if len(execute) != 0 || len(resume) != 1 {
		t.Fatalf("execute=%d resume=%d", len(execute), len(resume))
	}
	req := resume[0]
	if req.SessionID != "session-existing" || req.GitCommonDir != task.GitCommonDir || req.WorkingDir != task.Worktree {
		t.Fatalf("resume request = %#v", req)
	}
	if !strings.Contains(req.Prompt, "original requirement") || !strings.Contains(req.Prompt, "new acceptance detail") {
		t.Fatalf("resume prompt lost cumulative requirement: %q", req.Prompt)
	}
	fixture.worktrees.mu.Lock()
	prepareCalls := fixture.worktrees.prepareCall[taskID]
	fixture.worktrees.mu.Unlock()
	if prepareCalls != 0 {
		t.Fatalf("resume prepared a new worktree %d times", prepareCalls)
	}
}

func TestSchedulerRejectsInvalidPersistedExecutionState(t *testing.T) {
	tests := []struct {
		name   string
		taskID string
		mutate func(*model.Task)
	}{
		{name: "invalid task ID", taskID: "invalid-task", mutate: func(*model.Task) {}},
		{name: "wrong branch", taskID: "T-000000000041", mutate: func(task *model.Task) { task.Branch = "other" }},
		{name: "relative worktree", taskID: "T-000000000042", mutate: func(task *model.Task) { task.Worktree = task.ID }},
		{name: "relative Git common directory", taskID: "T-000000000043", mutate: func(task *model.Task) { task.GitCommonDir = ".git" }},
		{name: "invalid base commit", taskID: "T-000000000044", mutate: func(task *model.Task) { task.BaseCommit = "main" }},
		{name: "partial state", taskID: "T-000000000045", mutate: func(task *model.Task) { task.SessionID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			fixture.createTask(t, tt.taskID, "p1")
			task := mustTask(t, fixture.db, tt.taskID)
			task.Worktree = filepath.Join("/worktrees", tt.taskID)
			task.Branch = "codex/" + tt.taskID
			task.BaseCommit = schedulerBaseCommit
			task.GitCommonDir = filepath.Join("/git", tt.taskID)
			task.SessionID = "session-existing"
			tt.mutate(task)
			if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
				t.Fatal(err)
			}
			cancel, done := fixture.run(t)
			waitTaskStatus(t, fixture.db, tt.taskID, model.StatusFailed)
			cancel()
			waitRun(t, done)
			execute, resume := fixture.runner.calls()
			if len(execute)+len(resume) != 0 || fixture.operator.pushes(tt.taskID) != 0 {
				t.Fatalf("invalid state executed: execute=%d resume=%d pushes=%d", len(execute), len(resume), fixture.operator.pushes(tt.taskID))
			}
		})
	}
}

func TestSchedulerFailuresNeverAdvanceToApproval(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*schedulerFixture, string)
		wantPush  int
	}{
		{"runner", func(f *schedulerFixture, id string) { f.runner.errByTask[id] = errors.New("runner failed") }, 0},
		{"checks", func(f *schedulerFixture, id string) { f.worktrees.checkErr[id] = errors.New("checks failed") }, 0},
		{"dirty tree", func(f *schedulerFixture, id string) { f.worktrees.validateErr[id] = errors.New("worktree is dirty") }, 0},
		{"missing commit", func(f *schedulerFixture, id string) {
			f.worktrees.validateErr[id] = errors.New("task has no new commits")
		}, 0},
		{"push", func(f *schedulerFixture, id string) { f.operator.pushErr[id] = errors.New("push failed") }, 1},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			taskID := fmt.Sprintf("T-%012X", 100+i)
			fixture.createTask(t, taskID, "p1")
			tt.configure(fixture, taskID)
			cancel, done := fixture.run(t)
			waitTaskStatus(t, fixture.db, taskID, model.StatusFailed)
			cancel()
			waitRun(t, done)
			if got := fixture.operator.pushes(taskID); got != tt.wantPush {
				t.Fatalf("push calls = %d, want %d", got, tt.wantPush)
			}
			messages := strings.Join(fixture.notifier.all(), "\n")
			if strings.Contains(messages, "批准合并") {
				t.Fatalf("failure notification offered merge: %q", messages)
			}
		})
	}
}

func TestSchedulerPersistsBlockedResultForSupplement(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000151"
	fixture.createTask(t, taskID, "p1")
	fixture.runner.resultByTask[taskID] = codex.Result{
		SessionID:     "session-blocked",
		Final:         "waiting for a missing detail",
		Blocked:       true,
		BlockedReason: "need the deployment target",
	}
	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusBlocked)
	cancel()
	waitRun(t, done)

	task := mustTask(t, fixture.db, taskID)
	if task.SessionID != "session-blocked" || task.Failure != "need the deployment target" {
		t.Fatalf("blocked task = %#v, want session and reason persisted", task)
	}
	if fixture.operator.pushes(taskID) != 0 {
		t.Fatal("blocked task was pushed")
	}
	messages := strings.Join(fixture.notifier.all(), "\n")
	for _, want := range []string{"需要补充信息", "need the deployment target", "补充 #" + taskID} {
		if !strings.Contains(messages, want) {
			t.Fatalf("blocked notification lacks %q: %q", want, messages)
		}
	}
}

func TestSchedulerCancellationDuringRunnerAndChecksPreservesCancelled(t *testing.T) {
	tests := []struct {
		name       string
		waitStage  func(*schedulerFixture) string
		blockStage func(*schedulerFixture, string, chan struct{})
	}{
		{
			name: "runner",
			waitStage: func(f *schedulerFixture) string {
				return <-f.runner.started
			},
			blockStage: func(f *schedulerFixture, id string, release chan struct{}) {
				f.runner.releases[id] = release
			},
		},
		{
			name: "checks",
			waitStage: func(f *schedulerFixture) string {
				return <-f.worktrees.checkStart
			},
			blockStage: func(f *schedulerFixture, id string, release chan struct{}) {
				f.worktrees.checkBlock[id] = release
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			taskID := fmt.Sprintf("T-%012X", 200+i)
			fixture.createTask(t, taskID, "p1")
			release := make(chan struct{})
			tt.blockStage(fixture, taskID, release)
			scheduler := fixture.scheduler(t)
			ctx, stop := context.WithCancel(context.Background())
			done := runScheduler(scheduler, ctx)
			select {
			case got := <-stageChannel(fixture, tt.name):
				if got != taskID {
					t.Fatalf("stage task = %q", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("task did not reach cancellable stage")
			}
			task := mustTask(t, fixture.db, taskID)
			version := task.Version
			if !model.CanTransition(task.Status, model.StatusCancelled) {
				t.Fatalf("cannot cancel task from %s", task.Status)
			}
			task.Status = model.StatusCancelled
			task.UpdatedAt = time.Now().UTC()
			if err := fixture.db.SaveTask(context.Background(), task, version); err != nil {
				t.Fatal(err)
			}
			scheduler.Cancel(taskID)
			waitTaskStatus(t, fixture.db, taskID, model.StatusCancelled)
			close(release)
			stop()
			waitRun(t, done)
			if got := mustTask(t, fixture.db, taskID).Status; got != model.StatusCancelled {
				t.Fatalf("cancelled task rewritten to %s", got)
			}
			if fixture.operator.pushes(taskID) != 0 {
				t.Fatal("cancelled task was pushed")
			}
		})
	}
}

func TestSchedulerNotifierFailureDoesNotReplaySideEffectsAndMessagesAreSafe(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000301"
	fixture.createTask(t, taskID, "p1")
	fixture.runner.resultByTask[taskID] = codex.Result{
		SessionID: "session-safe",
		Final:     fixture.logs.secret + " OPENAI_API_KEY=visible Bearer visible " + string([]byte{0xff}) + strings.Repeat("长", 2000),
	}
	fixture.notifier.err = errors.New("notifier unavailable")
	scheduler := fixture.scheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := runScheduler(scheduler, ctx)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	for range 20 {
		scheduler.Wake()
	}
	time.Sleep(100 * time.Millisecond)
	execute, resume := fixture.runner.calls()
	if len(execute)+len(resume) != 1 || fixture.operator.pushes(taskID) != 1 {
		t.Fatalf("notifier failure replayed work: execute=%d resume=%d pushes=%d", len(execute), len(resume), fixture.operator.pushes(taskID))
	}
	for _, message := range fixture.notifier.all() {
		if !utf8.ValidString(message) || len([]rune(message)) > maxNotificationRunes {
			t.Fatalf("unsafe notification length/UTF-8: runes=%d valid=%v", len([]rune(message)), utf8.ValidString(message))
		}
		for _, raw := range []string{fixture.logs.secret, "OPENAI_API_KEY=visible", "Bearer visible"} {
			if strings.Contains(message, raw) {
				t.Fatalf("notification leaked %q: %q", raw, message)
			}
		}
	}
	cancel()
	waitRun(t, done)
}

func TestSchedulerGenericThenExactRedactionIsStableForFailureAndCompletion(t *testing.T) {
	fixture := newSchedulerFixture(t, 2)
	logs, err := tasklog.Open(t.TempDir(), []string{"REDACTED", "generic-R-secret"})
	if err != nil {
		t.Fatal(err)
	}
	input := "FOO_PASSWORD=value Bearer bearer-value REDACTED generic-R-secret"
	successID := "T-000000000351"
	failureID := "T-000000000352"
	fixture.createTask(t, successID, "p1")
	fixture.createTask(t, failureID, "p1")
	fixture.runner.resultByTask[successID] = codex.Result{SessionID: "session-safe", Final: input}
	fixture.runner.errByTask[failureID] = errors.New(input)
	scheduler := fixture.schedulerWithLogs(logs)
	ctx, cancel := context.WithCancel(context.Background())
	done := runScheduler(scheduler, ctx)
	waitTaskStatus(t, fixture.db, successID, model.StatusAwaitingMergeApproval)
	waitTaskStatus(t, fixture.db, failureID, model.StatusFailed)
	cancel()
	waitRun(t, done)

	outputs := fixture.notifier.all()
	success := mustTask(t, fixture.db, successID)
	failure := mustTask(t, fixture.db, failureID)
	outputs = append(outputs, success.Summary, failure.Failure)
	for i, output := range outputs {
		if !utf8.ValidString(output) {
			t.Fatalf("output %d is invalid UTF-8", i)
		}
		for _, secret := range []string{"REDACTED", "generic-R-secret"} {
			if strings.Contains(output, secret) {
				t.Fatalf("output %d leaked exact secret %q: %q", i, secret, output)
			}
		}
	}
}

func TestSchedulerCompletionPreservesApprovalCommandAtRuneLimit(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	scheduler := fixture.scheduler(t)
	project, ok := fixture.registry.ProjectByID("p1")
	if !ok {
		t.Fatal("project p1 missing")
	}
	project.Checks = [][]string{{"check", strings.Repeat("检查", 2000)}}
	task := &model.Task{
		ID: "T-000000000361", Branch: "codex/T-000000000361", TaskCommit: schedulerTaskCommit,
		Summary: strings.Repeat("摘要", 2000),
	}
	scheduler.notify(context.Background(), "group", scheduler.completion(task, project))
	messages := fixture.notifier.all()
	if len(messages) != 1 {
		t.Fatalf("notification calls = %d, want 1", len(messages))
	}
	message := messages[0]
	if len([]rune(message)) > maxNotificationRunes {
		t.Fatalf("completion runes = %d", len([]rune(message)))
	}
	for _, want := range []string{
		"分支：" + task.Branch,
		"提交：" + task.TaskCommit,
		"检查：",
		"摘要：",
		"请回复：批准合并 #" + task.ID,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("bounded completion lacks %q: %q", want, message)
		}
	}
}

func TestSchedulerCompletionListsEveryCheckAsPassed(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	scheduler := fixture.scheduler(t)
	project, ok := fixture.registry.ProjectByID("p1")
	if !ok {
		t.Fatal("project p1 missing")
	}
	project.Checks = [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
	task := &model.Task{ID: "T-000000000371", Branch: "codex/T-000000000371", TaskCommit: schedulerTaskCommit}
	message := scheduler.completion(task, project)
	for _, want := range []string{"go test ./... （通过）", "go vet ./... （通过）"} {
		if !strings.Contains(message, want) {
			t.Errorf("completion lacks %q: %q", want, message)
		}
	}
}

func TestSchedulerReconcilesValidatedAndPushedStatesAfterRestart(t *testing.T) {
	tests := []struct {
		name       string
		status     model.Status
		priorPush  int
		wantPushes int
	}{
		{name: "validated commit resumes exact push", status: model.StatusChecking, priorPush: 1, wantPushes: 2},
		{name: "pushed advances without another push", status: model.StatusPushed, priorPush: 1, wantPushes: 1},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			taskID := fmt.Sprintf("T-%012X", 370+i)
			fixture.createTask(t, taskID, "p1")
			task := mustTask(t, fixture.db, taskID)
			task.Status = tt.status
			task.Worktree = filepath.Join("/worktrees", taskID)
			task.Branch = "codex/" + taskID
			task.BaseCommit = schedulerBaseCommit
			task.GitCommonDir = filepath.Join("/git", taskID)
			task.SessionID = "session-existing"
			task.Summary = "persisted summary"
			task.TaskCommit = schedulerTaskCommit
			if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
				t.Fatal(err)
			}
			fixture.operator.pushCalls[taskID] = tt.priorPush

			cancel, done := fixture.run(t)
			waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
			cancel()
			waitRun(t, done)
			if got := fixture.operator.pushes(taskID); got != tt.wantPushes {
				t.Fatalf("pushes = %d, want %d", got, tt.wantPushes)
			}
			execute, resume := fixture.runner.calls()
			if len(execute)+len(resume) != 0 {
				t.Fatalf("reconciliation reran Codex: execute=%d resume=%d", len(execute), len(resume))
			}
		})
	}
}

func TestSchedulerReconciliationCannotStealVersionFromActivePush(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000381"
	fixture.createTask(t, taskID, "p1")
	task := mustTask(t, fixture.db, taskID)
	task.Status = model.StatusChecking
	task.Worktree = filepath.Join("/worktrees", taskID)
	task.Branch = "codex/" + taskID
	task.BaseCommit = schedulerBaseCommit
	task.GitCommonDir = filepath.Join("/git", taskID)
	task.SessionID = "session-existing"
	task.Summary = "persisted summary"
	task.TaskCommit = schedulerTaskCommit
	if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}
	pushRelease := make(chan struct{})
	fixture.operator.pushBlock[taskID] = pushRelease

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := runScheduler(fixture.scheduler(t), ctx)
	select {
	case got := <-fixture.operator.pushStart:
		if got != taskID {
			t.Fatalf("push task = %q", got)
		}
	case <-time.After(3 * time.Second):
		cancel()
		waitRun(t, firstDone)
		t.Fatal("first scheduler did not start push")
	}

	secondDB, err := store.Open(fixture.dbPath)
	if err != nil {
		close(pushRelease)
		cancel()
		waitRun(t, firstDone)
		t.Fatal(err)
	}
	second := NewScheduler(fixture.registry, secondDB, fixture.runner, fixture.worktrees, fixture.operator, fixture.notifier, fixture.logs, slog.Default(), 5*time.Millisecond)
	secondDone := runScheduler(second, ctx)
	for range 20 {
		second.Wake()
	}
	time.Sleep(100 * time.Millisecond)
	close(pushRelease)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	if got := fixture.operator.pushes(taskID); got != 1 {
		t.Errorf("active exact push was duplicated %d times", got)
	}
	cancel()
	waitRun(t, firstDone)
	waitRun(t, secondDone)
	if err := secondDB.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerPushedReconciliationRequiresLeaseBeforeVersionWrite(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000386"
	fixture.createTask(t, taskID, "p1")
	task := mustTask(t, fixture.db, taskID)
	task.Status = model.StatusPushed
	task.Worktree = filepath.Join("/worktrees", taskID)
	task.Branch = "codex/" + taskID
	task.BaseCommit = schedulerBaseCommit
	task.GitCommonDir = filepath.Join("/git", taskID)
	task.SessionID = "session-existing"
	task.Summary = "persisted summary"
	task.TaskCommit = schedulerTaskCommit
	if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}

	secondDB, err := store.Open(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := secondDB.Close(); err != nil {
			t.Errorf("close second scheduler store: %v", err)
		}
	})
	first := fixture.scheduler(t)
	second := NewScheduler(fixture.registry, secondDB, fixture.runner, fixture.worktrees, fixture.operator, fixture.notifier, fixture.logs, slog.Default(), 5*time.Millisecond)
	ctx := context.Background()
	if !first.tryAcquirePushLease(ctx, taskID) {
		t.Fatal("first scheduler failed to acquire pushed reconciliation lease")
	}
	release := make(chan struct{})
	started := make(chan struct{})
	errs := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		first.withAcquiredPushLease(ctx, taskID, func(leaseCtx context.Context) {
			close(started)
			for range 20 {
				if err := second.scan(leaseCtx); err != nil {
					errs <- err
					return
				}
			}
			close(release)
		})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first scheduler did not hold pushed lease")
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case <-release:
	}
	before := mustTask(t, secondDB, taskID)
	if before.Status != model.StatusPushed || before.Version != task.Version {
		t.Fatalf("second scheduler wrote without lease: %#v", before)
	}
	<-done
	if err := second.scan(ctx); err != nil {
		t.Fatal(err)
	}
	waitTaskStatus(t, secondDB, taskID, model.StatusAwaitingMergeApproval)
	if got := fixture.operator.pushes(taskID); got != 0 {
		t.Fatalf("pushed reconciliation performed an external push: %d", got)
	}
}

func TestSchedulerCrashAfterPushReconcilesPersistedExactCommit(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000391"
	fixture.createTask(t, taskID, "p1")
	ctx, cancel := context.WithCancel(context.Background())
	var cancelOnce sync.Once
	fixture.operator.afterPush = func(string) { cancelOnce.Do(cancel) }
	firstDone := runScheduler(fixture.scheduler(t), ctx)
	waitRun(t, firstDone)
	afterCrash := mustTask(t, fixture.db, taskID)
	if afterCrash.Status != model.StatusChecking || afterCrash.TaskCommit != schedulerTaskCommit {
		t.Fatalf("post-push crash checkpoint = %#v", afterCrash)
	}
	if got := fixture.operator.pushes(taskID); got != 1 {
		t.Fatalf("initial push calls = %d, want 1", got)
	}
	fixture.operator.mu.Lock()
	fixture.operator.afterPush = nil
	fixture.operator.mu.Unlock()

	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondDone := runScheduler(fixture.scheduler(t), secondCtx)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingMergeApproval)
	secondCancel()
	waitRun(t, secondDone)
	if got := fixture.operator.pushes(taskID); got != 2 {
		t.Fatalf("idempotent reconciliation pushes = %d, want 2 total", got)
	}
	execute, resume := fixture.runner.calls()
	if len(execute) != 1 || len(resume) != 0 {
		t.Fatalf("crash reconciliation reran Codex: execute=%d resume=%d", len(execute), len(resume))
	}
}

func TestSchedulerRunShutdownCancelsAndWaitsForWorkers(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000000401"
	fixture.createTask(t, taskID, "p1")
	fixture.runner.releases[taskID] = make(chan struct{})
	scheduler := fixture.scheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := runScheduler(scheduler, ctx)
	select {
	case <-fixture.runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	waitRun(t, done)
	select {
	case <-fixture.runner.finished:
	default:
		t.Fatal("Run returned before worker finished")
	}
	if got := fixture.operator.pushes(taskID); got != 0 {
		t.Fatalf("shutdown task pushes = %d", got)
	}
}

func TestMergeApprovalSchedulerPersistsExactRCCommit(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001111"
	fixture.createApprovedTask(t, taskID, model.StatusMerging, "merge", schedulerTaskCommit)
	fixture.operator.mergeRC[taskID] = approvalRCCommit

	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingDeployApproval)
	cancel()
	waitRun(t, done)

	task := mustTask(t, fixture.db, taskID)
	if task.RCCommit != approvalRCCommit {
		t.Fatalf("RC commit = %q, want %q", task.RCCommit, approvalRCCommit)
	}
	if task.DeployKey != "" {
		t.Fatalf("merge persisted deploy key before approval: %q", task.DeployKey)
	}
	approval, err := fixture.db.LatestApproval(context.Background(), taskID, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if approval.BoundCommit != schedulerTaskCommit || approval.Result != "completed" {
		t.Fatalf("merge approval = %#v", approval)
	}
	mergeCalls, deployCalls := fixture.operator.approvalCalls(taskID)
	if mergeCalls != 1 || deployCalls != 0 {
		t.Fatalf("approval calls merge=%d deploy=%d", mergeCalls, deployCalls)
	}
	messages := strings.Join(fixture.notifier.all(), "\n")
	if !strings.Contains(messages, approvalRCCommit) || !strings.Contains(messages, "批准部署 #"+taskID) {
		t.Fatalf("merge notification = %q", messages)
	}
}

func TestMergeApprovalCommitMismatchNeverPushesRC(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001112"
	fixture.createApprovedTask(t, taskID, model.StatusMerging, "merge", schedulerTaskCommit)
	fixture.operator.mergeErr[taskID] = ops.ErrTaskCommitChanged

	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusMergeConflict)
	cancel()
	waitRun(t, done)

	approval, err := fixture.db.LatestApproval(context.Background(), taskID, "merge")
	if err != nil {
		t.Fatal(err)
	}
	if approval.Result != "commit_changed" {
		t.Fatalf("merge mismatch approval result = %q", approval.Result)
	}
	if task := mustTask(t, fixture.db, taskID); task.RCCommit != "" {
		t.Fatalf("merge mismatch persisted RC commit %q", task.RCCommit)
	}
	mergeCalls, _ := fixture.operator.approvalCalls(taskID)
	if mergeCalls != 1 {
		t.Fatalf("merge mismatch calls = %d", mergeCalls)
	}
}

func TestMergeApprovalClassifiesConflictAndHelperFailure(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus model.Status
		wantResult string
	}{
		{"merge conflict", ops.ErrMergeConflict, model.StatusMergeConflict, "merge_conflict"},
		{"helper failure", errors.New("helper unavailable"), model.StatusFailed, "failed"},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			taskID := fmt.Sprintf("T-00000000112%d", i)
			fixture.createApprovedTask(t, taskID, model.StatusMerging, "merge", schedulerTaskCommit)
			fixture.operator.mergeErr[taskID] = test.err
			cancel, done := fixture.run(t)
			waitTaskStatus(t, fixture.db, taskID, test.wantStatus)
			cancel()
			waitRun(t, done)
			approval, err := fixture.db.LatestApproval(context.Background(), taskID, "merge")
			if err != nil {
				t.Fatal(err)
			}
			if approval.Result != test.wantResult {
				t.Fatalf("approval result = %q, want %q", approval.Result, test.wantResult)
			}
		})
	}
}

func TestSchedulerReconcilesMergedWithoutRepeatingExternalMerge(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001129"
	fixture.createTask(t, taskID, "p1")
	task := mustTask(t, fixture.db, taskID)
	task.Status = model.StatusMerged
	task.TaskCommit = schedulerTaskCommit
	task.RCCommit = approvalRCCommit
	if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}
	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingDeployApproval)
	cancel()
	waitRun(t, done)
	mergeCalls, deployCalls := fixture.operator.approvalCalls(taskID)
	if mergeCalls != 0 || deployCalls != 0 {
		t.Fatalf("merged reconciliation calls merge=%d deploy=%d", mergeCalls, deployCalls)
	}
}

func TestDeployApprovalSchedulerSuccessFailureAndNewRetry(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001113"
	fixture.createApprovedTask(t, taskID, model.StatusDeploying, "deploy", approvalRCCommit)
	fixture.operator.deployErr[taskID] = errors.New("deployment unavailable")

	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusDeployFailed)
	cancel()
	waitRun(t, done)
	approval, err := fixture.db.LatestApproval(context.Background(), taskID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if approval.Result != "failed" {
		t.Fatalf("failed deploy approval result = %q", approval.Result)
	}
	if got := mustTask(t, fixture.db, taskID).DeployKey; got != ops.DeployKey("p1", taskID, approvalRCCommit) {
		t.Fatalf("persisted deploy key = %q", got)
	}

	current := mustTask(t, fixture.db, taskID)
	version := current.Version
	current.Status = model.StatusDeploying
	current.Failure = ""
	current.UpdatedAt = time.Now().UTC()
	retry := model.Approval{TaskID: taskID, Kind: "deploy", UserID: "admin", GroupID: "group", MessageID: "retry-deploy", BoundCommit: approvalRCCommit, Result: "approved", CreatedAt: current.UpdatedAt}
	input := model.Input{TaskID: taskID, Kind: "approve_deploy", UserID: "admin", GroupID: "group", MessageID: retry.MessageID, Body: "retry", CreatedAt: current.UpdatedAt}
	if err := fixture.db.CommitApprovalMutation(context.Background(), current, version, retry, input, "deploy_approval", "retry", "group\x00retry-deploy", current.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	fixture.operator.mu.Lock()
	delete(fixture.operator.deployErr, taskID)
	fixture.operator.mu.Unlock()

	cancel, done = fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusDeployed)
	cancel()
	waitRun(t, done)
	_, deployCalls := fixture.operator.approvalCalls(taskID)
	if deployCalls != 2 {
		t.Fatalf("deploy calls = %d, want one per admin approval", deployCalls)
	}
}

func TestDeployApprovalRemoteRCChangeRequiresNewApproval(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001114"
	fixture.createApprovedTask(t, taskID, model.StatusDeploying, "deploy", approvalRCCommit)
	fixture.operator.deployErr[taskID] = ops.NewRCCommitChanged(schedulerNewRCCommit)

	cancel, done := fixture.run(t)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingDeployApproval)
	cancel()
	waitRun(t, done)

	approval, err := fixture.db.LatestApproval(context.Background(), taskID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if approval.Result != "commit_changed" {
		t.Fatalf("RC mismatch approval result = %q", approval.Result)
	}
	changed := mustTask(t, fixture.db, taskID)
	if changed.RCCommit != schedulerNewRCCommit || changed.DeployKey != "" {
		t.Fatalf("RC mismatch task = %#v", changed)
	}
	_, deployCalls := fixture.operator.approvalCalls(taskID)
	if deployCalls != 1 {
		t.Fatalf("RC mismatch deploy calls = %d", deployCalls)
	}
	time.Sleep(30 * time.Millisecond)
	_, deployCalls = fixture.operator.approvalCalls(taskID)
	if deployCalls != 1 {
		t.Fatalf("RC mismatch automatically retried: %d", deployCalls)
	}
}

func TestDeployApprovalIsNeverAutomaticallyReplayedAfterInterruption(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001116"
	fixture.createApprovedTask(t, taskID, model.StatusDeploying, "deploy", approvalRCCommit)
	fixture.operator.deployBlock[taskID] = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := runScheduler(fixture.scheduler(t), ctx)
	select {
	case <-fixture.operator.deployStart:
	case <-time.After(3 * time.Second):
		cancel()
		waitRun(t, done)
		t.Fatal("deploy did not start")
	}
	cancel()
	waitRun(t, done)
	if got := mustTask(t, fixture.db, taskID); got.Status != model.StatusDeploying {
		t.Fatalf("interrupted deploy task = %#v", got)
	}
	approval, err := fixture.db.LatestApproval(context.Background(), taskID, "deploy")
	if err != nil || approval.Result != "executing" {
		t.Fatalf("interrupted deploy approval = %#v, %v", approval, err)
	}

	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondDone := runScheduler(fixture.scheduler(t), secondCtx)
	time.Sleep(50 * time.Millisecond)
	secondCancel()
	waitRun(t, secondDone)
	_, deployCalls := fixture.operator.approvalCalls(taskID)
	if deployCalls != 1 {
		t.Fatalf("interrupted deploy automatically replayed %d times", deployCalls)
	}
}

func TestApprovalExecutionIsClaimedAcrossSchedulers(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskID := "T-000000001115"
	fixture.createApprovedTask(t, taskID, model.StatusMerging, "merge", schedulerTaskCommit)
	fixture.operator.mergeRC[taskID] = approvalRCCommit
	secondDB, err := store.Open(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := runScheduler(fixture.scheduler(t), ctx)
	second := NewScheduler(fixture.registry, secondDB, fixture.runner, fixture.worktrees, fixture.operator, fixture.notifier, fixture.logs, slog.Default(), 5*time.Millisecond)
	secondDone := runScheduler(second, ctx)
	waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingDeployApproval)
	cancel()
	waitRun(t, firstDone)
	waitRun(t, secondDone)
	mergeCalls, _ := fixture.operator.approvalCalls(taskID)
	if mergeCalls != 1 {
		t.Fatalf("cross-scheduler merge calls = %d", mergeCalls)
	}
}

func TestApprovalValidationFailuresDoNotCallOperatorAndNotify(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		status     model.Status
		mutate     func(*model.Task)
		wantStatus model.Status
	}{
		{
			name: "unknown merge project", kind: "merge", status: model.StatusMerging, wantStatus: model.StatusFailed,
			mutate: func(task *model.Task) { task.ProjectID = "missing" },
		},
		{
			name: "invalid deploy key", kind: "deploy", status: model.StatusDeploying, wantStatus: model.StatusDeployFailed,
			mutate: func(task *model.Task) { task.DeployKey = "wrong" },
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 1)
			taskID := fmt.Sprintf("T-00000000113%d", i)
			commit := schedulerTaskCommit
			if test.kind == "deploy" {
				commit = approvalRCCommit
			}
			fixture.createApprovedTask(t, taskID, test.status, test.kind, commit)
			task := mustTask(t, fixture.db, taskID)
			test.mutate(task)
			if err := fixture.db.SaveTask(context.Background(), task, task.Version); err != nil {
				t.Fatal(err)
			}
			cancel, done := fixture.run(t)
			waitTaskStatus(t, fixture.db, taskID, test.wantStatus)
			cancel()
			waitRun(t, done)
			mergeCalls, deployCalls := fixture.operator.approvalCalls(taskID)
			if mergeCalls != 0 || deployCalls != 0 {
				t.Fatalf("validation failure calls merge=%d deploy=%d", mergeCalls, deployCalls)
			}
			if len(fixture.notifier.all()) == 0 {
				t.Fatal("validation failure did not notify")
			}
		})
	}
}

func TestApprovalOperationsWaitForSameProjectGitMutation(t *testing.T) {
	for _, kind := range []string{"merge", "deploy"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newSchedulerFixture(t, 2)
			pushID := "T-000000001140"
			fixture.createTask(t, pushID, "p1")
			pushing := mustTask(t, fixture.db, pushID)
			pushing.Status = model.StatusChecking
			pushing.Worktree = filepath.Join("/worktrees", pushID)
			pushing.Branch = "codex/" + pushID
			pushing.BaseCommit = schedulerBaseCommit
			pushing.GitCommonDir = filepath.Join("/git", pushID)
			pushing.SessionID = "existing-session"
			pushing.TaskCommit = schedulerTaskCommit
			if err := fixture.db.SaveTask(context.Background(), pushing, pushing.Version); err != nil {
				t.Fatal(err)
			}
			pushRelease := make(chan struct{})
			fixture.operator.pushBlock[pushID] = pushRelease

			approvalID := "T-000000001141"
			status, commit := model.StatusMerging, schedulerTaskCommit
			if kind == "deploy" {
				status, commit = model.StatusDeploying, approvalRCCommit
			}
			fixture.createApprovedTask(t, approvalID, status, kind, commit)
			fixture.operator.mergeRC[approvalID] = approvalRCCommit

			cancel, done := fixture.run(t)
			select {
			case <-fixture.operator.pushStart:
			case <-time.After(3 * time.Second):
				cancel()
				waitRun(t, done)
				t.Fatal("task push did not start")
			}
			operationStart := fixture.operator.mergeStart
			if kind == "deploy" {
				operationStart = fixture.operator.deployStart
			}
			select {
			case taskID := <-operationStart:
				t.Fatalf("%s started during same-project push: %s", kind, taskID)
			case <-time.After(100 * time.Millisecond):
			}
			close(pushRelease)
			select {
			case <-operationStart:
			case <-time.After(3 * time.Second):
				cancel()
				waitRun(t, done)
				t.Fatalf("%s did not start after push", kind)
			}
			want := model.StatusAwaitingDeployApproval
			if kind == "deploy" {
				want = model.StatusDeployed
			}
			waitTaskStatus(t, fixture.db, approvalID, want)
			cancel()
			waitRun(t, done)
		})
	}
}

func TestMergeApprovalsForDifferentProjectsRunInParallel(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	taskIDs := []string{"T-000000001150", "T-000000001151"}
	projects := []string{"p1", "p2"}
	releases := make([]chan struct{}, len(taskIDs))
	for i, taskID := range taskIDs {
		fixture.createApprovedProjectTask(t, taskID, projects[i], model.StatusMerging, "merge", schedulerTaskCommit)
		fixture.operator.mergeRC[taskID] = approvalRCCommit
		releases[i] = make(chan struct{})
		fixture.operator.mergeBlock[taskID] = releases[i]
	}
	cancel, done := fixture.run(t)
	started := map[string]bool{}
	for len(started) < len(taskIDs) {
		select {
		case taskID := <-fixture.operator.mergeStart:
			started[taskID] = true
		case <-time.After(3 * time.Second):
			cancel()
			for _, release := range releases {
				close(release)
			}
			waitRun(t, done)
			t.Fatalf("parallel merges started = %v", started)
		}
	}
	for _, release := range releases {
		close(release)
	}
	for _, taskID := range taskIDs {
		waitTaskStatus(t, fixture.db, taskID, model.StatusAwaitingDeployApproval)
	}
	cancel()
	waitRun(t, done)
}

func newSchedulerFixture(t *testing.T, p1Concurrency int) *schedulerFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "tasks.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Config{
		MessageWorkers: 1,
		OneBot:         config.OneBotConfig{URL: "ws://127.0.0.1", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath:   dbPath,
		LogDir:         filepath.Join(root, "logs"),
		WorktreeRoot:   filepath.Join(root, "worktrees"),
		Consultation:   config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90},
		Codex:          config.CodexConfig{Binary: "/bin/true"},
		OpsCommand:     []string{"/bin/true"},
		Projects: []config.Project{
			{ID: "p1", Aliases: []string{"one"}, RepoPath: filepath.Join(root, "p1"), BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}}, DeployAction: "p1-rc", MaxConcurrent: p1Concurrency, CodexTimeoutSeconds: 30, LogRetentionDays: 1},
			{ID: "p2", Aliases: []string{"two"}, RepoPath: filepath.Join(root, "p2"), BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}}, DeployAction: "p2-rc", MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 1},
		},
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	events := &schedulerEvents{}
	meta := &metadataProbe{pending: make(map[string]bool), active: make(map[string]int), maxActive: make(map[string]int)}
	fixture := &schedulerFixture{
		registry:  registry,
		db:        db,
		dbPath:    dbPath,
		meta:      meta,
		eventsLog: events,
		runner: &schedulerRunner{
			projects: make(map[string]string), releases: make(map[string]<-chan struct{}), started: make(chan string, 32), finished: make(chan string, 32),
			resultByTask: make(map[string]codex.Result), errByTask: make(map[string]error), running: make(map[string]int), maxRunning: make(map[string]int), events: events,
		},
		worktrees: &schedulerWorktrees{
			meta: meta, events: events, checkStart: make(chan string, 32), checkBlock: make(map[string]<-chan struct{}), prepareErr: make(map[string]error),
			checkErr: make(map[string]error), validateErr: make(map[string]error), prepared: make(map[string]gitwork.Prepared), prepareCall: make(map[string]int),
		},
		operator: &schedulerOperator{
			meta: meta, events: events, syncErr: make(map[string]error), pushErr: make(map[string]error), pushCalls: make(map[string]int),
			pushBlock: make(map[string]<-chan struct{}), pushStart: make(chan string, 32), mergeErr: make(map[string]error),
			mergeRC: make(map[string]string), mergeCall: make(map[string]int), mergeBlock: make(map[string]<-chan struct{}), mergeStart: make(chan string, 32),
			deployErr: make(map[string]error), deployCall: make(map[string]int), deployBlock: make(map[string]<-chan struct{}), deployStart: make(chan string, 32),
		},
		notifier: &schedulerNotifier{events: events},
		logs:     &schedulerLogs{secret: "company-secret-value", streams: make(map[string]string)},
	}
	return fixture
}

func (f *schedulerFixture) createTask(t *testing.T, taskID, projectID string) {
	t.Helper()
	now := time.Now().UTC()
	task := &model.Task{
		ID: taskID, ProjectID: projectID, GroupID: "group", CreatorID: "creator",
		Requirement: "implement " + taskID, Plan: `{"summary":"plan"}`, Status: model.StatusQueued,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.db.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	f.runner.projects[taskID] = projectID
}

func (f *schedulerFixture) createApprovedTask(t *testing.T, taskID string, status model.Status, kind, commit string) {
	t.Helper()
	f.createApprovedProjectTask(t, taskID, "p1", status, kind, commit)
}

func (f *schedulerFixture) createApprovedProjectTask(t *testing.T, taskID, projectID string, status model.Status, kind, commit string) {
	t.Helper()
	f.createTask(t, taskID, projectID)
	task := mustTask(t, f.db, taskID)
	task.Status = status
	task.TaskCommit = schedulerTaskCommit
	if kind == "deploy" {
		task.RCCommit = commit
		task.DeployKey = ops.DeployKey(task.ProjectID, task.ID, commit)
	}
	if err := f.db.SaveTask(context.Background(), task, task.Version); err != nil {
		t.Fatal(err)
	}
	approval := model.Approval{
		TaskID: taskID, Kind: kind, UserID: "admin", GroupID: "group", MessageID: kind + "-" + taskID,
		BoundCommit: commit, Result: "approved", CreatedAt: time.Now().UTC(),
	}
	if err := f.db.AddApproval(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
}

func (f *schedulerFixture) scheduler(t *testing.T) *Scheduler {
	t.Helper()
	return f.schedulerWithLogs(f.logs)
}

func (f *schedulerFixture) schedulerWithLogs(logs TaskLogs) *Scheduler {
	return NewScheduler(f.registry, f.db, f.runner, f.worktrees, f.operator, f.notifier, logs, slog.Default(), 5*time.Millisecond)
}

func (f *schedulerFixture) run(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return cancel, runScheduler(f.scheduler(t), ctx)
}

func (f *schedulerFixture) events() []string {
	return f.eventsLog.all()
}

func runScheduler(scheduler *Scheduler, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	return done
}

func TestSchedulerReturnsFatalStoreWhenScanFails(t *testing.T) {
	fixture := newSchedulerFixture(t, 1)
	scheduler := fixture.scheduler(t)
	if err := fixture.db.Close(); err != nil {
		t.Fatal(err)
	}
	done := runScheduler(scheduler, context.Background())
	select {
	case err := <-done:
		if !errors.Is(err, ErrFatalStore) {
			t.Fatalf("Scheduler.Run error = %v, want ErrFatalStore", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not exit after store failure")
	}
}

func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Scheduler.Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Scheduler.Run did not stop")
	}
}

func waitTaskStatus(t *testing.T, db *store.Store, taskID string, want model.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := db.GetTask(context.Background(), taskID)
		if err == nil && task.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, err := db.GetTask(context.Background(), taskID)
	t.Fatalf("task status = %v, error = %v, want %s", task, err, want)
}

func mustTask(t *testing.T, db *store.Store, taskID string) *model.Task {
	t.Helper()
	task, err := db.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func schedulerTransitions(t *testing.T, dbPath, taskID string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close audit database: %v", err)
		}
	})
	rows, err := db.Query(`SELECT detail FROM audit_events WHERE task_id = ? AND kind = 'scheduler_transition' ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var transitions []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return transitions
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func stageChannel(f *schedulerFixture, stage string) <-chan string {
	if stage == "checks" {
		return f.worktrees.checkStart
	}
	return f.runner.started
}
