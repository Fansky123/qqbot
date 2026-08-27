# Local QQ Account Switching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow the local QQ Codex Worker to discover a scanned NapCat account, require membership in every configured QQ group, remember an explicitly switched account, and start or stop the local stack through one command.

**Architecture:** `internal/onebot` gains a one-shot authenticated probe and an automatic self-ID mode. A small `qqcodex-probe` executable exposes only login and group information as JSON. A Bash launcher coordinates NapCat and Worker processes, performs transactional account switching, and never edits the user-maintained group or employee lists.

**Tech Stack:** Go 1.26, `github.com/coder/websocket`, OneBot 11 actions, Bash, `jq`, Linux `/proc`, existing Go test helpers.

---

## File Map

- Modify `internal/onebot/protocol.go`: define empty-parameter OneBot actions without weakening typed send-message parameters.
- Modify `internal/onebot/protocol_test.go`: verify login/group action wire format.
- Create `internal/onebot/probe.go`: dial OneBot once, correlate action responses, and decode bounded login/group data.
- Create `internal/onebot/probe_test.go`: cover authentication, interleaved events, invalid data, failures, and cancellation.
- Modify `internal/onebot/client.go`: resolve `self_id: "auto"` before dispatching events on each connection.
- Modify `internal/onebot/client_test.go`: cover automatic self-ID and reconnect identity changes.
- Modify `internal/config/config.go`: accept `auto` or a canonical decimal OneBot self ID.
- Modify `internal/config/config_test.go`: cover valid and invalid self-ID modes.
- Create `cmd/qqcodex-probe/main.go`: load configuration and token, run the probe, compare every configured group, and print bounded JSON.
- Create `cmd/qqcodex-probe/main_test.go`: prove probe mode has no Worker side effects and does not expose tokens.
- Create `scripts/qqcodex-local`: implement `start`, `switch-account`, `status`, and `stop`.
- Create `scripts/qqcodex-local_test.sh`: exercise launcher decisions with temporary directories and fake processes.
- Modify `configs/qqcodex.example.json`: document automatic self-ID.
- Modify local ignored `configs/qqcodex.local.json`: enable automatic self-ID without touching authorization arrays.
- Modify `README.md`: document multiple groups and the local launcher flow.
- Modify local `/home/fanfan007/.local/share/napcat/start-napcat.sh`: add an explicit `--scan` option that suppresses `-q` without forwarding an invalid flag to QQ.
- Create local symlink `/home/fanfan007/.local/bin/qqcodex-local`: expose the committed launcher.

### Task 1: OneBot Login And Group Probe

**Files:**
- Modify: `internal/onebot/protocol.go`
- Modify: `internal/onebot/protocol_test.go`
- Create: `internal/onebot/probe.go`
- Create: `internal/onebot/probe_test.go`

- [ ] **Step 1: Write failing action-construction tests**

Add tests that require empty JSON params for `get_login_info` and `get_group_list`, while preserving the existing typed `send_group_msg` action:

```go
func TestProtocolProbeActions(t *testing.T) {
	for _, tt := range []struct {
		name, action string
		build        func(string) ActionRequest
	}{
		{name: "login", action: "get_login_info", build: LoginInfoAction},
		{name: "groups", action: "get_group_list", build: GroupListAction},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.build("probe-echo")
			if got.Action != tt.action || got.Echo != "probe-echo" {
				t.Fatalf("action = %#v", got)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(`"params":{}`)) {
				t.Fatalf("action JSON = %s, want empty params", raw)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused protocol test and observe failure**

Run: `go test ./internal/onebot -run TestProtocolProbeActions -count=1`

Expected: FAIL because `LoginInfoAction` and `GroupListAction` do not exist.

- [ ] **Step 3: Add the minimal typed action constructors**

Change `ActionParams` to omit empty send-only fields and add constructors:

```go
type ActionParams struct {
	GroupID ID        `json:"group_id,omitempty"`
	Message []Segment `json:"message,omitempty"`
}

func LoginInfoAction(echo string) ActionRequest {
	return ActionRequest{Action: "get_login_info", Params: ActionParams{}, Echo: echo}
}

func GroupListAction(echo string) ActionRequest {
	return ActionRequest{Action: "get_group_list", Params: ActionParams{}, Echo: echo}
}
```

- [ ] **Step 4: Write failing probe tests with a fake OneBot server**

Create `probe_test.go` with a successful test that checks the bearer token, sends an unrelated lifecycle event before each response, returns one numeric and one string group ID, and expects canonical strings:

```go
func TestProbeReturnsLoginAndGroups(t *testing.T) {
	auth := make(chan string, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, request *http.Request) {
		auth <- request.Header.Get("Authorization")
		writeProbeResponse(t, ctx, conn, "get_login_info", map[string]any{
			"user_id": 3289886218, "nickname": "范范",
		})
		writeProbeResponse(t, ctx, conn, "get_group_list", []any{
			map[string]any{"group_id": 2163011680},
			map[string]any{"group_id": "123456789"},
		})
	})
	defer server.Close()

	got, err := Probe(context.Background(), webSocketURL(server.URL), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got.SelfID != "3289886218" || got.Nickname != "范范" {
		t.Fatalf("account = %#v", got)
	}
	if !slices.Equal([]ID{"2163011680", "123456789"}, got.GroupIDs) {
		t.Fatalf("group IDs = %#v", got.GroupIDs)
	}
	if receive(t, auth) != "Bearer secret" {
		t.Fatal("probe did not authenticate")
	}
}
```

Also add table tests for missing/invalid `user_id`, non-string nickname, non-array group data, invalid group IDs, action failure, response over the existing 1 MiB read limit, and caller cancellation.

- [ ] **Step 5: Run probe tests and observe failure**

Run: `go test ./internal/onebot -run Probe -count=1`

Expected: FAIL because `Probe`, `Account`, and response decoding do not exist.

- [ ] **Step 6: Implement the one-shot authenticated probe**

Create these public types and entry point in `probe.go`:

```go
const (
	maxNicknameRunes = 128
	maxProbeGroups    = 10000
)

type Account struct {
	SelfID   ID
	Nickname string
	GroupIDs []ID
}

func Probe(ctx context.Context, endpoint, token string) (Account, error) {
	if ctx == nil {
		return Account{}, errors.New("onebot probe context is required")
	}
	parsed, err := validateEndpoint(endpoint)
	if err != nil || token == "" {
		return Account{}, errors.New("onebot probe configuration is invalid")
	}
	header := make(http.Header, 1)
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return Account{}, fmt.Errorf("connect to OneBot: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxEventBytes)

	loginRaw, err := performAction(ctx, conn, LoginInfoAction)
	if err != nil {
		return Account{}, fmt.Errorf("get OneBot login info: %w", err)
	}
	selfID, nickname, err := decodeLoginInfo(loginRaw)
	if err != nil {
		return Account{}, err
	}
	groupsRaw, err := performAction(ctx, conn, GroupListAction)
	if err != nil {
		return Account{}, fmt.Errorf("get OneBot group list: %w", err)
	}
	groupIDs, err := decodeGroupList(groupsRaw)
	if err != nil {
		return Account{}, err
	}
	return Account{SelfID: selfID, Nickname: nickname, GroupIDs: groupIDs}, nil
}
```

Implement `performAction` so it creates a random echo, writes one request, skips event envelopes with `post_type`, ignores unrelated response echoes, rejects failed/malformed matching responses using the same status/retcode rules as `clientSession.handleResponse`, and returns a copied `data` payload. `decodeLoginInfo` must use `ID.UnmarshalJSON`, require a string nickname no longer than 128 runes, and `decodeGroupList` must reject more than 10,000 rows and deduplicate canonical IDs without reordering the first occurrence.

- [ ] **Step 7: Run protocol and probe tests**

Run: `go test ./internal/onebot -run 'ProtocolProbeActions|Probe' -count=1`

Expected: PASS.

- [ ] **Step 8: Run the complete OneBot package tests**

Run: `go test ./internal/onebot -count=1`

Expected: PASS with all existing send/reconnect tests unchanged.

- [ ] **Step 9: Commit the probe**

```bash
git add internal/onebot/protocol.go internal/onebot/protocol_test.go internal/onebot/probe.go internal/onebot/probe_test.go
git commit -m "feat: probe OneBot account and groups"
```

### Task 2: Automatic Self-ID Per OneBot Connection

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/onebot/client.go`
- Modify: `internal/onebot/client_test.go`
- Modify: `docs/superpowers/specs/2026-08-27-local-qq-account-switching-design.md`

- [ ] **Step 1: Write failing configuration tests**

Add table cases that accept `auto` and a decimal ID, and reject empty, signed, non-decimal, zero-padded, and out-of-range values:

```go
func TestValidateOneBotSelfID(t *testing.T) {
	for _, selfID := range []string{"auto", "3289886218"} {
		cfg := validConfig(t)
		cfg.OneBot.SelfID = selfID
		if err := Validate(cfg); err != nil {
			t.Errorf("self_id %q rejected: %v", selfID, err)
		}
	}
	for _, selfID := range []string{"", "+1", "-1", "01", "abc", "18446744073709551616"} {
		cfg := validConfig(t)
		cfg.OneBot.SelfID = selfID
		if err := Validate(cfg); err == nil {
			t.Errorf("self_id %q accepted", selfID)
		}
	}
}
```

- [ ] **Step 2: Run the configuration test and observe failure**

Run: `go test ./internal/config -run TestValidateOneBotSelfID -count=1`

Expected: FAIL because current validation accepts malformed non-empty IDs.

- [ ] **Step 3: Implement explicit self-ID validation**

Add a local helper that keeps `internal/config` independent of `internal/onebot`:

```go
func validateSelfID(value string) error {
	if value == "auto" {
		return nil
	}
	if value == "" {
		return fmt.Errorf("onebot self ID is required")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return fmt.Errorf("onebot self ID must be auto or an unsigned decimal integer")
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(number, 10) != value {
		return fmt.Errorf("onebot self ID must be canonical")
	}
	return nil
}
```

Call it after checking URL and access-token environment variable. Keep `self_id` required, with `auto` as the explicit dynamic value.

- [ ] **Step 4: Write failing automatic-client tests**

Add a test server that expects `get_login_info` before an event and proves only the discovered QQ mention triggers the handler:

```go
func TestClientAutoSelfIDDiscoversAccountBeforeDispatch(t *testing.T) {
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		respondLoginInfo(t, ctx, conn, "3289886218", "范范")
		_ = wsjson.Write(ctx, conn, groupEvent("1", "2", "3", "3289886218", "hello", true))
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	messages := make(chan GroupMessage, 1)
	go client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
		messages <- message
		return nil
	})
	if got := receive(t, messages); got.SelfID != "3289886218" || !got.Mentioned {
		t.Fatalf("message = %#v", got)
	}
}
```

Add reconnect coverage in which the first connection identifies account A, closes, and the second identifies account B; verify an event for A is ignored on the second connection and an event for B is dispatched. Add malformed/failing login response coverage that proves no event is dispatched and the reconnect backoff is used.

- [ ] **Step 5: Run automatic-client tests and observe failure**

Run: `go test ./internal/onebot -run 'AutoSelfID|ReconnectsWithNewSelfID' -count=1`

Expected: FAIL because `Client.validate` rejects `auto`.

- [ ] **Step 6: Resolve automatic self-ID before starting the session reader**

Represent automatic mode as an empty resolved ID inside `clientConfig`:

```go
func resolveConfiguredSelfID(value string) (string, error) {
	if value == "auto" {
		return "", nil
	}
	return normalizeID(value)
}
```

In `serve`, after dialing and before publishing `c.active`, run only the login action for automatic mode:

```go
selfID := cfg.selfID
if selfID == "" {
	loginRaw, err := performAction(ctx, conn, LoginInfoAction)
	if err != nil {
		_ = conn.CloseNow()
		return time.Time{}
	}
	discovered, _, err := decodeLoginInfo(loginRaw)
	if err != nil {
		_ = conn.CloseNow()
		return time.Time{}
	}
	selfID = string(discovered)
}
```

Pass `selfID` to `DecodeGroupMessage`. Do not mutate the public `Client.SelfID`, so concurrent status reads and reconnects cannot observe partial state. Numeric fixed-ID mode remains byte-for-byte compatible and does not issue a new action.

- [ ] **Step 7: Correct the design wording for legacy fixed-ID compatibility**

Change “每次 WebSocket 连接成功后” to “自动模式每次 WebSocket 连接成功后” and explicitly state that fixed numeric mode keeps its existing event-based check without adding a login action. This matches the selected backward-compatibility requirement and prevents existing OneBot servers from receiving an unexpected action.

- [ ] **Step 8: Run focused and complete tests**

Run: `go test ./internal/config ./internal/onebot -count=1`

Expected: PASS.

- [ ] **Step 9: Commit automatic self-ID**

```bash
git add internal/config/config.go internal/config/config_test.go internal/onebot/client.go internal/onebot/client_test.go docs/superpowers/specs/2026-08-27-local-qq-account-switching-design.md
git commit -m "feat: discover OneBot self ID automatically"
```

### Task 3: Read-Only Probe CLI With Multi-Group Comparison

**Files:**
- Create: `cmd/qqcodex-probe/main.go`
- Create: `cmd/qqcodex-probe/main_test.go`

- [ ] **Step 1: Write failing CLI tests**

Define an injected probe function and test deterministic JSON, missing-group comparison, token loading, and error redaction:

```go
func TestRunPrintsAccountAndMissingConfiguredGroups(t *testing.T) {
	path := writeProbeConfig(t, []string{"2163011680", "987654321"})
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"-config", path}, func(name string) string {
		if name == "NAPCAT_ACCESS_TOKEN" {
			return "onebot-secret"
		}
		return ""
	}, &stdout, func(context.Context, string, string) (onebot.Account, error) {
		return onebot.Account{
			SelfID: "3289886218", Nickname: "范范", GroupIDs: []onebot.ID{"2163011680"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal([]string{"987654321"}, got.MissingGroupIDs) {
		t.Fatalf("missing groups = %#v", got.MissingGroupIDs)
	}
	if strings.Contains(stdout.String(), "onebot-secret") {
		t.Fatal("probe output contains token")
	}
}
```

Also test missing token, malformed flags, probe failure, duplicate configured groups, and that the output contains no project, repository, employee, admin, Codex, or ops fields.

- [ ] **Step 2: Run CLI tests and observe failure**

Run: `go test ./cmd/qqcodex-probe -count=1`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement the minimal probe command**

Use this output contract:

```go
type probeOutput struct {
	SelfID          string   `json:"self_id"`
	Nickname        string   `json:"nickname"`
	GroupIDs        []string `json:"group_ids"`
	MissingGroupIDs []string `json:"missing_group_ids"`
}

type prober func(context.Context, string, string) (onebot.Account, error)
```

`run` must parse exactly `qqcodex-probe -config <path>`, call `config.Load`, read only `cfg.OneBot.AccessTokenEnv`, reject an empty token, call the injected prober with URL and token, compare `cfg.AllowedGroupIDs` in configured order against the probed set, and encode one JSON object to stdout. Return errors without including token values.

The real `main` uses `signal.NotifyContext`, `os.Getenv`, `os.Stdout`, and `onebot.Probe`, logs only the returned safe error, and exits nonzero on failure.

- [ ] **Step 4: Run CLI tests and build the binary**

Run: `go test ./cmd/qqcodex-probe -count=1 && go build -o bin/qqcodex-probe ./cmd/qqcodex-probe`

Expected: tests PASS and `bin/qqcodex-probe` is executable.

- [ ] **Step 5: Commit the probe command**

```bash
git add cmd/qqcodex-probe/main.go cmd/qqcodex-probe/main_test.go
git commit -m "feat: add local OneBot probe command"
```

### Task 4: Transactional Local Launcher

**Files:**
- Create: `scripts/qqcodex-local`
- Create: `scripts/qqcodex-local_test.sh`

- [ ] **Step 1: Write failing launcher tests around public commands**

The test script must create a private temporary root, fake NapCat/Worker launchers, fake `/proc` inspection through injected functions, and a fake probe binary that returns controlled JSON. Use a small assertion helper that exits immediately:

```bash
assert_eq() {
    local want="$1" got="$2" message="$3"
    if [[ "$got" != "$want" ]]; then
        echo "$message: got '$got', want '$want'" >&2
        exit 1
    fi
}
```

Cover these independent cases by invoking the real script with test-only path overrides:

1. `start` adopts a first account when no cache exists, validates two configured groups, writes mode `0600` cache, and starts Worker.
2. `start` rejects a detected account different from an existing cache and does not invoke Worker.
3. `start` reports every `missing_group_ids` entry and does not invoke Worker.
4. `switch-account` clears WebUI quick login only during the scan, commits the new ID to both state files after success, and starts Worker.
5. failed/timeout switch restores the old WebUI JSON byte-for-byte and preserves the old cache.
6. `stop` terminates only PIDs whose executable matches the expected configured executable.
7. stale or reused PID files are removed without signaling the unrelated process.
8. an ordinary QQ process or unrelated listener on port 3001 causes a safe refusal.
9. `status` reports stopped, online, mismatched, and unmanaged states without displaying the token.

- [ ] **Step 2: Run launcher tests and observe failure**

Run: `bash scripts/qqcodex-local_test.sh`

Expected: FAIL because `scripts/qqcodex-local` does not exist.

- [ ] **Step 3: Implement configuration, private state, and atomic writes**

Start the script with strict mode and overrideable machine paths:

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077

project_root="${QQCODEX_LOCAL_PROJECT_ROOT:-/home/fanfan007/文档/go/qqbot}"
napcat_root="${QQCODEX_LOCAL_NAPCAT_ROOT:-/home/fanfan007/.local/share/napcat}"
worker_root="${QQCODEX_LOCAL_WORKER_ROOT:-/home/fanfan007/.local/share/qqcodex-worker}"
config_path="${QQCODEX_LOCAL_CONFIG:-$project_root/configs/qqcodex.local.json}"
probe_binary="${QQCODEX_LOCAL_PROBE:-$project_root/bin/qqcodex-probe}"
napcat_launcher="${QQCODEX_LOCAL_NAPCAT_LAUNCHER:-$napcat_root/start-napcat.sh}"
worker_launcher="${QQCODEX_LOCAL_WORKER_LAUNCHER:-$worker_root/start-worker.sh}"
token_file="${QQCODEX_LOCAL_TOKEN_FILE:-$worker_root/napcat-access-token}"
webui_config="${QQCODEX_LOCAL_WEBUI_CONFIG:-$napcat_root/napcat/config/webui.json}"
account_file="${QQCODEX_LOCAL_ACCOUNT_FILE:-$napcat_root/bot-uin}"
run_root="${QQCODEX_LOCAL_RUN_ROOT:-$worker_root/run}"
```

Add `require_file`, `read_pid`, `pid_matches`, `atomic_write_account`, and `atomic_set_webui_account`. Every temporary file must be created beside its destination, set to mode `0600`, and renamed only after `jq -e` validates the complete JSON.

- [ ] **Step 4: Implement authenticated probe and multi-group refusal**

Load the token into the environment without putting it in argv:

```bash
probe_current_account() {
    local timeout_seconds="${QQCODEX_LOCAL_LOGIN_TIMEOUT:-300}"
    local deadline=$((SECONDS + timeout_seconds))
    export NAPCAT_ACCESS_TOKEN
    NAPCAT_ACCESS_TOKEN="$(tr -d '\r\n' < "$token_file")"
    while (( SECONDS < deadline )); do
        if probe_json="$($probe_binary -config "$config_path" 2>/dev/null)"; then
            printf '%s\n' "$probe_json"
            unset NAPCAT_ACCESS_TOKEN
            return 0
        fi
        sleep 1
    done
    unset NAPCAT_ACCESS_TOKEN
    echo "Timed out waiting for QQ login." >&2
    return 1
}

require_all_groups() {
    local probe_json="$1"
    if [[ "$(jq '.missing_group_ids | length' <<<"$probe_json")" != 0 ]]; then
        jq -r '.missing_group_ids[] | "Bot account is not in configured group: \(.)"' <<<"$probe_json" >&2
        return 1
    fi
}
```

Never print the complete probe JSON in normal command output. Display only the numeric account and nickname selected through fixed `jq -r` expressions.

- [ ] **Step 5: Implement managed process lifecycle**

`start_napcat` records `$!` only after `/proc/$pid/exe` resolves to the configured QQ executable. `start_worker` records its PID, installs `INT`, `TERM`, `HUP`, and `EXIT` traps, waits in the foreground, and removes the PID file on exit. `stop_managed` reads each PID, checks executable identity, sends `TERM`, waits with a bounded loop, and reports but does not use `KILL` automatically.

Before launch, reject:

- a live PID file for either managed process;
- a stale PID whose executable does not match, after removing only the stale file;
- an existing `qq` process not named by the managed NapCat PID;
- port 3001 owned by a process outside the managed NapCat tree;
- a live `qqcodex` executable not named by the managed Worker PID.

- [ ] **Step 6: Implement `start` and transactional `switch-account`**

`start` launches NapCat normally, probes, requires all groups, then:

```bash
detected_id="$(jq -er '.self_id' <<<"$probe_json")"
saved_id="$(read_saved_account || true)"
if [[ -n "$saved_id" && "$detected_id" != "$saved_id" ]]; then
    echo "Logged-in QQ $detected_id differs from saved bot QQ $saved_id; use switch-account." >&2
    return 1
fi
if [[ -z "$saved_id" ]]; then
    atomic_write_account "$detected_id"
    atomic_set_webui_account "$detected_id"
fi
start_worker
```

`switch-account` snapshots `webui.json` to the private run directory, stops only managed processes, sets `autoLoginAccount` to an empty string, invokes `start-napcat.sh --scan`, and installs a rollback trap. Only after probe and group success does it atomically write the new account to both destinations, remove the snapshot, disable rollback, and start Worker. On any earlier exit, stop the new managed NapCat and atomically restore the exact snapshot.

- [ ] **Step 7: Implement `status`, `stop`, usage, and command dispatch**

Accept exactly one subcommand:

```bash
case "${1:-}" in
    start) start_command ;;
    switch-account) switch_account_command ;;
    status) status_command ;;
    stop) stop_command ;;
    *) echo "usage: qqcodex-local {start|switch-account|status|stop}" >&2; exit 2 ;;
esac
```

`status` may perform a short probe only when the endpoint is reachable; it reports saved account, detected account, NapCat PID, and Worker PID without failing merely because services are stopped. `stop` is idempotent.

- [ ] **Step 8: Run launcher tests and shell checks**

Run: `bash -n scripts/qqcodex-local scripts/qqcodex-local_test.sh && bash scripts/qqcodex-local_test.sh`

Expected: syntax checks and all launcher scenarios PASS.

- [ ] **Step 9: Commit the launcher**

```bash
git add scripts/qqcodex-local scripts/qqcodex-local_test.sh
git commit -m "feat: add transactional local QQ launcher"
```

### Task 5: Configuration, Documentation, And Local Installation

**Files:**
- Modify: `configs/qqcodex.example.json`
- Modify: `README.md`
- Modify ignored local file: `configs/qqcodex.local.json`
- Modify local file: `/home/fanfan007/.local/share/napcat/start-napcat.sh`
- Create local symlink: `/home/fanfan007/.local/bin/qqcodex-local`

- [ ] **Step 1: Update tracked example and README**

Set the example to explicit automatic mode:

```json
"self_id": "auto"
```

Replace the dedicated-account-only setup wording with a risk warning and the four launcher commands. Document that `allowed_group_ids` is an array and every configured group must contain the selected account. Keep the existing RC-only warning unchanged.

- [ ] **Step 2: Update local config without touching authorization**

Use `jq` only to inspect before and after, and `apply_patch` to change exactly:

```json
"self_id": "auto"
```

Verify the following values are byte-for-byte equal before and after through canonical JSON comparison:

```bash
jq -S '{allowed_group_ids,employee_ids,admin_ids,projects}' configs/qqcodex.local.json
```

- [ ] **Step 3: Add explicit scan handling to the local NapCat launcher**

Before its current launch-argument logic, parse only `--scan`:

```bash
scan_login=false
if [[ "${1:-}" == "--scan" ]]; then
    scan_login=true
    shift
fi

launch_args=("$@")
if [[ "$scan_login" == false && $# == 0 && -r "$bot_uin_file" ]]; then
    bot_uin="$(tr -d '\r\n' < "$bot_uin_file")"
    launch_args=(-q "$bot_uin")
fi
```

Run `bash -n` and verify `--scan` is consumed instead of appearing in `/opt/QQ/qq` argv with a temporary fake QQ executable.

- [ ] **Step 4: Build and install local entry points**

Run:

```bash
go build -o bin/qqcodex ./cmd/qqcodex
go build -o bin/qqcodex-probe ./cmd/qqcodex-probe
chmod 700 /home/fanfan007/.local/share/napcat/start-napcat.sh
mkdir -p /home/fanfan007/.local/bin
ln -sfn /home/fanfan007/文档/go/qqbot/scripts/qqcodex-local /home/fanfan007/.local/bin/qqcodex-local
chmod 700 scripts/qqcodex-local
```

Verify the symlink resolves inside the repository and both binaries are executable.

- [ ] **Step 5: Run tracked tests and commit documentation/config example**

Run: `go test ./... && bash scripts/qqcodex-local_test.sh && git diff --check`

Expected: PASS.

```bash
git add README.md configs/qqcodex.example.json
git commit -m "docs: document local QQ account switching"
```

The ignored local configuration, local NapCat launcher, built binaries, and user symlink are deliberately not added to Git.

### Task 6: Automated Verification And Safe Real Login Checkpoint

**Files:**
- No new tracked files expected.

- [ ] **Step 1: Run complete automated verification**

Run:

```bash
go test ./...
go test -race ./internal/onebot ./cmd/qqcodex-probe
go vet ./...
bash -n scripts/qqcodex-local scripts/qqcodex-local_test.sh
bash scripts/qqcodex-local_test.sh
go build ./cmd/qqcodex ./cmd/qqcodex-probe ./cmd/qqcodex-ops
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 2: Audit local endpoint, permissions, and secrets**

Verify:

```bash
ss -ltnp | rg '127\.0\.0\.1:(3001|6099)'
stat -c '%a %n' \
  /home/fanfan007/.local/share/napcat/bot-uin \
  /home/fanfan007/.local/share/napcat/napcat/config/webui.json \
  /home/fanfan007/.local/share/napcat/napcat/config/onebot11.json \
  /home/fanfan007/.local/share/qqcodex-worker/napcat-access-token
```

Expected: listeners use loopback only and state/token/config files are mode `0600`. Search tracked and generated launcher output for the actual token hash and Codex key; expected result is no matches outside their approved protected source files.

- [ ] **Step 3: Stop the legacy unmanaged local processes**

Resolve exact PIDs and executable paths first. Send `TERM` only to the current known Worker and NapCat processes after confirming they belong to this setup. Do not terminate an ordinary QQ process; if one exists, ask the user to close it.

- [ ] **Step 4: Pause for the user's QR scan**

Run: `qqcodex-local switch-account`

Expected: NapCat displays a QR code and the launcher waits. The user scans with the desired QQ account. This is the only unavoidable manual checkpoint.

- [ ] **Step 5: Verify the selected account and multiple groups**

Run in another terminal: `qqcodex-local status`

Expected: saved and detected QQ IDs match, all configured groups are present, NapCat is online, and Worker is running. If any configured group is absent, the Worker must remain stopped and the exact missing group IDs must be shown.

- [ ] **Step 6: Verify QQ consultation and task routing**

For every configured test group, send `@当前机器人 你好` from one authorized employee. Expected: one consultation reply in the same group. Then send one harmless test task such as `@当前机器人 [qqbot] 只分析 README 中的启动说明，不修改文件`; expected: a draft/plan reply that stops at the existing confirmation gate.

From an unconfigured group or unauthorized account, send the same mention; expected: no Codex call and no reply. Confirm the previous QQ account no longer responds.

- [ ] **Step 7: Verify remembered startup**

Run `qqcodex-local stop`, then `qqcodex-local start`.

Expected: NapCat quick-logs the newly selected account without another scan, the probe sees the same ID, all groups pass, and Worker reconnects.

- [ ] **Step 8: Inspect final repository state**

Run: `git status --short --branch && git log --oneline -8`

Expected: `main` is clean; only intentional feature commits follow design commit `7bfa8b6`. Built binaries and machine-local files remain untracked or ignored as intended.

## Plan Self-Review

- Spec coverage: automatic identity, fixed-ID compatibility, multiple groups, explicit account adoption, transactional rollback, loopback/token boundaries, managed PID safety, local installation, and manual QR acceptance each map to a task above.
- Placeholder scan: the plan contains no deferred implementation markers; each error case names its concrete behavior and test.
- Type consistency: `onebot.Account`, `Probe`, `probeOutput`, `missing_group_ids`, `LoginInfoAction`, and `GroupListAction` use the same names from protocol through CLI and launcher.
