# QQ Codex Worker

QQ Codex Worker 把指定 QQ 群中的严格命令转换为可审计的 Codex 任务。当前版本面向本地工作站和 RC 环境验证，不是生产发布系统。

> **安全边界：只允许配置 RC 仓库、RC 分支、RC 部署目标和 RC 凭据。绝不能把生产仓库、生产主机或生产密钥写入任一配置。**

当前代码会校验命令、权限、Git 引用、审批提交和 worker/ops Git common directory 隔离，但在同一台本地电脑上运行两个进程，不能证明真实的 OS 身份隔离、凭据隔离或部署网络隔离。真实 RC 启用前必须完成本文的人工上线门禁。

## 组件与信任边界

```text
QQ 群
  -> NapCat OneBot 11 WebSocket
  -> qqcodex（worker 身份）
       SQLite / 日志 / worker source clone / task worktree / Codex API key
  -> 受限 typed wrapper
  -> qqcodex-ops（ops 身份）
       ops config / 独立 ops clone / Git 与 RC 凭据 / 固定部署 argv
```

- `qqcodex` 读取 QQ 消息、调用 Codex、执行 worker 配置中的检查并维护 SQLite 状态。
- `qqcodex-ops` 只接受 `validate`、`sync`、`push`、`merge`、`deploy` 五类 typed action。worker 不读取 ops 配置。
- worker 与 ops 必须使用物理独立的 Git clone。不同 worktree 但共享同一个 Git common directory 仍会被拒绝。
- worker 配置中的 `deploy_action` 只是项目元数据 ID，不会作为命令执行。真正执行的是 ops 配置中的固定 `deploy_action` argv。
- 两份配置的 `id`、`remote`、`base_branch`、`rc_branch` 和 `checks` 必须一致；启动预检会比较它们的稳定指纹。

## 本地准备

### 1. NapCat 与专用 QQ

1. 准备一个只用于机器人的 QQ 账号，只加入测试群和明确允许的公司群。
2. 在本机安装并登录 NapCat，启用 OneBot 11 WebSocket 服务。
3. WebSocket 只监听 `127.0.0.1`，设置强 access token，并把消息格式设为 OneBot 11 array message。
4. 记录机器人 QQ 号、允许的群号、员工 QQ 号和管理员 QQ 号。配置中均使用十进制字符串。
5. 不要把 NapCat WebSocket 暴露到局域网或公网。

普通 QQ 账号使用第三方机器人框架存在限制登录、风控或封号风险，应用无法消除该风险。先用专用账号和测试群验证，账号异常时立即停止服务。

### 2. Codex 凭据

当前 Runner 要求公司分配的 `CODEX_API_KEY`，并主动拒绝 `CODEX_HOME/auth.json` 或 `$HOME/.codex/auth.json` 中的缓存登录凭据。不要给 Codex Git 推送、RC 部署或生产凭据。

```bash
export CODEX_API_KEY='company-codex-api-key'
export CODEX_HOME="$HOME/.codex-qq-worker"
mkdir -p "$CODEX_HOME"
chmod 700 "$CODEX_HOME"
```

如果当前 `$HOME/.codex/auth.json` 已存在，仅修改 `CODEX_HOME` 仍会被拒绝。请用专用 worker OS 账号或隔离的 `HOME` 启动服务，不要为了运行机器人删除个人 Codex 登录文件。

`codex.environment_keep` 只添加任务确实需要的非敏感环境变量。`PATH`、`HOME`、语言和临时目录变量会自动保留；`CODEX_API_KEY` 不需要加入该列表。

### 3. 配置与目录

复制示例到不会提交的本地文件，并将其中路径和 ID 替换为绝对值：

```bash
cp configs/qqcodex.example.json configs/qqcodex.local.json
cp configs/ops.example.json configs/ops.local.json
git status --short
```

确认本地配置未被 Git 跟踪；必要时把具体文件名加入本地 `.git/info/exclude`。配置文件和凭据文件使用 `0600`，目录使用 `0700`。数据库父目录必须预先存在且属于运行用户；日志目录和 worktree 根可以由程序创建缺失的最后一级目录，但父目录必须安全。

准备两个独立 clone，并预先 fetch 配置的 base/RC 远端引用：

```bash
git clone ssh://git@git.example.com/company/order-api.git /srv/qqcodex-worker/repos/order-api
git clone ssh://git@git.example.com/company/order-api.git /srv/qqcodex-ops/repos/order-api
git -C /srv/qqcodex-worker/repos/order-api fetch origin main rc
git -C /srv/qqcodex-ops/repos/order-api fetch origin main rc
```

不要使用同一 clone 的两个 worktree 代替两个 clone。worker 的数据库父目录、日志目录、worktree 根和每个 source repo 之间也不能互相包含。

### 4. 构建与启动

需要 Linux、Git、Go 1.26 或更高版本，以及可执行的 `codex` CLI：

```bash
go build -o bin/qqcodex ./cmd/qqcodex
go build -o bin/qqcodex-ops ./cmd/qqcodex-ops

realpath bin/qqcodex-ops
realpath configs/ops.local.json
```

仅做本地假 RC 功能验证时，把 `qqcodex.local.json` 的 `ops_command` 改为上述两个绝对路径，例如 `["/absolute/path/bin/qqcodex-ops", "-config", "/absolute/path/configs/ops.local.json"]`。该方式让两个进程共享当前 OS 身份，不满足真实 RC 隔离要求。

然后启动 worker：

```bash
export NAPCAT_ACCESS_TOKEN='onebot-access-token'
export CODEX_API_KEY='company-codex-api-key'
./bin/qqcodex -config ./configs/qqcodex.local.json
```

启动会先校验 Codex、数据库和目录安全性、worker Git 仓库与远端引用、检查命令、ops 项目元数据、ops 仓库引用，以及两个 Git common directory 不相同。任一预检失败时不会启动 OneBot 或 scheduler。

直接把 `bin/qqcodex-ops` 写进 `ops_command` 只适合本地功能验证，不能提供 OS 级凭据隔离。真实 RC 必须使用下文所述的受限 wrapper 或等价服务边界。

## 群命令

任务编号格式为 `T-` 加 12 位大写十六进制字符。机器人不提供自由形式 shell 命令，也没有 `help` 管理命令。

| 操作 | 群消息 | 权限与条件 |
| --- | --- | --- |
| 创建 | `@机器人 [orders] 修复重复提交并补测试` | 指定群内的白名单员工；必须 @ 机器人；`orders` 是项目 alias |
| 确认 | `确认 #T-012345ABCDEF` | 仅任务发起人；只允许 `awaiting_confirmation` |
| 补充 | `补充 #T-012345ABCDEF 增加兼容旧数据的迁移` | 发起人或管理员；只允许 `blocked` 或 `awaiting_merge_approval` |
| 取消 | `取消 #T-012345ABCDEF` | 发起人或管理员；仅在状态机允许且尚未合并时 |
| 状态 | `状态 #T-012345ABCDEF` | 发起人或管理员；任务必须属于当前群 |
| 日志 | `日志 #T-012345ABCDEF` | 发起人或管理员；返回经过截断和脱敏的本地日志摘要 |
| 批准合并 | `批准合并 #T-012345ABCDEF` | 仅管理员；只允许 `awaiting_merge_approval` |
| 批准部署 | `批准部署 #T-012345ABCDEF` | 仅管理员；允许 `awaiting_deploy_approval`，或对 `deploy_failed` 发起一次新重试 |

命令必须完全匹配上述中文和空格格式。非指定群在进入任务服务前被过滤；非白名单用户、跨群任务访问、员工审批和非发起人确认都会被拒绝。

## 状态与审批

| 状态 | 含义 |
| --- | --- |
| `draft` | 已持久化创建消息，正在规划或等待同一创建消息恢复规划 |
| `awaiting_confirmation` | 规划已返回，等待发起人确认 |
| `queued` | 已确认，等待项目并发槽位 |
| `running` | Codex 正在工作或恢复同一 session |
| `blocked` | Codex 需要补充信息 |
| `checking` | 执行配置检查、验证提交或推送任务分支 |
| `pushed` | 精确任务提交已推送，正在转换到审批状态 |
| `awaiting_merge_approval` | 等待管理员批准当前任务提交 |
| `merging` | ops 正在校验并合并到 RC |
| `merge_conflict` | 审批后的任务提交发生变化，或 RC 合并产生冲突，需要人工处理或新任务 |
| `merged` | 精确 RC 提交已持久化，正在转换到部署审批 |
| `awaiting_deploy_approval` | 等待管理员批准当前 RC 提交 |
| `deploying` | ops 正在部署精确 RC 提交 |
| `deploy_failed` | RC 部署失败；管理员检查后可用一条新的批准消息重试一次 |
| `deployed` | RC 部署完成 |
| `failed` | 规划、Codex、检查、Git、合并后检查或持久化流程失败 |
| `cancelled` | 任务在合并前被取消 |

管理员发送批准命令时，系统把审批记录绑定到数据库中当时的完整任务提交或 RC 提交哈希。管理员不在群消息中手填哈希。若远端任务提交或 RC 提交在审批后变化，当前审批失效，系统不会继续部署；必须检查新哈希后发送一条新的批准消息。重复的同一 QQ message ID 只重放通知，不重复外部副作用。

`max_concurrent` 限制单项目同时执行的 Codex 任务数；同一项目的 Git/RC 变更串行化，不同项目可并行。`message_workers` 只控制群消息处理并发，不改变项目执行上限。

## 日志、保留与清理

- SQLite 保存任务、输入、审批和 audit 行，是恢复与审计依据。
- 每个任务的本地日志位于 `log_dir`，使用私有权限和有界 JSONL 记录。
- access token、Codex API key、常见 credential assignment 和 Bearer 内容在落盘及回群前会过滤，但仍应避免把任何凭据写进任务描述或代码输出。
- QQ 的 `日志` 命令只返回有界摘要；长消息按 `onebot.message_runes` 以 Unicode 字符边界分段发送。
- `log_retention_days` 仅用于显式清理。服务不会自动清理。

停止正常 worker 后执行：

```bash
./bin/qqcodex -config ./configs/qqcodex.local.json -cleanup-expired
```

清理模式不需要 OneBot token 或 Codex API key，也不会连接 OneBot、启动 scheduler 或运行 Codex。它只选择超过各项目保留期的 `failed`、`cancelled` 任务，先通过受限路径校验移除本地 worktree，再删除本地任务日志。worktree 删除失败时日志会保留并报告任务 ID。SQLite 任务/audit 行和所有远端任务分支始终保留；其他状态永不由该命令清理。

## 重启、排障与恢复

同一数据库一次只允许一个正常服务或清理进程持有 runtime lock。退出使用 `SIGINT`/`SIGTERM`；服务停止接收消息后排空已接收消息，并在超时后返回错误。

启动恢复不会盲目重放外部操作：

- `queued` 保持排队；已经持久化经验证 `TaskCommit` 的 `checking` 可继续精确推送；`pushed` 可进入合并审批；`merged` 可进入部署审批。
- 中断的 `running` 或未形成安全 checkpoint 的 `checking` 变为 `failed`。
- 中断的 `merging` 变为 `failed`，必须人工检查远端 RC 后再决定处理方式。
- 中断的 `deploying` 变为 `deploy_failed`，必须人工检查 RC 环境后由管理员新批准；不会自动重放部署。
- `draft` 不会自动重新调用 Codex；只有 OneBot 重放原始创建消息时才能按同一 message ID 恢复规划。否则保留记录供人工审计。

常见排障顺序：

1. 运行 `状态 #<任务编号>`，记录状态、任务提交和 RC 提交。
2. 运行 `日志 #<任务编号>` 查看脱敏摘要，再在本机检查该任务 JSONL 日志。
3. 检查 NapCat WebSocket 是否仍只监听回环地址、token 是否一致、机器人 `self_id` 和群 ID 是否正确。
4. 检查数据库父目录、日志目录和 worktree 根的 owner/mode；程序不会擅自修复已有目录权限。
5. 检查 worker/ops 两个 clone 的 `origin/main`、`origin/rc` 是否存在，且不是同一 Git common directory。
6. 对 `merge_conflict`、中断的 `merging` 或 `deploying`，先由管理员从远端和 RC 环境确认真实状态，禁止直接重放旧审批。

不要手工删除 SQLite 行来“解锁”任务，也不要对记录中的 worktree 路径运行未经校验的递归删除。需要回滚 RC 时使用公司既有的受控 RC 流程；本程序不会自动回滚。

## 真实 RC 上线门

当前本地版本通过测试证明应用层的命令、持久化、审批和 helper 协议行为，**不能证明以下 OS 和基础设施控制已经存在**。真实 RC 启用前，必须全部人工验收：

1. `qqcodex` 以 `qqcodex-worker` 身份运行；`qqcodex-ops` 以独立的受限 ops 身份运行。两者配置分别只对所属身份可读。
2. 两个身份使用物理独立 clone/common directory。worker 无权读取 ops clone、ops 配置、Git 私钥、部署凭据或部署日志中的秘密。
3. `ops_command` 指向 root-owned、不可被 worker 修改的 typed wrapper 或等价受限服务。wrapper 只允许 helper 的固定 action/flag 语法，不能透传任意程序或任意参数。
4. 禁止 `sudo sh`、`sudo bash -c`、通用 shell wrapper、用户提供 argv、共享私钥，以及让 Codex 可读的 Git/部署 credential helper。
5. `check_runner` 是 root-owned 的受限 runner，只接受 `--` 后的已配置检查 argv，并在无部署凭据、受限环境/网络/资源下运行。当前仓库不提供完整的 OS sandbox，必须在部署环境另行落实和验收。
6. 若 wrapper/IPC 机制确实需要 Unix shared group，只给 bundle/IPC 交接点最小权限；不得通过 shared group 让 worker 读取或修改 ops repo、ops config、凭据和部署文件。
7. ops `deploy_action` 必须是固定、可审计、对同一 RC commit 幂等的 argv。部署目标必须持久化 `QQCODEX_DEPLOY_KEY` 并拒绝已成功处理的重复 key；重复调用不得扩大副作用，目标必须再次校验为 RC，而不是由任务文本选择。
8. 远端 `main`、`rc` 和任务分支启用保护规则：禁止 force push，限制直接写 RC，保留审计，helper 凭据只有所需仓库和 RC 权限。
9. 使用假仓库、假部署目标完成全流程，再在一个非关键 RC 项目验证两任务并发、拒绝未授权群/用户、旧哈希审批失效、合并冲突、部署失败、重启恢复、消息分段和日志脱敏。
10. 验证 SQLite、日志和配置的备份/恢复、磁盘容量、进程守护、告警和人工应急责任人。

只要任一门禁未完成，就保持在本地假 RC 模式，不接入真实 RC 凭据。

## 自动化验证

```bash
go test ./...
go test -race ./internal/app ./internal/tasksvc ./internal/onebot ./internal/ops
go vet ./...
staticcheck ./...
go build ./cmd/qqcodex ./cmd/qqcodex-ops
```

测试覆盖指定群过滤、用户/管理员权限、审批提交不可变与变化拒绝、合并冲突、部署失败与人工重试、重启恢复不重复外部副作用、Unicode 消息分段、日志脱敏，以及过期本地产物的安全清理。自动测试不替代上一节的 OS/RC 人工验收。
