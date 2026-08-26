# QQ 咨询模式 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `subagent-driven-development` (recommended) or `executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 允许授权群成员 @范范进行通用或项目只读咨询，同时不改变任务确认和执行边界。

**Architecture:** 保持任务服务和 scheduler 不变，在 `internal/consultsvc` 中添加只读、无状态的咨询服务。应用层根据解析出的命令种类路由到咨询或任务服务；SQLite 咨询记录保存最终的已脱敏回复，从而让同一 OneBot message ID 重放回复而不重复调用 Codex。

**Tech Stack:** Go 1.26、SQLite、Codex CLI、OneBot 11、现有 tasklog/authorizer。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/config/config.go` | 定义、加载和校验咨询工作目录与超时。 |
| `internal/command/parser.go` | 无歧义地区分任务、项目咨询和通用咨询。 |
| `internal/model/consultation.go` | 定义独立于任务状态机的持久化咨询记录。 |
| `internal/store/schema.sql`、`internal/store/sqlite.go` | 保存、领取和完成咨询记录。 |
| `internal/tasklog/log.go` | 为咨询日志允许受限的 `Q-` ID。 |
| `internal/codex/runner.go` | 以只读、临时会话方式运行一次咨询。 |
| `internal/consultsvc/service.go` | 授权、去重、调用、脱敏和回群。 |
| `internal/app/app.go` | 按已解析命令路由并复用现有通知重试。 |
| `cmd/qqcodex/main.go` | 创建咨询服务并验证安全咨询目录。 |
| `configs/qqcodex.example.json`、`README.md` | 暴露安全配置与群消息格式。 |

### Task 1: Add consultation configuration and private-directory validation

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/qqcodex/main.go`
- Modify: `cmd/qqcodex/main_test.go`
- Modify: `configs/qqcodex.example.json`

- [ ] **Step 1: Write failing configuration tests**

Add a `ConsultationConfig` fixture and test that a configuration without an absolute workspace or with a non-positive timeout is rejected. Add startup tests proving the workspace is created with private permissions and that it is rejected when it overlaps the database parent, log root, worktree root, or a project repo.

```go
cfg.Consultation = config.ConsultationConfig{
    Workspace: filepath.Join(root, "consultation"),
    TimeoutSeconds: 90,
}
cfg.Consultation.Workspace = cfg.LogDir
if err := validateStartup(&cfg); err == nil {
    t.Fatal("accepted overlapping consultation workspace")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config ./cmd/qqcodex -run 'Test(Validate|Load|ValidateStartup)' -count=1`

Expected: FAIL because `Config` has no `Consultation` field and startup does not validate the workspace.

- [ ] **Step 3: Write minimal implementation**

Add the following field and type:

```go
type Config struct {
    // existing fields
    Consultation ConsultationConfig `json:"consultation"`
}

type ConsultationConfig struct {
    Workspace      string `json:"workspace"`
    TimeoutSeconds int    `json:"timeout_seconds"`
}
```

Require an absolute workspace and a positive timeout in `config.Validate`. Add the workspace to `validateStartup`, `validateCleanupStartup`, and `validatePathOverlap`; use the existing `ensurePrivateDirectory` helper so the leaf directory is created at `0700` only below a secure existing parent. Extend all valid test fixtures and the example JSON with:

```json
"consultation": {
  "workspace": "/srv/qqcodex-worker/consultation",
  "timeout_seconds": 90
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config ./cmd/qqcodex -run 'Test(Validate|Load|ValidateStartup)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/qqcodex/main.go cmd/qqcodex/main_test.go configs/qqcodex.example.json
git commit -m "feat: configure private consultation workspace"
```

### Task 2: Parse consultation messages without making task creation ambiguous

**Files:**
- Modify: `internal/command/parser.go`
- Modify: `internal/command/parser_test.go`

- [ ] **Step 1: Write the failing test**

Add tests for these exact commands:

```go
{name: "general consultation", text: "这个报错是什么意思？", mentioned: true,
 want: Command{Kind: KindConsult, Body: "这个报错是什么意思？"}},
{name: "project consultation", text: "问 [orders] 为什么会死锁？", mentioned: true,
 want: Command{Kind: KindProjectConsult, ProjectAlias: "orders", Body: "为什么会死锁？"}},
{name: "task still wins", text: "[orders] 修复死锁", mentioned: true,
 want: Command{Kind: KindCreate, ProjectAlias: "orders", Body: "修复死锁"}},
```

Also assert that a consultation without `Mentioned`, `问` without a valid `[alias] question`, and an empty generic message are rejected. Keep existing fixed commands valid with or without a mention.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/command -run TestParse -count=1`

Expected: FAIL because consultation kinds do not exist.

- [ ] **Step 3: Write minimal implementation**

Add `KindConsult` and `KindProjectConsult`. Parse fixed task commands first, then current `[alias] body` task creation, then exact `问 [alias] body` project consultation. Only after those forms, treat a non-empty mentioned message as `KindConsult` with its trimmed text in `Body`; non-mentioned plain text remains unrecognized.

A message beginning with `问` is an explicit project-consultation attempt: malformed or missing project/body returns an error rather than silently becoming generic chat.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/command -run TestParse -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/command/parser.go internal/command/parser_test.go
git commit -m "feat: parse qq consultation commands"
```

### Task 3: Persist consultation replies for exactly-once Codex calls

**Files:**
- Create: `internal/model/consultation.go`
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/sqlite_test.go`

- [ ] **Step 1: Write the failing test**

Create a test that inserts a pending consultation, reads it back by `(groupID, messageID)`, completes it with the active lease token, and verifies the stored reply. Add cases showing: a second insert conflicts, an expired pending consultation can be atomically reclaimed by a new token, and a stale token cannot overwrite the reply.

```go
record := model.Consultation{
    ID: "Q-012345ABCDEF", GroupID: "g1", MessageID: "m1", UserID: "u1",
    Question: "why", LeaseToken: "owner-a", LeaseExpiresAt: now.Add(time.Minute),
    CreatedAt: now, UpdatedAt: now,
}
if err := db.CreateConsultation(ctx, &record); err != nil { t.Fatal(err) }
if err := db.CompleteConsultation(ctx, record.ID, "owner-a", "answer", now); err != nil { t.Fatal(err) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run Consultation -count=1`

Expected: FAIL because the model, schema, and Store methods do not exist.

- [ ] **Step 3: Write minimal implementation**

Define `model.Consultation` with `ID`, `GroupID`, `MessageID`, `UserID`, `ProjectID`, `Question`, `Reply`, `LeaseToken`, `LeaseExpiresAt`, `CompletedAt`, `CreatedAt`, and `UpdatedAt`.

Create a `consultations` table with primary key `id` and `UNIQUE(group_id, message_id)`. Add these Store methods:

```go
func (s *Store) CreateConsultation(context.Context, *model.Consultation) error
func (s *Store) GetConsultationByMessage(context.Context, groupID, messageID string) (*model.Consultation, error)
func (s *Store) TryReclaimConsultation(context.Context, id, newToken string, leaseUntil, now time.Time) (bool, error)
func (s *Store) CompleteConsultation(context.Context, id, leaseToken, reply string, now time.Time) error
```

`CompleteConsultation` updates only an incomplete row owned by `leaseToken`; zero affected rows return `store.ErrConflict`. Do not write a consultation into `tasks`, `task_inputs`, `audit_events`, or `processed_messages`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store -run Consultation -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/model/consultation.go internal/store/schema.sql internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat: persist qq consultation replies"
```

### Task 4: Add a read-only, ephemeral Codex consultation invocation

**Files:**
- Modify: `internal/tasklog/log.go`
- Modify: `internal/tasklog/log_test.go`
- Modify: `internal/codex/runner.go`
- Modify: `internal/codex/runner_test.go`

- [ ] **Step 1: Write the failing test**

First add a tasklog test that accepts `Q-012345ABCDEF` and keeps rejecting malformed IDs. Then add a Runner test using the existing helper executable that calls `Ask` and asserts its argument record contains `exec`, `-C`, `--sandbox read-only`, `--ephemeral`, `--skip-git-repo-check`, `--json`, and `-o /proc/self/fd/3`; it must not contain `workspace-write`, `--add-dir`, or a session resume command.

```go
result, err := runner.Ask(context.Background(), Request{
    TaskID: "Q-012345ABCDEF", WorkingDir: workingDir,
    Prompt: "consult", Timeout: time.Second,
})
if err != nil || result.Final == "" { t.Fatalf("Ask() = %#v, %v", result, err) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tasklog ./internal/codex -run 'Test(TaskLog.*Consultation|Runner.*Ask)' -count=1`

Expected: FAIL because `Q-` IDs and `Runner.Ask` are unsupported.

- [ ] **Step 3: Write minimal implementation**

Expand only the log-ID validation to accept `T-` or `Q-` followed by 12 uppercase hex characters. Add an `invocationConsult` enum value and:

```go
func (r Runner) Ask(ctx context.Context, req Request) (Result, error) {
    return r.run(ctx, req, invocationConsult)
}
```

For this invocation, retain the existing sanitized environment, output capture, process-group cancellation, redacted logging and size limits. Generate CLI args equivalent to:

```go
[]string{"exec", /* environment policy */, "-C", req.WorkingDir,
    "--sandbox", "read-only", "--ephemeral", "--skip-git-repo-check",
    "--json", "-o", finalOutputPath, "--", req.Prompt}
```

Do not require `GitCommonDir` or a returned session ID for consultation. Add `ConsultationPrompt(projectID, question string)` that JSON-encodes the untrusted question and states that Codex must answer only, must not write files, run Git/ops/deploy commands, reveal secrets, create tasks, or use web search.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tasklog ./internal/codex -run 'Test(TaskLog.*Consultation|Runner.*Ask)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tasklog/log.go internal/tasklog/log_test.go internal/codex/runner.go internal/codex/runner_test.go
git commit -m "feat: run codex qq consultations read only"
```

### Task 5: Implement the isolated consultation service

**Files:**
- Create: `internal/consultsvc/service.go`
- Create: `internal/consultsvc/service_test.go`

- [ ] **Step 1: Write the failing test**

Build fakes for the `Ask` runner and notifier. Test: unauthorized group/user causes no runner or Store calls; generic consultation selects `Consultation.Workspace`; project consultation selects the configured `RepoPath`; unknown aliases cause no runner call; a completed duplicate replays its stored reply; and a notifier failure returns `tasksvc.ErrNotificationDelivery` while a retry does not call Codex twice.

Also assert a successful consultation does not create a `Task`, wake a scheduler, or call an ops fake.

```go
message := tasksvc.Message{GroupID: "g1", UserID: "u1", MessageID: "m1", Mentioned: true, Text: "是什么原因？"}
if err := svc.Handle(context.Background(), message); err != nil { t.Fatal(err) }
if got := runner.LastRequest().WorkingDir; got != cfg.Consultation.Workspace {
    t.Fatalf("working directory = %q", got)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/consultsvc -count=1`

Expected: FAIL because the package and service do not exist.

- [ ] **Step 3: Write minimal implementation**

Define a `Service` that receives only `*config.Registry`, `config.ConsultationConfig`, `*store.Store`, `auth.Authorizer`, an `Ask(context.Context, codex.Request)` runner, the existing `tasksvc.Notifier`, and a redactor. Keep its `Handle` signature compatible with `tasksvc.Message` so the app can reuse its queue and retry policy.

Derive a stable ID as `Q-` plus the first six bytes of SHA-256 over `groupID + "\\x00" + messageID`. Parse and authorize before persisting. For a newly created record, generate a cryptographically random lease token, invoke `Runner.Ask`, sanitize and bound the final answer with the existing task-service redaction rules, then atomically complete the record before sending it.

For an existing completed record, send its saved reply. For an active pending record, wait with a short context-aware poll; after its lease expires, atomically reclaim it and make one new call. If `Ask` fails, complete the owned record with the fixed reply `咨询暂时无法完成，请稍后重试` and return the underlying error only after the reply is durable. Store failures wrap `tasksvc.ErrFatalStore`; notification failures join `tasksvc.ErrNotificationDelivery`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/consultsvc -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/consultsvc/service.go internal/consultsvc/service_test.go
git commit -m "feat: answer qq consultations"
```

### Task 6: Route messages and wire the production dependencies

**Files:**
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_integration_test.go`
- Modify: `cmd/qqcodex/main.go`
- Modify: `cmd/qqcodex/main_test.go`

- [ ] **Step 1: Write the failing test**

Add a routing test using fake task and consultation handlers. A mentioned `普通问题` must call only consultation; a mentioned `[p] 修复问题` and an unmentioned `确认 #T-...` must call only task service. Add an integration test that sends a real OneBot `at` event for a generic consultation, receives the fake Codex final reply, confirms that no task exists for the message, and resends the event without incrementing the fake consultation call count.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app -run '(TestRunRoutesConsultations|TestEndToEndConsultation)' -count=1`

Expected: FAIL because `App` has no consultation handler or routing logic.

- [ ] **Step 3: Write minimal implementation**

Add a required `Consultations` handler to `app.App`. In `handleMessage`, call `command.Parse` once: route only `KindConsult` and `KindProjectConsult` to `Consultations.Handle`; all other commands and parse errors continue to `Service.Handle`, preserving the current task behavior and task-generated validation errors. Reuse the same retry loop so only the shared notification-delivery sentinel retries.

In `buildAndRun`, construct one `consultsvc.Service` with the same authorizer, Registry, Store, Runner, OneBot notifier and task log redactor already created for task processing. Do not pass scheduler, worktree manager, operator or ops configuration to it. Update app fixtures to provide a no-op consultation handler where the test does not exercise this route.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/app ./cmd/qqcodex -run '(TestRunRoutesConsultations|TestEndToEndConsultation|TestValidateStartup)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/app.go internal/app/app_integration_test.go cmd/qqcodex/main.go cmd/qqcodex/main_test.go
git commit -m "feat: route qq consultations"
```

### Task 7: Document the message contract and verify the complete system

**Files:**
- Modify: `README.md`
- Modify: `configs/qqcodex.example.json`
- Modify: `configs/qqcodex.local.json` (ignored local runtime configuration; do not commit)

- [ ] **Step 1: Update operator documentation**

Replace the statement that the robot has no free-form interaction with a command table documenting:

```text
@范范 这个错误如何排查？
@范范 问 [qqbot] 这个锁为什么会死锁？
@范范 [qqbot] 修复这个锁问题
```

State that consultations are single-message, reply-only, read-only, no-web-search calls; they do not create tasks or inherit chat history, and only allowed groups/users can invoke them. Document the separate private `consultation.workspace` directory and timeout.

- [ ] **Step 2: Update the ignored local runtime configuration**

Read `configs/qqcodex.local.json`, choose a new private sibling directory such as `/home/fanfan007/.local/share/qqcodex-worker/consultation`, and add:

```json
"consultation": {
  "workspace": "/home/fanfan007/.local/share/qqcodex-worker/consultation",
  "timeout_seconds": 90
}
```

Ensure the configured parent is owned by the worker user and private. Do not add this ignored local file to Git.

- [ ] **Step 3: Run the complete verification suite**

Run:

```bash
gofmt -w internal/config/config.go internal/command/parser.go internal/model/consultation.go internal/store/sqlite.go internal/tasklog/log.go internal/codex/runner.go internal/consultsvc/service.go internal/app/app.go cmd/qqcodex/main.go
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/qqcodex ./cmd/qqcodex-ops
```

Expected: every command exits zero.

- [ ] **Step 4: Restart and inspect the local worker**

Build the local binary, restart only the existing `qqcodex` worker through `/home/fanfan007/.local/share/qqcodex-worker/start-worker.sh`, then verify its process is connected only to `127.0.0.1:3001` and its environment contains `CODEX_API_KEY` but not `OPENAI_API_KEY`. Do not send a production QQ message as part of automated verification.

- [ ] **Step 5: Commit source documentation and report the local-only config separately**

```bash
git add README.md configs/qqcodex.example.json
git commit -m "docs: describe qq consultation mode"
git status --short
```

Expected: the working tree is clean apart from the intentionally ignored `configs/qqcodex.local.json` change.

## Plan self-review

- Configuration, parser, persistence, runner, service, routing, docs, local setup, and final verification each have an implementation task.
- Task creation keeps the existing `[project]` form; project consultation requires `问 [project]`, so intent is deterministic.
- The only new durable state is a consultation record; no task, scheduler, Git, ops, or deployment path accepts consultation input.
- Each production-code task begins with a focused failing test and ends with a passing verification command and commit.

