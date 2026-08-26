# Local Codex Authentication Reuse Design

Date: 2026-08-26

## 1. Goal

Allow the local QQ Codex Worker to reuse the model provider, `base_url`, and API key already configured for the desktop Codex installation. Worker startup must be non-interactive while preserving the existing separation between personal Codex state, the Worker control plane, and task tool processes.

This is an incremental design for the local development environment. It does not change the QQ authorization model, Git/RC approval flow, or the requirement that only the RC environment may eventually be connected.

## 2. Selected Approach

The Worker keeps its isolated runtime directories:

```text
HOME=/home/fanfan007/.local/share/qqcodex-worker/home
CODEX_HOME=/home/fanfan007/.local/share/qqcodex-worker/home/.codex
```

The isolated `CODEX_HOME/config.toml` contains only the model and custom provider settings required by this Worker:

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

The Worker does not copy or link the complete personal Codex configuration. In particular, it does not inherit personal trusted-project entries, plugins, marketplaces, MCP servers, desktop settings, or `sandbox_mode = "danger-full-access"`.

At startup, the launcher reads the `OPENAI_API_KEY` value from the personal `/home/fanfan007/.codex/auth.json`. The key remains in memory and is exported to the Worker process through the existing `CODEX_API_KEY` application contract. There is no interactive key prompt and no second credential file in the isolated `CODEX_HOME`.

## 3. Authentication Flow

```text
/home/fanfan007/.codex/auth.json
        |
        | launcher reads OPENAI_API_KEY with jq
        v
qqcodex process: CODEX_API_KEY
        |
        | runner creates top-level Codex CLI environment
        v
codex process: CODEX_API_KEY + OPENAI_API_KEY
        |
        | Codex shell_environment_policy allowlist
        v
task tools: neither CODEX_API_KEY nor OPENAI_API_KEY
```

The runner supplies both names to the top-level Codex CLI because the application uses `CODEX_API_KEY` as its internal secret name while the configured custom provider uses OpenAI-compatible authentication. The two variables contain the same value.

The existing rejection of `auth.json` inside either isolated `HOME` or `CODEX_HOME` remains in place. This prevents accidental credential persistence in the Worker runtime.

## 4. Secret Boundaries

The launcher must:

- require `jq` and the personal authentication file;
- fail if the JSON is malformed or `OPENAI_API_KEY` is absent or empty;
- never print, interpolate into command arguments, or write the key to disk;
- read the NapCat token through the existing protected local configuration;
- start the Worker with isolated `HOME` and `CODEX_HOME`.

The runner must:

- pass both API-key environment variable names only to the top-level Codex CLI;
- exclude `CODEX_API_KEY`, `OPENAI_API_KEY`, and `CODEX_HOME` from the environment that Codex may expose to task tools;
- ignore attempts to add either API key through the configurable environment keep-list;
- retain the existing minimal tool environment for `PATH`, `HOME`, locale, and temporary directories.

The personal authentication file currently has group-readable permissions. Local setup must tighten it to mode `0600` before the launcher begins using it.

Task logs already redact configured secrets. The loaded model key remains part of that redaction set, but containment must not depend on redaction alone.

## 5. Failure Behavior

Worker startup stops before connecting to OneBot when any of these conditions is true:

- the personal authentication file is missing or unreadable;
- `jq` is unavailable;
- the authentication JSON is invalid;
- `OPENAI_API_KEY` is missing or empty;
- the isolated provider configuration is missing or invalid;
- an `auth.json` exists in an isolated Worker authentication root.

Errors identify the invalid file or missing setting without showing credential contents. NapCat continues to run independently if Worker authentication fails.

## 6. Files and Changes

- `/home/fanfan007/.local/share/qqcodex-worker/home/.codex/config.toml`: create the minimal isolated provider configuration with mode `0600`.
- `/home/fanfan007/.local/share/qqcodex-worker/start-worker.sh`: replace the interactive prompt with validated local-auth loading.
- `/home/fanfan007/.codex/auth.json`: change file mode from `0664` to `0600`; do not modify its contents.
- `internal/codex/runner.go`: provide the custom provider with `OPENAI_API_KEY` at the Codex process boundary and block it from task tools.
- `internal/codex/runner_test.go`: test the process/tool environment separation and keep-list rejection.

The machine-specific launcher and isolated config remain outside Git. The reusable runner security rule and its tests are committed in this repository.

## 7. Verification

Implementation follows test-driven development:

1. Add runner tests that fail because `OPENAI_API_KEY` is not yet supplied to the Codex process or filtered from all tool-environment paths.
2. Make the smallest runner change that passes those tests.
3. Run the focused runner tests and `go test ./...`.
4. Validate launcher failure behavior with a controlled missing or invalid auth input without printing a real key.
5. Verify file modes and confirm the isolated configuration contains no personal trust, plugin, MCP, or dangerous sandbox entries.
6. Run one minimal real `codex exec` request in the isolated environment against the configured custom provider.
7. Start the formal Worker and confirm it remains connected to the local NapCat OneBot endpoint.

The real request is the final proof that the custom `base_url` and reused key work together; unit tests alone cannot establish that external authentication succeeds.

## 8. Out of Scope

- Copying or linking the whole personal `~/.codex` directory.
- Storing an API key in the repository, Worker JSON configuration, shell script, or isolated `auth.json`.
- Giving task Shell/Git/test processes access to the model API key.
- Changing the model provider or key automatically when the personal config later changes.
- Server deployment, service supervision, credential vault integration, production access, or real RC credentials.

When the Worker moves to a server, local personal-auth reuse must be replaced with a server-managed secret source while preserving the runner boundary defined here.
