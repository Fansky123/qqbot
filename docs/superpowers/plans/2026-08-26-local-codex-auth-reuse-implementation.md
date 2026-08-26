# Local Codex Authentication Reuse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the local QQ Codex Worker start without an API-key prompt by reusing the desktop Codex custom provider and API key while keeping that key out of model-invoked tools.

**Architecture:** The local launcher reads the personal Codex API key once and passes it to `qqcodex` through the existing `CODEX_API_KEY` contract. The Go runner mirrors that value to `OPENAI_API_KEY` only in the top-level Codex CLI environment, while its shell environment policy excludes both key names from task subprocesses. The Worker uses a minimal isolated `CODEX_HOME/config.toml` instead of inheriting personal Codex state.

**Tech Stack:** Go 1.26, Go `testing`, Bash, `jq` 1.7, Codex CLI 0.149.0-alpha.4.1, TOML.

---

## File Map

| Path | Responsibility |
| --- | --- |
| `internal/codex/runner.go` | Build the isolated Codex process environment and task-tool environment policy. |
| `internal/codex/runner_test.go` | Prove the model key reaches Codex but cannot be admitted to task tools, including through `KeepEnv`. |
| `/home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml` | Machine-local minimal model/provider configuration; never committed. |
| `/home/fanfan007/.local/share/qqcodex-worker/start-worker.sh` | Machine-local non-interactive launcher; never committed. |
| `/home/fanfan007/.codex/auth.json` | Existing personal authentication source; contents remain unchanged and mode becomes `0600`. |
| `/home/fanfan007/文档/go/qqbot/bin/qqcodex` | Ignored local Worker binary rebuilt after tests pass. |

## Task 1: Enforce the custom-provider key boundary in the Runner

**Files:**

- Modify: `internal/codex/runner_test.go:302`
- Modify: `internal/codex/runner_test.go:1411`
- Modify: `internal/codex/runner.go:391`
- Modify: `internal/codex/runner.go:446`
- Modify: `internal/codex/runner.go:519`

- [ ] **Step 1: Change the environment test to require a mirrored provider key and reject keep-list exposure**

Replace `TestRunnerSanitizesEnvironment` with:

```go
func TestRunnerSanitizesEnvironment(t *testing.T) {
	runner, req, recordPath := helperRunner(t)
	setEnv(t, "CODEX_API_KEY", "codex-key")
	setEnv(t, "OPENAI_API_KEY", "ambient-key-must-not-win")
	setEnv(t, "PATH", "/bin")
	setEnv(t, "HOME", "/private/home")
	unsetEnv(t, "CODEX_HOME")
	setEnv(t, "LANG", "C.UTF-8")
	setEnv(t, "TMPDIR", "/tmp")
	unsetEnv(t, "TMP")
	unsetEnv(t, "TEMP")
	setEnv(t, "ALLOWED_CUSTOM", "kept")
	setEnv(t, "UNKNOWN_SECRET", "must-not-leak")
	runner.KeepEnv = append(runner.KeepEnv,
		"ALLOWED_CUSTOM", "PATH", "CODEX_API_KEY", "OPENAI_API_KEY", "CODEX_HOME", "ALLOWED_CUSTOM",
	)

	if _, err := runner.Plan(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	record := readHelperRecord(t, recordPath)
	want := []string{
		"CODEX_API_KEY=codex-key", "OPENAI_API_KEY=codex-key", "CODEX_HOME=/private/home/.codex",
		"PATH=/bin", "HOME=/private/home", "LANG=C.UTF-8", "TMPDIR=/tmp",
		"GO_WANT_CODEX_HELPER=1", "QQ_CODEX_HELPER_RECORD=" + recordPath, "ALLOWED_CUSTOM=kept",
	}
	if !reflect.DeepEqual(record.Env, want) {
		t.Fatalf("environment = %#v, want %#v", record.Env, want)
	}
	for _, arg := range record.Args {
		if strings.Contains(arg, "filters.CODEX_API_KEY") ||
			strings.Contains(arg, "filters.OPENAI_API_KEY") ||
			strings.Contains(arg, "filters.CODEX_HOME") {
			t.Fatalf("authentication environment exposed to tool subprocess policy: %q", arg)
		}
	}
	wantPolicy := expectedPolicyArgs(runner.KeepEnv)
	if len(record.Args) < 1+len(wantPolicy) || !slices.Equal(record.Args[1:1+len(wantPolicy)], wantPolicy) {
		t.Fatalf("argv = %#v, want policy prefix %#v", record.Args, wantPolicy)
	}
}
```

Update the filter in `expectedPolicyArgs` so the test helper models the required policy:

```go
		if name == "CODEX_API_KEY" || name == "OPENAI_API_KEY" || name == "CODEX_HOME" {
			continue
		}
```

- [ ] **Step 2: Run the focused test and verify the production code fails the new contract**

Run:

```bash
go test ./internal/codex -run '^TestRunnerSanitizesEnvironment$' -count=1 -v
```

Expected: FAIL because the recorded Codex environment does not contain `OPENAI_API_KEY=codex-key`; before the implementation it may also expose an `OPENAI_API_KEY` tool filter.

- [ ] **Step 3: Mirror the internal key only at the Codex process boundary**

In `sanitizedEnvironment`, replace the initial environment construction with:

```go
	env := []string{
		"CODEX_API_KEY=" + apiKey,
		"OPENAI_API_KEY=" + apiKey,
		"CODEX_HOME=" + codexHome,
	}
```

Do not read an ambient `OPENAI_API_KEY`; `CODEX_API_KEY` remains the single application input and source of truth.

- [ ] **Step 4: Block both key names from the tool environment allowlist**

Replace `toolEnvironmentNames` with:

```go
func toolEnvironmentNames(keep []string) ([]string, error) {
	names := append([]string{"PATH", "HOME", "LANG", "TMPDIR", "TMP", "TEMP"}, keep...)
	seen := make(map[string]struct{}, len(names))
	allowed := make([]string, 0, len(names))
	for _, name := range names {
		if !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if name == "CODEX_API_KEY" || name == "OPENAI_API_KEY" || name == "CODEX_HOME" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		allowed = append(allowed, name)
	}
	return allowed, nil
}
```

Update the comment above `policy := environmentPolicyArgs(toolEnv)` to:

```go
	// The Codex process receives the API key under both supported names, while
	// this CLI policy only allows explicitly named non-auth variables into tools.
```

- [ ] **Step 5: Format and run the focused runner tests**

Run:

```bash
gofmt -w internal/codex/runner.go internal/codex/runner_test.go
go test ./internal/codex -run '^(TestRunnerSanitizesEnvironment|TestRunnerRequiresAPIKeyAndAbsoluteHomes|TestRunnerPassesAmbientCodexHomeOnlyToParentProcess|TestRunnerRejectsEnvironmentAssignment)$' -count=1 -v
```

Expected: PASS. The sanitization test records identical values for `CODEX_API_KEY` and `OPENAI_API_KEY`, and no policy argument contains a filter for either key or `CODEX_HOME`.

- [ ] **Step 6: Run the full test suite**

Run:

```bash
go test ./...
```

Expected: PASS for every package.

- [ ] **Step 7: Commit the reusable Runner change**

```bash
git add internal/codex/runner.go internal/codex/runner_test.go
git commit -m "fix: support isolated custom codex provider auth"
```

Expected: one commit containing only the Runner boundary and its tests.

## Task 2: Configure the isolated local Codex runtime

**Files:**

- Create: `/home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml`
- Modify: `/home/fanfan007/.local/share/qqcodex-worker/start-worker.sh`
- Modify mode only: `/home/fanfan007/.codex/auth.json`

- [ ] **Step 1: Tighten the personal authentication file without changing its contents**

Record its checksum, change only its mode, and confirm the checksum is stable:

```bash
auth_path=/home/fanfan007/.codex/auth.json
auth_hash_before="$(sha256sum "$auth_path" | cut -d' ' -f1)"
chmod 0600 "$auth_path"
test "$(stat -c '%a' "$auth_path")" = 600
test "$(sha256sum "$auth_path" | cut -d' ' -f1)" = "$auth_hash_before"
```

Expected: all commands exit zero; no credential contents are printed.

- [ ] **Step 2: Create the minimal isolated provider configuration**

Create `/home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml` with mode `0600` and exactly this content:

```toml
model_provider = "custom"
model = "gpt-5.6-sol"
model_reasoning_effort = "xhigh"
disable_response_storage = true
personality = "pragmatic"

[model_providers.custom]
name = "OpenAI"
wire_api = "responses"
requires_openai_auth = true
base_url = "https://midcode.cc/v1"
```

Then run:

```bash
chmod 0600 /home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml
test "$(stat -c '%a' /home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml)" = 600
! rg -n 'danger-full-access|trusted|mcp_servers|marketplaces|plugins' /home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml
test ! -e /home/fanfan007/.local/share/qqcodex-worker/home/.codex/auth.json
```

Expected: mode is `600`, the forbidden-setting scan has no matches, and no isolated `auth.json` exists.

- [ ] **Step 3: Replace the interactive launcher with validated auth loading**

Replace `/home/fanfan007/.local/share/qqcodex-worker/start-worker.sh` with:

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077

worker_root="/home/fanfan007/.local/share/qqcodex-worker"
project_root="/home/fanfan007/文档/go/qqbot"
config_path="${1:-$project_root/configs/qqcodex.local.json}"
token_file="$worker_root/napcat-access-token"
auth_file="/home/fanfan007/.codex/auth.json"
codex_binary="/usr/lib/chatgpt/resources/codex"
isolated_home="$worker_root/home"
isolated_codex_home="$isolated_home/.codex"
provider_config="$isolated_codex_home/config.toml"

if [[ ! -x "$project_root/bin/qqcodex" ]]; then
    echo "Worker binary is missing: $project_root/bin/qqcodex" >&2
    exit 1
fi
if [[ ! -r "$config_path" ]]; then
    echo "Worker config is missing: $config_path" >&2
    exit 1
fi
if [[ ! -r "$token_file" ]]; then
    echo "NapCat access token is missing: $token_file" >&2
    exit 1
fi
if [[ ! -r "$auth_file" ]]; then
    echo "Local Codex authentication is missing: $auth_file" >&2
    exit 1
fi
if [[ ! -x "$codex_binary" ]]; then
    echo "Codex CLI is missing: $codex_binary" >&2
    exit 1
fi
if [[ ! -r "$provider_config" ]]; then
    echo "Isolated Codex provider config is missing: $provider_config" >&2
    exit 1
fi
if [[ -e "$isolated_codex_home/auth.json" || -L "$isolated_codex_home/auth.json" ]]; then
    echo "Cached authentication is not allowed in the isolated Codex home." >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required to read local Codex authentication." >&2
    exit 1
fi
if ! HOME="$isolated_home" CODEX_HOME="$isolated_codex_home" \
    "$codex_binary" --strict-config features list >/dev/null; then
    echo "Isolated Codex provider config is invalid." >&2
    exit 1
fi
if ! codex_api_key="$(jq -er '.OPENAI_API_KEY | select(type == "string" and length > 0)' "$auth_file")"; then
    echo "Local Codex authentication is invalid or has no OPENAI_API_KEY." >&2
    exit 1
fi

export HOME="$isolated_home"
export CODEX_HOME="$isolated_codex_home"
export CODEX_API_KEY="$codex_api_key"
unset codex_api_key
export NAPCAT_ACCESS_TOKEN
NAPCAT_ACCESS_TOKEN="$(tr -d '\r\n' < "$token_file")"

exec "$project_root/bin/qqcodex" -config "$config_path"
```

Keep the launcher mode at `0700`:

```bash
chmod 0700 /home/fanfan007/.local/share/qqcodex-worker/start-worker.sh
bash -n /home/fanfan007/.local/share/qqcodex-worker/start-worker.sh
```

Expected: syntax validation exits zero and does not print the key.

- [ ] **Step 4: Validate the launcher inputs without exposing their values**

Run:

```bash
command -v jq >/dev/null
jq -e '.OPENAI_API_KEY | type == "string" and length > 0' /home/fanfan007/.codex/auth.json >/dev/null
for invalid_auth in '{}' '{"OPENAI_API_KEY":""}' '{"OPENAI_API_KEY":1}' 'not-json'; do
    if jq -er '.OPENAI_API_KEY | select(type == "string" and length > 0)' \
        <<<"$invalid_auth" >/dev/null 2>&1; then
        echo 'Invalid authentication fixture was accepted.' >&2
        exit 1
    fi
done
HOME=/home/fanfan007/.local/share/qqcodex-worker/home \
CODEX_HOME=/home/fanfan007/.local/share/qqcodex-worker/home/.codex \
/usr/lib/chatgpt/resources/codex --strict-config features list >/dev/null
test -r /home/fanfan007/.local/share/qqcodex-worker/napcat-access-token
test -r /home/fanfan007/文档/go/qqbot/configs/qqcodex.local.json
stat -c '%a %n' \
  /home/fanfan007/.codex/auth.json \
  /home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml \
  /home/fanfan007/.local/share/qqcodex-worker/start-worker.sh
```

Expected modes: `600`, `600`, and `700`. Every invalid authentication fixture is rejected, strict config parsing exits zero, and no validation command emits the key.

This task changes only ignored machine-local files and file metadata, so it has no Git commit.

## Task 3: Rebuild and verify the complete local path

**Files:**

- Rebuild: `/home/fanfan007/文档/go/qqbot/bin/qqcodex`
- Runtime log: `/home/fanfan007/.local/share/qqcodex-worker/logs/worker-console.log`
- Runtime PID: `/home/fanfan007/.local/share/qqcodex-worker/worker.pid`

- [ ] **Step 1: Re-run repository verification from a clean checkout state**

Run:

```bash
git diff --check
go test ./...
git status --short --branch
```

Expected: `git diff --check` and all tests pass; the branch is clean after the Task 1 commit.

- [ ] **Step 2: Rebuild the ignored local Worker binary**

Run:

```bash
go build -o /home/fanfan007/文档/go/qqbot/bin/qqcodex ./cmd/qqcodex
test -x /home/fanfan007/文档/go/qqbot/bin/qqcodex
```

Expected: build exits zero and the binary is executable.

- [ ] **Step 3: Make one real isolated Codex request through the configured custom provider**

Run without printing the key:

```bash
worker_home=/home/fanfan007/.local/share/qqcodex-worker/home
probe_output="$(mktemp /tmp/qqcodex-provider-probe.XXXXXX)"
codex_api_key="$(jq -er '.OPENAI_API_KEY | select(type == "string" and length > 0)' /home/fanfan007/.codex/auth.json)"
HOME="$worker_home" \
CODEX_HOME="$worker_home/.codex" \
CODEX_API_KEY="$codex_api_key" \
OPENAI_API_KEY="$codex_api_key" \
/usr/lib/chatgpt/resources/codex exec \
  -C /home/fanfan007/文档/go/qqbot \
  --sandbox read-only \
  --ephemeral \
  -o "$probe_output" \
  -- 'Reply with exactly: OK'
unset codex_api_key
test "$(tr -d '\r\n' < "$probe_output")" = OK
rm -f "$probe_output"
```

Expected: Codex exits zero and the final output is exactly `OK`. Authentication or endpoint errors stop execution before Worker startup.

- [ ] **Step 4: Start the Worker non-interactively**

First confirm no Worker is already running:

```bash
if pgrep -af '/bin/qqcodex.*-config.*/configs/qqcodex.local.json'; then
    echo 'A Worker is already running; inspect it before starting another instance.' >&2
    exit 1
fi
```

Then start it with its protected console log:

```bash
worker_root=/home/fanfan007/.local/share/qqcodex-worker
nohup "$worker_root/start-worker.sh" \
  >>"$worker_root/logs/worker-console.log" 2>&1 </dev/null &
worker_pid=$!
printf '%s\n' "$worker_pid" >"$worker_root/worker.pid"
chmod 0600 "$worker_root/worker.pid" "$worker_root/logs/worker-console.log"
```

Expected: the launcher does not ask for input and returns a live Worker PID.

- [ ] **Step 5: Confirm Worker and NapCat stay connected**

Run:

```bash
worker_root=/home/fanfan007/.local/share/qqcodex-worker
worker_pid="$(cat "$worker_root/worker.pid")"
kill -0 "$worker_pid"
sleep 5
kill -0 "$worker_pid"
tail -n 80 "$worker_root/logs/worker-console.log"
```

Expected: both `kill -0` checks succeed. The log shows normal Worker/OneBot startup and contains no authentication error, raw API key, or repeated reconnect failure.

- [ ] **Step 6: Perform the final secret-boundary audit**

Run:

```bash
worker_root=/home/fanfan007/.local/share/qqcodex-worker
test ! -e "$worker_root/home/.codex/auth.json"
! rg -n 'OPENAI_API_KEY|CODEX_API_KEY|Authorization: Bearer' \
  "$worker_root/logs/worker-console.log" \
  "$worker_root/home/.codex/config.toml"
git status --short --branch
```

Expected: no isolated auth cache, no credential-bearing log/config lines, and a clean Git branch. The provider configuration is allowed to contain the non-secret `base_url` only.
