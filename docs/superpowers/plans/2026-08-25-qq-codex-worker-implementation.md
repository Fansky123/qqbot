# QQ Codex Employee Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a local Go service that accepts authorized QQ group tasks through NapCat/OneBot 11, runs Codex in isolated Git worktrees, pushes successful task branches, and gates RC merge and deployment behind administrator approvals.

**Architecture:** A single `qqcodex` control-plane process owns OneBot messaging, task state, authorization, scheduling, and Codex child processes. Credentialed Git and RC actions go through a separate `qqcodex-ops` command with a separate configuration boundary; both commands share small internal packages and persist workflow state in SQLite.

**Tech Stack:** Go 1.26, `github.com/coder/websocket` v1.8.15, `modernc.org/sqlite` v1.57.0, SQLite WAL, Codex CLI non-interactive JSONL, Git worktrees, Go standard `testing` package.

---

## Milestones

1. **Control plane foundation:** domain model, configuration, SQLite, commands, and authorization.
2. **Execution plane:** Codex runner, worktrees, credentialed ops helper, task controller, scheduler, merge, and deployment approvals.
3. **QQ integration:** OneBot codec/client, process wiring, recovery, end-to-end tests, and operator documentation.

Each task below must leave `go test ./...` passing and end with the listed commit.

## File Map

| Path | Responsibility |
| --- | --- |
| `go.mod`, `go.sum` | Go module and the two runtime dependencies. |
| `cmd/qqcodex/main.go` | Main service flags, dependency wiring, lifecycle, and signal handling. |
| `cmd/qqcodex-ops/main.go` | Restricted Git/RC operations CLI. |
| `internal/model/task.go` | Task, input, approval, statuses, and transition rules. |
| `internal/config/config.go` | JSON config loading, validation, and project alias registry. |
| `internal/store/schema.sql` | SQLite schema. |
| `internal/store/sqlite.go` | Optimistic task persistence, inputs, approvals, deduplication, and recovery. |
| `internal/command/parser.go` | Strict group command parser. |
| `internal/auth/auth.go` | Employee/admin authorization rules. |
| `internal/codex/runner.go` | Safe Codex subprocess construction, JSONL parsing, planning, execution, and resume. |
| `internal/codex/schema.json` | Structured response schema for pre-execution task plans. |
| `internal/gitwork/manager.go` | Task branch/worktree creation, checks, and commit validation. |
| `internal/ops/config.go` | Separate credentialed operations config. |
| `internal/ops/operator.go` | Git sync/push/merge and fixed RC deployment actions. |
| `internal/ops/client.go` | Subprocess client used by the main service to call `qqcodex-ops`. |
| `internal/tasklog/log.go` | Per-task append-only logs, redaction, bounded summaries, and retention cleanup. |
| `internal/tasksvc/ports.go` | Narrow interfaces used by the task workflow. |
| `internal/tasksvc/service.go` | Create/confirm/supplement/cancel/status command handling. |
| `internal/tasksvc/scheduler.go` | Persistent polling, per-project concurrency, Codex execution, checks, and push. |
| `internal/tasksvc/approval.go` | Immutable merge/deploy approval handling. |
| `internal/onebot/protocol.go` | OneBot 11 event/action models, ID normalization, CQ escaping, and message splitting. |
| `internal/onebot/client.go` | Authenticated WebSocket connection, pending action responses, and reconnect loop. |
| `internal/app/app.go` | Converts OneBot messages to task service messages and coordinates startup/recovery. |
| `configs/qqcodex.example.json` | Secret-free main-service example config. |
| `configs/ops.example.json` | Secret-free restricted-ops example config. |
| `README.md` | Local setup, NapCat settings, commands, safety boundaries, and RC enablement. |

Test files live beside their packages as `*_test.go`. Cross-component tests live in `internal/app/app_integration_test.go`.

## Milestone 1: Control Plane Foundation

### Task 1: Bootstrap the module and task state machine

**Files:**
- Create: `go.mod`
- Create: `internal/model/task.go`
- Test: `internal/model/task_test.go`

- [ ] **Step 1: Initialize the module and pin runtime dependencies**

Run:

```bash
go mod init qqcodex
go get github.com/coder/websocket@v1.8.15
go get modernc.org/sqlite@v1.57.0
```

Expected: `go.mod` declares module `qqcodex`, Go `1.26.0`, and both dependencies; `go.sum` is created.

- [ ] **Step 2: Write failing transition tests**

Create `internal/model/task_test.go`:

```go
package model

import "testing"

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to Status
		want     bool
	}{
		{StatusDraft, StatusAwaitingConfirmation, true},
		{StatusAwaitingConfirmation, StatusQueued, true},
		{StatusQueued, StatusRunning, true},
		{StatusRunning, StatusChecking, true},
		{StatusChecking, StatusPushed, true},
		{StatusPushed, StatusAwaitingMergeApproval, true},
		{StatusAwaitingMergeApproval, StatusQueued, true},
		{StatusAwaitingMergeApproval, StatusMerging, true},
		{StatusMerged, StatusAwaitingDeployApproval, true},
		{StatusAwaitingDeployApproval, StatusDeploying, true},
		{StatusDeploying, StatusDeployed, true},
		{StatusDeployed, StatusRunning, false},
		{StatusCancelled, StatusQueued, false},
	}
	for _, tt := range tests {
		if got := CanTransition(tt.from, tt.to); got != tt.want {
			t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestTerminalStatus(t *testing.T) {
	for _, status := range []Status{StatusCancelled, StatusFailed, StatusMergeConflict, StatusDeployed} {
		if !status.Terminal() {
			t.Fatalf("%q must be terminal", status)
		}
	}
}
```

- [ ] **Step 3: Run the test and verify it fails**

Run: `go test ./internal/model -run 'TestCanTransition|TestTerminalStatus' -v`

Expected: FAIL with undefined `Status` or `CanTransition`.

- [ ] **Step 4: Implement the domain model and explicit transition table**

Create `internal/model/task.go` with these public types and constants:

```go
package model

import "time"

type Status string

const (
	StatusDraft                  Status = "draft"
	StatusAwaitingConfirmation   Status = "awaiting_confirmation"
	StatusQueued                 Status = "queued"
	StatusRunning                Status = "running"
	StatusBlocked                Status = "blocked"
	StatusChecking               Status = "checking"
	StatusPushed                 Status = "pushed"
	StatusAwaitingMergeApproval  Status = "awaiting_merge_approval"
	StatusMerging                Status = "merging"
	StatusMergeConflict          Status = "merge_conflict"
	StatusMerged                 Status = "merged"
	StatusAwaitingDeployApproval Status = "awaiting_deploy_approval"
	StatusDeploying              Status = "deploying"
	StatusDeployFailed           Status = "deploy_failed"
	StatusDeployed               Status = "deployed"
	StatusFailed                 Status = "failed"
	StatusCancelled              Status = "cancelled"
)

type Task struct {
	ID           string
	ProjectID    string
	GroupID      string
	CreatorID    string
	Requirement  string
	Plan         string
	Status       Status
	Branch       string
	Worktree     string
	BaseCommit   string
	TaskCommit   string
	RCCommit     string
	SessionID    string
	Summary      string
	Failure      string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Input struct {
	TaskID, Kind, UserID, GroupID, MessageID, Body string
	CreatedAt                                      time.Time
}

type Approval struct {
	TaskID, Kind, UserID, GroupID, MessageID, BoundCommit, Result string
	CreatedAt                                                     time.Time
}

var transitions = map[Status]map[Status]struct{}{
	StatusDraft:                  {StatusAwaitingConfirmation: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusAwaitingConfirmation:   {StatusQueued: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusQueued:                 {StatusRunning: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusRunning:                {StatusBlocked: {}, StatusChecking: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusBlocked:                {StatusQueued: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusChecking:               {StatusPushed: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusPushed:                 {StatusAwaitingMergeApproval: {}, StatusFailed: {}, StatusCancelled: {}},
	StatusAwaitingMergeApproval:  {StatusQueued: {}, StatusMerging: {}, StatusCancelled: {}},
	StatusMerging:                {StatusMerged: {}, StatusMergeConflict: {}, StatusFailed: {}},
	StatusMerged:                 {StatusAwaitingDeployApproval: {}},
	StatusAwaitingDeployApproval: {StatusDeploying: {}},
	StatusDeploying:              {StatusDeployed: {}, StatusDeployFailed: {}},
	StatusDeployFailed:           {StatusDeploying: {}},
}

func CanTransition(from, to Status) bool {
	_, ok := transitions[from][to]
	return ok
}

func (s Status) Terminal() bool {
	return s == StatusCancelled || s == StatusFailed || s == StatusMergeConflict || s == StatusDeployed
}
```

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/model -v && go test ./...`

Expected: PASS.

```bash
git add go.mod go.sum internal/model
git commit -m "feat: add task domain model"
```

### Task 2: Load and validate the project registry

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Create: `configs/qqcodex.example.json`

- [ ] **Step 1: Write failing config validation tests**

Create `internal/config/config_test.go` with table cases for duplicate aliases, relative repository paths, empty checks, admin users absent from employees, non-positive concurrency, and a valid config. The valid assertion must resolve both aliases to the same project:

```go
func TestRegistryResolvesAliases(t *testing.T) {
	cfg := validConfig(t)
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := registry.Project("orders")
	if !ok || a.ID != "order-api" {
		t.Fatalf("unexpected project: %#v, %v", a, ok)
	}
	b, ok := registry.Project("订单")
	if !ok || b.ID != a.ID {
		t.Fatalf("alias did not resolve: %#v, %v", b, ok)
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		OneBot:          OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath:    filepath.Join(root, "tasks.db"),
		LogDir:          filepath.Join(root, "logs"),
		WorktreeRoot:    filepath.Join(root, "worktrees"),
		MessageWorkers:  4,
		AllowedGroupIDs: []string{"20000"},
		EmployeeIDs:     []string{"30000", "30001"},
		AdminIDs:        []string{"30001"},
		Codex:           CodexConfig{Binary: "codex"},
		OpsCommand:      []string{"qqcodex-ops", "-config", filepath.Join(root, "ops.json")},
		Projects: []Project{{
			ID: "order-api", Aliases: []string{"orders", "订单"}, RepoPath: filepath.Join(root, "order-api"),
			BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}},
			DeployAction: "order-rc", MaxConcurrent: 2, CodexTimeoutSeconds: 1800, LogRetentionDays: 7,
		}},
	}
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/config -v`

Expected: FAIL because `NewRegistry` and config types do not exist.

- [ ] **Step 3: Implement exact configuration types**

Create `internal/config/config.go` with:

```go
type Config struct {
	OneBot          OneBotConfig `json:"onebot"`
	DatabasePath    string       `json:"database_path"`
	LogDir          string       `json:"log_dir"`
	WorktreeRoot    string       `json:"worktree_root"`
	MessageWorkers  int          `json:"message_workers"`
	AllowedGroupIDs []string     `json:"allowed_group_ids"`
	EmployeeIDs     []string     `json:"employee_ids"`
	AdminIDs        []string     `json:"admin_ids"`
	Codex           CodexConfig  `json:"codex"`
	OpsCommand      []string     `json:"ops_command"`
	Projects        []Project    `json:"projects"`
}

type OneBotConfig struct {
	URL            string `json:"url"`
	AccessTokenEnv string `json:"access_token_env"`
	SelfID         string `json:"self_id"`
	MessageRunes   int    `json:"message_runes"`
}

type CodexConfig struct {
	Binary          string   `json:"binary"`
	EnvironmentKeep []string `json:"environment_keep"`
}

type Project struct {
	ID                  string     `json:"id"`
	Aliases             []string   `json:"aliases"`
	RepoPath            string     `json:"repo_path"`
	BaseBranch          string     `json:"base_branch"`
	RCBranch            string     `json:"rc_branch"`
	Remote              string     `json:"remote"`
	Checks              [][]string `json:"checks"`
	DeployAction        string     `json:"deploy_action"`
	MaxConcurrent       int        `json:"max_concurrent"`
	CodexTimeoutSeconds int        `json:"codex_timeout_seconds"`
	LogRetentionDays    int        `json:"log_retention_days"`
}
```

Implement `Load(path string) (Config, error)`, `Validate(Config) error`, and immutable `Registry.Project(alias string) (Project, bool)`. Validation must use `filepath.IsAbs`, reject aliases with surrounding whitespace, and accept only Unicode letters, Unicode digits, `_`, and `-` by checking each rune with `unicode.IsLetter`/`unicode.IsDigit`. Require at least one non-empty check argv, require every admin to also be an employee, and default `MessageRunes` to 1200 and `MessageWorkers` to 4 only during `Load`.

Canonicalize the database identity before deriving its fixed sibling runtime-lock file: resolve parent and final symlinks for an existing database; for a new database resolve the existing parent before creation, then resolve and revalidate the final identity after initialization. Pin the canonical parent directory and database inode while opening, require the database to remain a single-link regular file, and use that parent handle for the runtime lock so aliases and parent-path replacement cannot create a second lock. Reject hardlinks and non-regular database files with path-free errors. Task 14 must acquire the non-blocking OS advisory lock before recovery or any scheduler/message loop, keep the lock handle open for the entire application lifetime, and fail startup if another process already owns it. Do not substitute a TTL database lease: a paused first process must remain the sole owner because deployment cannot be replayed safely.

- [ ] **Step 4: Add a secret-free example config**

Create `configs/qqcodex.example.json` using `/srv/repos/order-api`, `/srv/qqcodex/worktrees`, environment variable name `NAPCAT_ACCESS_TOKEN`, example numeric IDs `10001` and `10002`, `go test ./...` as the check argv, and `./qqcodex-ops -config ./configs/ops.json` as `ops_command`. Do not include an actual token, Codex key, or Git credential.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/config -v && go test ./...`

Expected: PASS.

```bash
git add internal/config configs/qqcodex.example.json
git commit -m "feat: add validated project configuration"
```

### Task 3: Persist tasks and workflow records in SQLite

**Files:**
- Create: `internal/store/schema.sql`
- Create: `internal/store/sqlite.go`
- Test: `internal/store/sqlite_test.go`

- [ ] **Step 1: Write failing persistence tests**

Create tests that open `filepath.Join(t.TempDir(), "tasks.db")`, create a task, reload it, save it with the expected version, reject a stale save with `ErrConflict`, append an input and approval, and deduplicate a OneBot message ID. Include this assertion:

```go
saved, err := db.GetTask(ctx, task.ID)
if err != nil {
	t.Fatal(err)
}
saved.Status = model.StatusAwaitingConfirmation
if err := db.SaveTask(ctx, saved, saved.Version); err != nil {
	t.Fatal(err)
}
if err := db.SaveTask(ctx, saved, saved.Version); !errors.Is(err, ErrConflict) {
	t.Fatalf("stale save error = %v, want ErrConflict", err)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/store -v`

Expected: FAIL because `Open`, `GetTask`, and `SaveTask` do not exist.

- [ ] **Step 3: Add the complete SQLite schema**

Create `internal/store/schema.sql` with `tasks`, `task_inputs`, `approvals`, `processed_messages`, and `audit_events`. Use `TEXT PRIMARY KEY` for task IDs and canonical processed-message keys, `INTEGER` Unix milliseconds for timestamps, `INTEGER NOT NULL DEFAULT 1` for task version, and a unique constraint on approval `(group_id, message_id)`. Every task field in `model.Task` must have a column.

The schema must begin with:

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
```

- [ ] **Step 4: Implement the SQLite store**

Embed `schema.sql` and implement:

```go
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("optimistic update conflict")

func Open(path string) (*Store, error)
func (s *Store) Close() error
func (s *Store) CreateTask(context.Context, *model.Task) error
func (s *Store) GetTask(context.Context, string) (*model.Task, error)
func (s *Store) SaveTask(context.Context, *model.Task, int64) error
func (s *Store) ListByStatus(context.Context, model.Status, int) ([]*model.Task, error)
func (s *Store) ListCleanupCandidates(context.Context, time.Time) ([]*model.Task, error)
func (s *Store) AppendInput(context.Context, model.Input) error
func (s *Store) AddApproval(context.Context, model.Approval) error
func (s *Store) InvalidateApprovals(context.Context, string, string, string) error
func (s *Store) MessageProcessed(context.Context, string) (bool, error)
func (s *Store) MarkMessageProcessed(context.Context, string, time.Time) error
func (s *Store) AppendAudit(context.Context, string, string, string, time.Time) error
```

`SaveTask` must execute the following parameterized shape, return `ErrConflict` when `RowsAffected` is zero, update the caller's `Version`, and never concatenate SQL strings from task data:

```sql
UPDATE tasks
SET project_id = ?, group_id = ?, creator_id = ?, requirement = ?, plan = ?,
    status = ?, branch = ?, worktree = ?, base_commit = ?, task_commit = ?,
    rc_commit = ?, session_id = ?, summary = ?, failure = ?, updated_at = ?,
    version = version + 1
WHERE id = ? AND version = ?;
```

`InvalidateApprovals` updates all still-valid approvals of the requested kind to the supplied result such as `superseded_by_new_commit`.

`ListCleanupCandidates` returns only `failed` and `cancelled` tasks older than the supplied cutoff. It never returns queued, running, merge-conflict, merged, deploy-failed, or deployed tasks.

- [ ] **Step 5: Add interrupted-state recovery**

Add `RecoverInterrupted(ctx)` that runs in one transaction:

- `running` and `checking` become `failed` with failure `service restarted during execution`.
- `pushed` becomes `awaiting_merge_approval` because the remote task push already completed before that state was recorded.
- `merging` becomes `failed` with failure `service restarted during merge; inspect remote RC before retrying`.
- `merged` becomes `awaiting_deploy_approval` because the RC commit was recorded before that state was recorded.
- `deploying` becomes `deploy_failed` with failure `service restarted during deploy; inspect RC before retrying`.
- `draft`, `awaiting_confirmation`, and `queued` remain unchanged.

Add a test that inserts one task in each listed state and verifies exact recovery results.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/store -v && go test ./...`

Expected: PASS and no SQLite database files appear in the repository.

```bash
git add internal/store
git commit -m "feat: persist task workflow in sqlite"
```

### Task 4: Parse commands and enforce roles

**Files:**
- Create: `internal/command/parser.go`
- Test: `internal/command/parser_test.go`
- Create: `internal/auth/auth.go`
- Test: `internal/auth/auth_test.go`

- [ ] **Step 1: Write strict parser tests**

Cover create, confirm, supplement, cancel, status, log, merge approval, and deploy approval. Reject missing mentions for create, unknown commands, task IDs outside `^T-[A-F0-9]{12}$`, empty supplements, and project aliases containing spaces.

Use this table shape:

```go
tests := []struct {
	name      string
	text      string
	mentioned bool
	wantKind  Kind
	wantErr   bool
}{
	{"create", "[orders] fix duplicate submit", true, KindCreate, false},
	{"create without mention", "[orders] fix duplicate submit", false, "", true},
	{"confirm", "确认 #T-012345ABCDEF", false, KindConfirm, false},
	{"bad id", "状态 #../../etc", false, "", true},
}
```

- [ ] **Step 2: Write authorization tests**

Verify an allowed employee in an allowed group can create and operate their own task, a message from another group is rejected, an employee cannot operate another employee's task, admin can operate any task, and only admin can approve merge/deploy.

- [ ] **Step 3: Run tests and verify they fail**

Run: `go test ./internal/command ./internal/auth -v`

Expected: FAIL because packages are not implemented.

- [ ] **Step 4: Implement parser and authorizer**

Define:

```go
type Kind string

const (
	KindCreate        Kind = "create"
	KindConfirm       Kind = "confirm"
	KindSupplement    Kind = "supplement"
	KindCancel        Kind = "cancel"
	KindStatus        Kind = "status"
	KindLog           Kind = "log"
	KindApproveMerge  Kind = "approve_merge"
	KindApproveDeploy Kind = "approve_deploy"
)

type Command struct {
	Kind, ProjectAlias, TaskID, Body string
}

func Parse(text string, mentioned bool) (Command, error)
```

Use anchored regular expressions and `strings.TrimSpace`; never use substring matching for approval commands.

In `internal/auth/auth.go`, define `RoleNone`, `RoleEmployee`, `RoleAdmin`, `New(allowedGroupIDs, employeeIDs, adminIDs []string) Authorizer`, `AllowedGroup(groupID string) bool`, `Role(userID string) Role`, `CanOperate(userID, creatorID string) bool`, and `CanApprove(userID string) bool` using immutable lookup maps.

- [ ] **Step 5: Run tests and commit milestone 1**

Run: `go test ./internal/command ./internal/auth -v && go test ./...`

Expected: PASS.

```bash
git add internal/command internal/auth
git commit -m "feat: parse commands and enforce roles"
```

## Milestone 2: Codex and Git Execution Plane

### Task 5: Run Codex safely in planning, execution, and resume modes

**Files:**
- Create: `internal/codex/schema.json`
- Create: `internal/codex/runner.go`
- Test: `internal/codex/runner_test.go`

- [ ] **Step 1: Write a fake Codex helper and failing tests**

Use the Go helper-process test pattern: when `GO_WANT_CODEX_HELPER=1`, the test process records argv/environment, writes a `thread.started` JSONL event and final agent event, writes the path passed to `--output-last-message`, and exits with a test-selected status.

Tests must verify:

- Plan uses `exec`, `--sandbox read-only`, `--ephemeral`, `--output-schema`, and repository `-C`.
- Execute uses `--sandbox workspace-write` and exactly one `--add-dir` equal to Git common dir.
- Resume uses `exec resume <session-id>` and does not use `--last`.
- Child environment includes only configured names plus `CODEX_API_KEY`, `PATH`, `HOME`, `LANG`, and temporary-directory variables.
- Timeout kills the process and returns `context.DeadlineExceeded` or a wrapped equivalent.
- A non-zero exit preserves captured JSONL and final text in the returned result/error.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/codex -v`

Expected: FAIL because `Runner` does not exist.

- [ ] **Step 3: Add the planning response schema**

Create `internal/codex/schema.json` as a strict JSON object with required string `summary`, required arrays of strings `scope`, `checks`, and `risks`, and `additionalProperties: false`. Embed it in the runner and write it to a mode-`0600` temporary file for each planning invocation; remove that file after the Codex process exits.

- [ ] **Step 4: Implement runner requests and result parsing**

Define:

```go
type Request struct {
	TaskID       string
	WorkingDir   string
	GitCommonDir string
	Prompt      string
	SessionID   string
	Timeout     time.Duration
}

type Result struct {
	SessionID   string
	Final       string
	EventsJSONL []byte
}

type Plan struct {
	Summary string   `json:"summary"`
	Scope   []string `json:"scope"`
	Checks  []string `json:"checks"`
	Risks   []string `json:"risks"`
}

type Runner struct {
	Binary string
	KeepEnv []string
	LogDir string
}

func (r Runner) Plan(context.Context, Request) (Result, error)
func (r Runner) Execute(context.Context, Request) (Result, error)
func (r Runner) Resume(context.Context, Request) (Result, error)
func ParsePlan(final string) (Plan, error)
```

Construct argv exactly as documented by the installed CLI:

```text
plan:    exec -C <repo> --sandbox read-only --ephemeral --output-schema <schema> --json -o <last> <prompt>
execute: exec -C <worktree> --sandbox workspace-write --add-dir <git-common> --json -o <last> <prompt>
resume:  exec resume --json -o <last> <session-id> <prompt>
```

Use `exec.CommandContext`, set `cmd.Dir`, set a sanitized `cmd.Env`, put the command in its own Unix process group, scan stdout line-by-line with a raised scanner buffer, and extract `thread_id` only from `type == "thread.started"`. Keep stderr in the task log but never return it directly to QQ.

- [ ] **Step 5: Add prompt builders**

Add pure functions `PlanningPrompt(projectID, requirement string, checks [][]string) string` and `ExecutionPrompt(task model.Task, checks [][]string) string`. Both must state the exact repository boundary, forbid deployment and remote push, and instruct Codex not to modify project configuration outside the task. The execution prompt must require configured checks and a Git commit, while explaining that the controller performs the remote push.

Test that a requirement containing backticks, `$()`, and newlines appears as data inside the prompt and never changes subprocess argv count.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/codex -v && go test ./...`

Expected: PASS.

```bash
git add internal/codex
git commit -m "feat: add sandboxed codex runner"
```

### Task 6: Manage isolated Git worktrees and checks

**Files:**
- Create: `internal/gitwork/manager.go`
- Test: `internal/gitwork/manager_test.go`

- [ ] **Step 1: Write integration tests using temporary repositories**

Create a bare remote, a seed repository with `main` and `rc`, and two tasks. Verify:

- `Prepare` creates `codex/T-...` from `origin/main` under the configured root.
- Two task worktrees can coexist.
- `GitCommonDir` returns the shared Git directory as an absolute path.
- `ValidateCommit` rejects a dirty tree, no new commits, the wrong branch, and a commit not descended from the recorded base.
- `RunChecks` executes argv without a shell by using an argument containing `$()` and verifying it remains literal.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/gitwork -v`

Expected: FAIL because `Manager` does not exist.

- [ ] **Step 3: Implement the worktree manager**

Define:

```go
type Prepared struct {
	Path, Branch, BaseCommit, GitCommonDir string
}

type Manager struct {
	Root string
}

func (m Manager) Prepare(ctx context.Context, project config.Project, taskID string) (Prepared, error)
func (m Manager) RunChecks(ctx context.Context, project config.Project, worktree string) error
func (m Manager) ValidateCommit(ctx context.Context, prepared Prepared) (string, error)
func (m Manager) Remove(ctx context.Context, repoPath, worktree string) error
```

Validate task IDs before constructing branch/path. Use `git -C <repo> rev-parse origin/<base>`, `git worktree add -b <branch> <path> <base-commit>`, and `git rev-parse --git-common-dir`. Resolve all paths with `filepath.Abs`/`EvalSymlinks` and verify the final worktree path remains below `Manager.Root` with `filepath.Rel`.

`RunChecks` must use `exec.CommandContext(check[0], check[1:]...)` with `cmd.Dir = worktree`; reject empty argv during config validation rather than invoking a shell.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/gitwork -v && go test ./...`

Expected: PASS.

```bash
git add internal/gitwork
git commit -m "feat: manage isolated git worktrees"
```

### Task 7: Implement the restricted Git and RC operations helper

**Files:**
- Create: `internal/ops/config.go`
- Create: `internal/ops/operator.go`
- Create: `internal/ops/client.go`
- Create: `cmd/qqcodex-ops/main.go`
- Test: `internal/ops/operator_test.go`
- Test: `internal/ops/client_test.go`
- Create: `configs/ops.example.json`

- [ ] **Step 1: Write failing operator tests**

Using two physically separate worker/ops clones and local bare remotes, verify `Sync`, `PushTaskBundle`, and `MergeRC`. `Client.PushTask` must resolve the exact source ref, create one bounded bundle, and send it only through helper stdin; no source path or `.git` data may cross the boundary. `PushTaskBundle` must reject a branch outside `codex/<valid-task-id>`, a commit mismatch, extra/wrong refs, appended junk, empty/oversize/cancelled input, missing trusted base ancestry, and every non-fast-forward update. It accepts a missing remote task branch on first push or a remote task commit that is an ancestor of the new commit on supplement. The helper must spool into a 0700/0600 private path, verify/list-heads the single full ref, fsck-import to a random temporary ref, remove bundle and refs before any external push, and re-guard the ops clone. `MergeRC` must verify the remote task ref equals the approved task commit, return the new RC commit, run checks, stop on conflict, and never force push. A deploy test must use a fixed helper argv that records environment values and prove a task requirement cannot alter the command.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/ops -v`

Expected: FAIL because the ops package is missing.

- [ ] **Step 3: Implement separate ops configuration**

Define a config keyed by project ID:

```go
type Project struct {
	RepoPath     string     `json:"repo_path"`
	Remote       string     `json:"remote"`
	BaseBranch   string     `json:"base_branch"`
	RCBranch     string     `json:"rc_branch"`
	Checks       [][]string `json:"checks"`
	DeployAction []string   `json:"deploy_action"`
}
```

Implement `LoadConfig(path)` with absolute repo validation, fixed remote/ref validation through `git check-ref-format --branch`, non-empty checks/deploy argv, and no secret fields. Secrets are supplied only to the ops process environment or its OS account.

- [ ] **Step 4: Implement operator actions**

Define:

```go
func (o *Operator) Sync(ctx context.Context, projectID string) error
func (o *Operator) PushTaskBundle(ctx context.Context, projectID, taskID, branch, commit string, bundle io.Reader) error
func (o *Operator) MergeRC(ctx context.Context, projectID, taskID, taskCommit string) (string, error)
func (o *Operator) DeployRC(ctx context.Context, projectID, taskID, rcCommit string) error
```

Required commands:

```text
sync:   git -C <repo> fetch --prune <remote> <base> <rc>
push:   git -C <ops-repo> fetch <bundle> <full-task-ref>:<temporary-ref>; verify/import/cleanup; push <remote-url> <commit>:refs/heads/<task-branch>
merge:  temporary worktree at exact remote RC commit; git merge --no-ff --no-edit <task-commit>; checks; normal push HEAD:refs/heads/<rc>
deploy: fixed deploy argv with QQCODEX_PROJECT_ID, QQCODEX_TASK_ID, QQCODEX_RC_COMMIT environment variables
```

Before push, verify local branch and remote task ref expectations. Before deploy, fetch and verify the configured remote RC ref equals the approved `rcCommit`. Do not use `--force`, `--force-with-lease`, `sh -c`, or a user-provided path/ref.

- [ ] **Step 5: Add the JSON CLI and subprocess client**

`qqcodex-ops` accepts only:

```text
qqcodex-ops -config <path> sync --project <id>
qqcodex-ops -config <path> push --project <id> --task <id> --branch <branch> --commit <sha>
qqcodex-ops -config <path> merge --project <id> --task <id> --commit <sha>
qqcodex-ops -config <path> deploy --project <id> --task <id> --rc-commit <sha>
```

It writes one JSON object to stdout, for example `{"ok":true,"rc_commit":"0123456789abcdef0123456789abcdef01234567"}` or `{"ok":false,"error":"remote RC changed"}`, and uses non-zero exit status for failure. `internal/ops/client.go` builds these argv from typed methods, sends task bundles through stdin for `push`, and parses only that JSON object. The public client constructor accepts exactly `[root-owned absolute helper, "-config", root-owned absolute config]`, requires a root-owned source Git executable, and validates non-replaceable files and parent directories; same-package tests use an unexported current-UID constructor with arbitrary test-helper arguments.

- [ ] **Step 6: Add the example config and run tests**

Create `configs/ops.example.json` with the same example project as the main config and deploy argv `['/usr/local/libexec/deploy-order-rc']`. Do not include credentials.

Run: `go test ./internal/ops -v && go test ./...`

Expected: PASS.

```bash
git add internal/ops cmd/qqcodex-ops configs/ops.example.json
git commit -m "feat: add restricted git and rc operator"
```

### Task 8: Store redacted per-task logs

**Files:**
- Create: `internal/tasklog/log.go`
- Test: `internal/tasklog/log_test.go`
- Modify: `internal/codex/runner.go`
- Modify: `internal/codex/runner_test.go`
- Modify: `internal/gitwork/manager.go`
- Modify: `internal/gitwork/manager_test.go`
- Modify: `internal/ops/client.go`
- Modify: `internal/ops/client_test.go`

- [ ] **Step 1: Write failing log safety tests**

Verify task IDs outside `^T-[A-F0-9]{12}$` are rejected, directories are mode `0700`, files are mode `0600`, concurrent appends do not interleave records, exact configured secret values are replaced with `[REDACTED]`, bearer headers and `CODEX_API_KEY=`/`NAPCAT_ACCESS_TOKEN=` values are redacted, and `Summary` returns at most the requested rune count.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/tasklog -v`

Expected: FAIL because `tasklog.Store` does not exist.

- [ ] **Step 3: Implement the append-only task log**

Define:

```go
type Store struct {
	Root string
	// private redaction values and per-task mutex map
}

func Open(root string, secretValues []string) (*Store, error)
func (s *Store) Append(taskID, stream string, data []byte) error
func (s *Store) Writer(taskID, stream string) io.Writer
func (s *Store) Summary(taskID string, maxRunes int) (string, error)
func (s *Store) Remove(taskID string) error
```

Each append writes one timestamped record with stream name after redaction. Resolve and validate the task path before every open/remove. `Summary` reads the tail of the file, redacts again, and truncates by runes rather than bytes.

- [ ] **Step 4: Route execution output into the task log**

Add a narrow `LogSink` interface with `Append(taskID, stream string, data []byte) error` to the Codex runner and ops client. Pass `tasklog.Store` from application wiring. Codex runner writes JSONL, stderr, and final output under streams `codex.events`, `codex.stderr`, and `codex.final`; ops client writes captured helper stderr under `ops.stderr`.

Change `gitwork.Manager.RunChecks` to:

```go
func (m Manager) RunChecks(
	ctx context.Context,
	project config.Project,
	worktree string,
	output io.Writer,
) error
```

Write each fixed argv and its combined output to `output`, with no environment dump.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/tasklog ./internal/codex ./internal/gitwork ./internal/ops -v && go test ./...`

Expected: PASS.

```bash
git add internal/tasklog internal/codex internal/gitwork internal/ops
git commit -m "feat: add redacted task logs"
```

### Task 9: Handle task creation and employee commands

**Files:**
- Create: `internal/tasksvc/ports.go`
- Create: `internal/tasksvc/service.go`
- Test: `internal/tasksvc/service_test.go`

- [ ] **Step 1: Define fakes and write failing workflow tests**

Use a real temporary SQLite store plus fake planner, notifier, scheduler wakeup, and log reader. Verify:

- Authorized create stores a deterministic task ID derived from group/message ID, calls the planner once, stores the plan, and enters `awaiting_confirmation`.
- Replaying the same OneBot message returns the existing task and does not call the planner twice.
- Confirm by creator changes status to `queued` and wakes scheduler.
- Non-owner supplement/cancel is rejected; admin succeeds.
- Supplement from `blocked` or `awaiting_merge_approval` clears task/RC commit and returns to `queued`.
- Cancel changes pre-merge tasks to `cancelled`, asks scheduler to cancel a running task, and refuses after `merged`.
- Status/log output is bounded and contains no raw environment values.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/tasksvc -run TestService -v`

Expected: FAIL because task service types are undefined.

- [ ] **Step 3: Define narrow ports**

Create `ports.go`:

```go
type Planner interface {
	Plan(context.Context, codex.Request) (codex.Result, error)
}

type SchedulerControl interface {
	Wake()
	Cancel(taskID string)
}

type Notifier interface {
	Send(context.Context, string, string) error
}

type LogReader interface {
	Summary(taskID string, maxRunes int) (string, error)
}

type Message struct {
	GroupID, UserID, MessageID, Text string
	Mentioned                         bool
}
```

- [ ] **Step 4: Implement deterministic task IDs and command handling**

Use SHA-256 over `groupID + "\x00" + messageID`, uppercase the first six bytes, and format `T-%X`, producing exactly 12 hex digits. Do not use time or random data for message-originated task IDs.

Implement `NewService(registry *config.Registry, db *store.Store, authorizer auth.Authorizer, planner Planner, scheduler SchedulerControl, notifier Notifier, logs LogReader) *Service` and `Service.Handle(ctx, Message) error`, dispatching parsed commands to private methods after checking `AllowedGroup` and user role. Every successful command must append input/audit records and mark the canonical message key `groupID + "\x00" + messageID` processed after the state change. Duplicate create relies on deterministic ID; all other state updates use optimistic versions and valid transition checks.

For create, insert the initial `draft` task before calling the planner. If the deterministic task ID already exists, return its current status without another planner call. This ordering makes concurrent delivery of the same OneBot event idempotent.

For create, build the planning request from the resolved project repo/checks and store the structured final plan. The confirmation response includes task ID, project, summary, scope, checks, risks, and exact `确认 #<id>` command.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/tasksvc -run TestService -v && go test ./...`

Expected: PASS.

```bash
git add internal/tasksvc/ports.go internal/tasksvc/service.go internal/tasksvc/service_test.go
git commit -m "feat: handle employee task commands"
```

### Task 10: Schedule concurrent Codex work and push successful branches

**Files:**
- Create: `internal/tasksvc/scheduler.go`
- Test: `internal/tasksvc/scheduler_test.go`
- Modify: `internal/tasksvc/ports.go`
- Modify: `internal/model/task.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/sqlite_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write scheduler tests with fakes**

Verify two tasks for one project run Codex concurrently up to `MaxConcurrent`, a third waits, another project runs independently, and cancellation propagates to the task context. Use a blocking fake to prove `Sync`/`Prepare` and `PushTask` calls for the same project never overlap even while the Codex calls do overlap. Verify this exact success sequence:

```text
queued -> running -> checking -> pushed -> awaiting_merge_approval
```

The fake call order must be:

```text
ops.Sync -> worktree.Prepare -> codex.Execute/Resume -> worktree.RunChecks -> worktree.ValidateCommit -> ops.PushTask -> notify completion
```

Also verify runner failure, check failure, dirty tree, missing commit, and push failure all become `failed` without merge/deploy calls.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/tasksvc -run TestScheduler -v`

Expected: FAIL because `Scheduler` is undefined.

- [ ] **Step 3: Extend ports for execution dependencies**

Add typed interfaces for `Runner`, `Worktrees`, and `Operator` matching the methods from Tasks 5-7. Add a narrow `TaskLogs` interface exposing only `Writer(taskID, stream) io.Writer` and `RedactText(string) string`; the application passes the same `tasklog.Store` used by the runner and command service. The worker/task service `Operator` interface exposes `PushTask(ctx context.Context, projectID, taskID, branch, commit string) error`, implemented by `*ops.Client`. `tasksvc` never sees an `io.Reader`, bundle, or source repository path: `ops.Client` encapsulates source-ref verification, bundle creation, and helper stdin transfer. `PushTaskBundle(..., io.Reader)` remains an internal boundary between the ops helper CLI and `ops.Operator`. Do not expose `exec.Cmd`, raw SQL, source repository paths, or unvalidated argv through task-service interfaces.

- [ ] **Step 4: Implement persistent polling and concurrency**

Implement:

```go
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
) *Scheduler
func (s *Scheduler) Run(context.Context) error
func (s *Scheduler) Wake()
func (s *Scheduler) Cancel(taskID string)
```

Poll `queued`, `merging`, `merged`, and `deploying` tasks after wakeups and every two seconds. Use a buffered channel per project sized to `MaxConcurrent`, a short-held Git metadata mutex per project, and one release-operation mutex per project so merge and deploy cannot overlap each other. Claim queued tasks through optimistic `SaveTask` to `running`, so two scheduler scans cannot start the same task. Task 11 adds a durable SQLite project-operation lock for cross-instance merge/deploy serialization.

Hold the Git metadata mutex around `Operator.Sync` plus `Worktrees.Prepare`, around `Operator.PushTask`, and around merge/deploy operator calls. Release it before Codex execution and checks so independent worktrees still run concurrently.

Persist `GitCommonDir` with the prepared worktree and validate all persisted worktree, branch, base, common-directory, session, and task-ID fields before reconstructing `gitwork.Prepared` for resume. On first execution create a worktree and call `Runner.Execute`; when `SessionID` and complete worktree metadata already exist, pass the cumulative requirement (including the latest supplement) to `Runner.Resume`. Store session ID and a redacted summary before checks, and write check output through `TaskLogs.Writer` rather than discarding it.

The Codex `Result` carries a typed, bounded `Blocked`/`BlockedReason` signal. The execution prompt requires Codex to emit one structured `task.blocked` event and stop when required information is missing. Persist the session, summary, and sanitized reason atomically with `running -> blocked`, then notify with the exact supplement command so Task9 can queue the task and resume the same session.

After checks and commit validation, acquire a durable SQLite push lease with an unpredictable scheduler owner, bounded expiry, owner-conditional heartbeat renewal, and owner-conditional release. While holding that lease, checkpoint the exact validated `TaskCommit` in `checking` before calling the idempotent exact-ref `PushTask`. A restarted scheduler may claim an expired lease for `checking` plus non-empty `TaskCommit` and retry only that exact push without rerunning Codex/checks; a `pushed` scanner also acquires the same lease before reloading and reconciling to `awaiting_merge_approval`. Invalidate obsolete approvals atomically with `checking -> pushed`, then persist `pushed -> awaiting_merge_approval` separately. The normal worker and all reconciliation scanners must acquire the lease before any reconciliation version write so an active push cannot lose its optimistic version.

- [ ] **Step 5: Add low-noise notifications and run tests**

Only notify task start, blocked, failure, and pushed completion. Completion includes branch, commit, every configured check command marked `（通过）`, summary, and exact `批准合并 #<id>`. Reserve a fixed suffix budget so truncation can never remove the approval command. Apply generic credential redaction once and the task log's exact-value redactor last, then bound every logical message before it reaches OneBot. Notification failure is retried a bounded number of times and logged without rolling back state or repeating external work.

Run: `go test ./internal/tasksvc -run TestScheduler -v && go test ./...`

Expected: PASS.

```bash
git add docs/superpowers/plans/2026-08-25-qq-codex-worker-implementation.md internal/config internal/model/task.go internal/store internal/tasksvc/ports.go internal/tasksvc/scheduler.go internal/tasksvc/scheduler_test.go
git commit -m "feat: schedule concurrent codex tasks"
```

### Task 11: Gate merge and RC deployment with immutable approvals

**Files:**
- Create: `internal/tasksvc/approval.go`
- Test: `internal/tasksvc/approval_test.go`
- Create: `cmd/qqcodex-ops/main_test.go`
- Modify: `internal/tasksvc/service.go`
- Modify: `internal/tasksvc/scheduler.go`
- Modify: `internal/tasksvc/ports.go`
- Modify: `internal/model/task.go`
- Modify: `internal/model/task_test.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/sqlite_test.go`
- Modify: `internal/ops/operator.go`
- Modify: `internal/ops/operator_test.go`
- Modify: `internal/ops/client.go`
- Modify: `internal/ops/client_test.go`
- Modify: `cmd/qqcodex-ops/main.go`

- [ ] **Step 1: Write failing approval tests**

Verify:

- Employee approval is rejected without a status change.
- Admin merge approval stores an approval bound to `TaskCommit` and transitions to `merging`.
- Remote task commit mismatch causes `merge_conflict`/failure and never pushes RC.
- Successful merge stores returned RC commit and enters `awaiting_deploy_approval`.
- Deploy approval binds the current RC commit and transitions to `deploying`.
- RC commit change invalidates deployment and returns to `awaiting_deploy_approval` with a failure message.
- Deployment failure becomes `deploy_failed`; a new admin message can retry.
- Duplicate approval message IDs do not execute merge/deploy twice.
- Notification replay reads the immutable approval row by group/message ID rather than a task commit that may have changed later.
- Two scheduler instances claim only one approval, and different tasks for one project cannot merge/deploy concurrently.
- An interrupted deploy remains `executing` and is never automatically replayed.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/tasksvc -run 'TestMergeApproval|TestDeployApproval' -v`

Expected: FAIL because approval handlers are missing.

- [ ] **Step 3: Implement approval command handling**

In `approval.go`, implement `approveMerge` and `approveDeploy`. Both check admin role, exact current state, non-empty bound commit, optimistic version, and unique message ID before waking the scheduler. Atomically persist the approval, command input, audit, processed-message claim, and transition to the external-operation state. Deployment approval also persists the deterministic `QQCODEX_DEPLOY_KEY` derived from project ID, task ID, and exact RC commit.

- [ ] **Step 4: Implement serialized merge and deployment jobs**

Scheduler behavior:

```text
merging:
  load latest task and approval -> verify bound TaskCommit -> Operator.MergeRC
  -> save RCCommit -> merged -> awaiting_deploy_approval -> notify

deploying:
  load latest task and approval -> verify bound RCCommit -> Operator.DeployRC
  -> deployed or deploy_failed -> notify
```

Atomically change only the newest exact `approved` row to `executing` and acquire a durable per-project SQLite operation lock before any external call. Use one in-process release mutex so merge and deploy cannot overlap, and hold the existing project Git metadata mutex around each operator call so they cannot overlap `Sync`/`Prepare`/`PushTask`. Reload the task immediately before each call. Complete only the claimed approval identity (`group_id` + `message_id`), not every approval with the same commit.

The ops helper response exposes stable `task_commit_changed`, `rc_commit_changed`, and `merge_conflict` codes. The client maps them to typed errors without matching public error text. A changed RC response includes the current exact RC commit; the scheduler validates and persists it, clears the old deploy key, invalidates the approval, and returns to `awaiting_deploy_approval`. Generic helper/check/persistence failures become `failed`, while actual merge conflicts become `merge_conflict`.

Never retry an `executing` approval automatically. If a process stops after the claim or external side effect, startup recovery changes `merging` to `failed` and `deploying` to `deploy_failed`, then releases the durable project lock. A new deploy approval message is required after RC mismatch, interrupted deployment, or `deploy_failed`. Reconcile `merged` to `awaiting_deploy_approval` without repeating the external merge.

- [ ] **Step 5: Run tests and commit milestone 2**

Run: `go test ./internal/tasksvc -v && go test ./...`

Expected: PASS.

```bash
git add docs/superpowers/plans/2026-08-25-qq-codex-worker-implementation.md cmd/qqcodex-ops internal/model internal/ops internal/store internal/tasksvc
git commit -m "feat: gate rc merge and deployment"
```

## Milestone 3: NapCat Integration and End-to-End Verification

### Task 12: Decode and encode OneBot 11 group messages safely

Before any external deploy call, persist `deploying` and the exact RC commit/`QQCODEX_DEPLOY_KEY`; after restart do not automatically replay the action. Reconciliation of the same key is manual and must rely on the durable deployment target's duplicate-key guarantee.

**Files:**
- Create: `internal/onebot/protocol.go`
- Test: `internal/onebot/protocol_test.go`

- [ ] **Step 1: Write protocol tests from OneBot 11 payloads**

Cover numeric and quoted IDs, group message segment arrays, self mention stripping, text concatenation, non-target self IDs, private messages, action responses with echo, CQ escaping for `&`, `[`, `]`, and commas, and rune-safe splitting at 1200 runes.

Use a representative event:

```json
{
  "post_type":"message",
  "message_type":"group",
  "message_id":9988,
  "group_id":123456,
  "user_id":654321,
  "self_id":111222,
  "message":[
    {"type":"at","data":{"qq":"111222"}},
    {"type":"text","data":{"text":" [orders] fix duplicate submit"}}
  ]
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/onebot -run TestProtocol -v`

Expected: FAIL because protocol types are missing.

- [ ] **Step 3: Implement normalized protocol types**

Define a custom `ID string` with `UnmarshalJSON` accepting only quoted decimal strings or JSON integers. Define `Segment`, `GroupMessage`, `ActionRequest`, and `ActionResponse`.

Implement:

```go
func DecodeGroupMessage(raw []byte, selfID string) (GroupMessage, bool, error)
func SendGroupAction(groupID, text, echo string) (ActionRequest, error)
func EscapeCQText(string) string
func SplitMessage(string, maxRunes int) []string
```

Reject floating-point IDs, negative IDs, non-decimal strings, missing fields, and events not explicitly equal to `post_type=message` plus `message_type=group`. Mark `Mentioned` only when an `at` segment targets the configured self ID.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/onebot -run TestProtocol -v && go test ./...`

Expected: PASS.

```bash
git add internal/onebot/protocol.go internal/onebot/protocol_test.go
git commit -m "feat: add onebot group message codec"
```

### Task 13: Connect to NapCat over authenticated WebSocket

**Files:**
- Create: `internal/onebot/client.go`
- Test: `internal/onebot/client_test.go`

- [ ] **Step 1: Write WebSocket server tests**

Use `httptest.Server` plus `websocket.Accept` to verify the client sends `Authorization: Bearer <token>`, dispatches group events, correlates action responses by echo, returns action errors, reconnects after a server close, and exits promptly when context is canceled.

- [ ] **Step 2: Run tests and verify they fail**

Run: `go test ./internal/onebot -run TestClient -v`

Expected: FAIL because `Client` does not exist.

- [ ] **Step 3: Implement one reader, one writer, and reconnect control**

Define:

```go
type Handler func(context.Context, GroupMessage) error

type Client struct {
	URL, Token, SelfID string
	MessageRunes       int
	// private connection, send queue, pending map, and logger fields
}

func (c *Client) Run(context.Context, Handler) error
func (c *Client) Send(context.Context, string, string) error
```

Only the writer goroutine may call `wsjson.Write`; only the reader goroutine may call `wsjson.Read`. `Send` splits/escapes text, creates an unpredictable echo with `crypto/rand`, queues an action, and waits for the matching response or context cancellation. Remove pending entries on every completion path.

Reconnect with delays 1s, 2s, 4s, 8s, then 15s maximum. Reset delay after a connection stays healthy for 30 seconds. Do not log the token or full WebSocket URL query.

- [ ] **Step 4: Run tests and commit**

Run: `go test -race ./internal/onebot -run TestClient -v && go test ./...`

Expected: PASS with no race reports.

```bash
git add internal/onebot/client.go internal/onebot/client_test.go
git commit -m "feat: connect to napcat websocket"
```

### Task 14: Wire the application, recovery, and end-to-end flow

**Files:**
- Create: `internal/app/app.go`
- Test: `internal/app/app_integration_test.go`
- Create: `cmd/qqcodex/main.go`

- [ ] **Step 1: Write an end-to-end integration test with fakes**

The test must start a fake OneBot WebSocket server, temporary SQLite database, temporary Git remote/repository, fake Codex executable, fake ops client, real service/scheduler, and then drive:

```text
create -> plan reply -> confirm -> execute -> checks -> commit -> push
-> admin approve merge -> merged -> admin approve deploy -> deployed
```

Also inject a duplicate confirm event and assert it does not execute twice. Assert the final task row contains task commit, RC commit, session ID, and `deployed` status.

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/app -run TestEndToEnd -v`

Expected: FAIL because `App` and command wiring do not exist.

- [ ] **Step 3: Implement application coordination**

Define `App.Run(ctx)` to:

1. Acquire the SQLite sibling runtime lock with `Store.AcquireRuntimeLock`, hold it until every application goroutine exits, then pass that same held guard to `Store.RecoverInterrupted`. Fail startup if the non-blocking lock is already owned. Never call recovery before acquiring the guard and never use a TTL lease for this process-level exclusion.
2. Start scheduler in a goroutine.
3. Run OneBot client and pass only messages from configured groups.
4. Process each accepted message in a worker pool bounded by `Config.MessageWorkers` so Codex planning cannot block the WebSocket reader.
5. Convert OneBot `GroupMessage` to `tasksvc.Message` and call `Service.Handle`.
6. When `Service.Handle` returns `tasksvc.ErrNotificationDelivery`, retry the same immutable `tasksvc.Message` with bounded attempts/backoff; service replay must resend the deterministic response without repeating planner, state mutation, audit records, scheduler wakeup, or cancellation. Do not retry authorization, parser, configuration, planner, or store errors.
7. Return fatal store/config errors; keep OneBot reconnect errors inside the client.
8. Cancel scheduler and wait for goroutines on shutdown.

- [ ] **Step 4: Implement the main command**

`cmd/qqcodex/main.go` accepts `-config` only, loads config, reads the NapCat token from the configured environment variable, opens SQLite, creates one `tasklog.Store`, passes that same store to the Codex runner, ops client, task service, and `NewScheduler(..., logs TaskLogs, ...)`, creates structured `slog` JSON logging, constructs all dependencies, and handles SIGINT/SIGTERM with `signal.NotifyContext`.

At startup, fail with concise errors when the Codex binary, ops command, database directory, log directory, or repository registry is invalid. Never print token values or the value of `CODEX_API_KEY`.

- [ ] **Step 5: Run integration, race, and full tests**

Run:

```bash
go test ./internal/app -run TestEndToEnd -v
go test -race ./internal/onebot ./internal/tasksvc ./internal/app
go test ./...
go vet ./...
```

Expected: all commands PASS with no race or vet findings.

- [ ] **Step 6: Commit**

```bash
git add internal/app cmd/qqcodex
git commit -m "feat: wire qq codex worker service"
```

### Task 15: Document operation and perform final hardening

**Files:**
- Create: `README.md`
- Modify: `configs/qqcodex.example.json`
- Modify: `configs/ops.example.json`
- Modify: `cmd/qqcodex/main.go`
- Modify: `internal/app/app.go`
- Test: `internal/app/app_integration_test.go`

- [ ] **Step 1: Add explicit expired-artifact cleanup**

Add `App.CleanupExpired(ctx)` and the main flag `-cleanup-expired`. In cleanup mode, do not connect OneBot or start the scheduler. Load only failed/cancelled candidates, apply each project's `LogRetentionDays`, remove its local worktree through `gitwork.Manager.Remove`, then remove its local task log. Keep SQLite task/audit rows and every remote task branch. If worktree removal fails, keep the log and report the task ID; never use recursive deletion on an unresolved path.

Extend integration tests to prove a seven-day-old failed task is cleaned, a recent failed task is retained, and queued/merge-conflict/deployed tasks are never removed.

- [ ] **Step 2: Write the operator documentation**

Document exact local steps:

1. Install/login a dedicated QQ account in NapCat and enable OneBot 11 array messages.
2. Bind NapCat to `127.0.0.1`, enable access token, and export `NAPCAT_ACCESS_TOKEN`.
3. Install/login Codex with the company credential or export `CODEX_API_KEY`.
4. Copy the two example JSON configs to untracked local files and fill IDs/paths.
5. Build `go build -o bin/qqcodex ./cmd/qqcodex` and `go build -o bin/qqcodex-ops ./cmd/qqcodex-ops`.
6. Run fake/local Git and deploy validation before adding an RC project.
7. Run `./bin/qqcodex -config ./configs/qqcodex.local.json`.

Include all QQ commands, state meanings, logs/retention, NapCat account-risk warning, recovery procedure, and the rule that production credentials must never be configured.

- [ ] **Step 3: Document credential separation before real RC enablement**

Provide an example deployment in which `qqcodex` runs as `qqcodex-worker`, `qqcodex-ops` runs through a root-owned wrapper or constrained service account, configs are readable only by their owning accounts, repository/worktree directories use a shared group, and only typed ops subcommands are allowed. State that direct `sudo sh`, arbitrary command arguments, and a shared private key readable by Codex are forbidden.

- [ ] **Step 4: Add final negative-path integration coverage**

Extend the integration test to cover unauthorized group, unauthorized user, stale approval hash, merge conflict, deploy failure, service restart during execution, message output splitting, and secret redaction. Assert no RC operation occurs for every rejected case.

- [ ] **Step 5: Format and run the full verification suite**

Run:

```bash
gofmt -w cmd internal
go mod tidy
git diff --check
go test ./...
go test -race ./internal/onebot ./internal/tasksvc ./internal/app
go vet ./...
go build ./cmd/qqcodex ./cmd/qqcodex-ops
```

Expected: clean diff check; all tests, race tests, vet, and builds succeed.

- [ ] **Step 6: Perform a manual sandbox-group acceptance test**

With a dedicated QQ test group, local bare Git remote, and fake deploy script, verify create, confirm, two concurrent tasks, supplement, cancel, status, logs, merge approval, deploy approval, and service restart. Save task IDs and resulting commit hashes in local operator notes, not in the repository.

- [ ] **Step 7: Commit milestone 3**

```bash
git add README.md configs cmd/qqcodex internal/app go.mod go.sum
git commit -m "docs: add qq codex worker operations guide"
```

## Final Acceptance Gate

Do not connect a real RC repository until every item below is true:

- [ ] Dedicated QQ account and test group passed the manual flow.
- [ ] NapCat listens only on loopback and requires a token.
- [ ] Employee/admin IDs and allowed group IDs are explicitly configured.
- [ ] Every project has fixed absolute paths, base/RC refs, checks, concurrency, and deploy action.
- [ ] `qqcodex` cannot read the ops credential store or production credentials.
- [ ] No production repository, branch, host, or secret exists in either config.
- [ ] Full tests, race tests, vet, and both builds pass.
- [ ] A merge conflict and a failed deploy were observed to stop safely in tests.
- [ ] Approvals were observed to bind exact task/RC commit hashes.
- [ ] RC operators understand how to stop the service and inspect SQLite/logs before retrying interrupted work.
- [ ] Worker and ops use physically separate repositories; task commits cross only as bounded single-ref bundles over helper stdin, and worker cannot read ops `.git`, refs, config, credentials, or deployment files.
- [ ] Real check-runner acceptance proves no ops HOME, parent `/proc`, credential-file, network, common-Git, UID/cgroup/PID, or filesystem escape; protected RC has ops as its sole writer.
- [ ] Ops config, helper, wrapper, and executable ownership is root or the dedicated ops identity; public client/source Git construction rejects worker-owned binaries; startup hard-fails when any gate is absent.
- [ ] Deployment target durably deduplicates `QQCODEX_DEPLOY_KEY`; `deploying` is persisted before the external action, restart never auto-replays it, and same-key reconciliation is manual.
