#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
LAUNCHER=$SCRIPT_DIR/qqcodex-local
SUITE_ROOT=$(mktemp -d)
OWNED_PIDS=()
declare -A OWNED_STARTS=()
LAST_OUT=
LAST_ERR=
LAST_STATUS=0
TEST_COUNT=0

cleanup_suite() {
  local attempt current_start pid still_running
  for pid in "${OWNED_PIDS[@]}"; do
    current_start=$(process_start_time "$pid" 2>/dev/null || true)
    if [[ -n $current_start && $current_start == "${OWNED_STARTS[$pid]-}" ]]; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  for attempt in {1..50}; do
    still_running=0
    for pid in "${OWNED_PIDS[@]}"; do
      current_start=$(process_start_time "$pid" 2>/dev/null || true)
      if [[ -n $current_start && $current_start == "${OWNED_STARTS[$pid]-}" ]]; then
        still_running=1
      fi
    done
    (( still_running == 0 )) && break
    sleep 0.02
  done
  for pid in "${OWNED_PIDS[@]}"; do
    current_start=$(process_start_time "$pid" 2>/dev/null || true)
    if [[ -n $current_start && $current_start == "${OWNED_STARTS[$pid]-}" ]]; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
    wait "$pid" 2>/dev/null || true
  done
  rm -rf -- "$SUITE_ROOT"
}
trap cleanup_suite EXIT INT TERM HUP

fail() {
  printf 'not ok - %s\n' "$*" >&2
  exit 1
}

pass() {
  TEST_COUNT=$((TEST_COUNT + 1))
  printf 'ok %d - %s\n' "$TEST_COUNT" "$1"
}

assert_eq() {
  local expected=$1 actual=$2 message=$3
  [[ $actual == "$expected" ]] || fail "$message: expected [$expected], got [$actual]"
}

assert_contains() {
  local file=$1 text=$2 message=$3
  grep -Fq -- "$text" "$file" || fail "$message: missing [$text] in $file"
}

assert_not_contains() {
  local file=$1 text=$2 message=$3
  if grep -Fq -- "$text" "$file"; then
    fail "$message: found forbidden [$text] in $file"
  fi
}

assert_file_absent() {
  [[ ! -e $1 ]] || fail "$2: unexpected file $1"
}

wait_for_file() {
  local file=$1 attempt
  for attempt in {1..150}; do
    [[ -s $file ]] && return 0
    sleep 0.02
  done
  if [[ -n ${WRAPPER_PID-} ]]; then
    if kill -0 "$WRAPPER_PID" 2>/dev/null; then
      printf 'wrapper %s is still running\n' "$WRAPPER_PID" >&2
    else
      printf 'wrapper %s exited before readiness\n' "$WRAPPER_PID" >&2
    fi
  fi
  if [[ -n ${LAST_ERR-} && -f $LAST_ERR ]]; then
    sed -n '1,20p' "$LAST_ERR" >&2
  fi
  fail "timed out waiting for $file"
}

wait_for_dead() {
  local pid=$1 attempt
  for attempt in {1..150}; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.02
  done
  fail "process $pid was not stopped"
}

track_pid() {
  OWNED_PIDS+=("$1")
  OWNED_STARTS[$1]=$(process_start_time "$1" 2>/dev/null || true)
}

process_start_time() {
  local stat_line fields
  stat_line=$(<"/proc/$1/stat") || return 1
  fields=${stat_line##*) }
  # shellcheck disable=SC2086
  set -- $fields
  [[ ${20-} =~ ^[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "${20}"
}

write_managed_identity() {
  local pid=$1 pid_file=$2 start_file=$3
  printf '%s\n' "$pid" >"$pid_file"
  process_start_time "$pid" >"$start_file"
}

setup_fixture() {
  local name=$1
  unset QQCODEX_LOCAL_JQ
  unset QQCODEX_TEST_JQ_FAIL_ACCOUNT QQCODEX_TEST_NAPCAT_REMOVE_RUN \
    QQCODEX_TEST_PGREP_ERROR QQCODEX_TEST_PORT_ERROR \
    QQCODEX_TEST_WORKER_IGNORE_TERM QQCODEX_TEST_WORKER_REMOVE_RUN
  FIXTURE=$SUITE_ROOT/$name
  PROJECT_ROOT=$FIXTURE/project
  NAPCAT_ROOT=$FIXTURE/napcat
  WORKER_ROOT=$FIXTURE/worker
  mkdir -p "$PROJECT_ROOT/configs" "$PROJECT_ROOT/bin" \
    "$NAPCAT_ROOT/napcat/config" "$WORKER_ROOT" "$FIXTURE/state/run"

  CONFIG=$PROJECT_ROOT/configs/qqcodex.local.json
  PROBE=$PROJECT_ROOT/bin/qqcodex-probe
  QQ_EXEC=$FIXTURE/napcat-qq
  WORKER_EXEC=$FIXTURE/qqcodex-worker
  NAPCAT_LAUNCHER=$NAPCAT_ROOT/start-napcat.sh
  WORKER_LAUNCHER=$WORKER_ROOT/start-worker.sh
  TOKEN_FILE=$FIXTURE/state/napcat-access-token
  WEBUI=$FIXTURE/state/webui.json
  ACCOUNT=$FIXTURE/state/bot-uin
  RUN_DIR=$FIXTURE/state/run
  NAPCAT_START=$RUN_DIR/napcat.start
  WORKER_START=$RUN_DIR/worker.start
  QQ_DISCOVERY=$FIXTURE/qq.pids
  WORKER_DISCOVERY=$FIXTURE/worker.pids
  PORT_STATE=$FIXTURE/port.busy
  PROBE_MODE_FILE=$FIXTURE/probe.mode
  PROBE_COUNT_FILE=$FIXTURE/probe.count
  PROBE_ARGS=$FIXTURE/probe.args
  NAPCAT_ARGS=$FIXTURE/napcat.args
  NAPCAT_WEBUI_ACCOUNT=$FIXTURE/napcat.webui-account
  WORKER_ARGS=$FIXTURE/worker.args
  WORKER_READY=$FIXTURE/worker.ready

  cp /bin/sleep "$QQ_EXEC"
  cp /bin/sleep "$WORKER_EXEC"
  chmod 0700 "$QQ_EXEC" "$WORKER_EXEC"
  printf '%s\n' 'ultra-secret-token' >"$TOKEN_FILE"
  : >"$QQ_DISCOVERY"
  : >"$WORKER_DISCOVERY"
  printf '0\n' >"$PORT_STATE"
  printf 'success\n' >"$PROBE_MODE_FILE"

  cat >"$CONFIG" <<'EOF'
{
  "onebot": {"url":"ws://127.0.0.1:3001","access_token_env":"NAPCAT_ACCESS_TOKEN","self_id":"1"},
  "allowed_group_ids": ["70001", "70002"],
  "employee_ids": ["80001", "80002"],
  "admin_ids": ["80002"],
  "projects": [{"id":"kept-project"}]
}
EOF
  cat >"$WEBUI" <<'EOF'
{"autoLoginAccount":"old","theme":"dark","nested":{"keep":true},"array":[1,2,3]}
EOF

  cat >"$PROBE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '<%s>\n' "$@" >>"$QQCODEX_TEST_PROBE_ARGS"
[[ ${NAPCAT_ACCESS_TOKEN-} == "$QQCODEX_TEST_TOKEN" ]] || exit 91
count=0
if [[ -r $QQCODEX_TEST_PROBE_COUNT ]]; then
  read -r count <"$QQCODEX_TEST_PROBE_COUNT" || true
fi
count=$((count + 1))
printf '%s\n' "$count" >"$QQCODEX_TEST_PROBE_COUNT"
mode=$(<"$QQCODEX_TEST_PROBE_MODE")
  case "$mode" in
  success) ;;
  error) exit 1 ;;
  invalid) printf '%s\n' '{"self_id":123,"nickname":"bad","group_ids":[],"missing_group_ids":[]}' ; exit 0 ;;
  hang) exec /bin/sleep 60 ;;
  retry-once) (( count > 1 )) || exit 1 ;;
  *) exit 92 ;;
esac
printf '%s\n' "$QQCODEX_TEST_PROBE_JSON"
EOF

  cat >"$NAPCAT_LAUNCHER" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '<%s>\n' "$@" >"$QQCODEX_TEST_NAPCAT_ARGS"
"$QQCODEX_TEST_REAL_JQ" -r '.autoLoginAccount' "$QQCODEX_TEST_WEBUI" \
  >"$QQCODEX_TEST_NAPCAT_WEBUI_ACCOUNT"
printf '%s\n' "$$" >"$QQCODEX_TEST_NAPCAT_SPAWNED"
if [[ -v NAPCAT_ACCESS_TOKEN ]]; then
  printf '%s\n' "$NAPCAT_ACCESS_TOKEN" >"$QQCODEX_TEST_NAPCAT_ENV_LEAK"
fi
if [[ ${QQCODEX_TEST_NAPCAT_REMOVE_RUN-0} == 1 ]]; then
  rmdir -- "$QQCODEX_LOCAL_RUN_ROOT"
fi
exec "$QQCODEX_TEST_QQ_EXEC" 60
EOF

  cat >"$WORKER_LAUNCHER" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '<%s>\n' "$@" >"$QQCODEX_TEST_WORKER_ARGS"
printf '%s\n' "$$" >"$QQCODEX_TEST_WORKER_SPAWNED"
if [[ -v NAPCAT_ACCESS_TOKEN ]]; then
  printf '%s\n' "$NAPCAT_ACCESS_TOKEN" >"$QQCODEX_TEST_WORKER_ENV_LEAK"
fi
if [[ ${QQCODEX_TEST_WORKER_REMOVE_RUN-0} == 1 ]]; then
  rm -f -- "$QQCODEX_LOCAL_RUN_ROOT/napcat.pid"
  rmdir -- "$QQCODEX_LOCAL_RUN_ROOT"
fi
if [[ ${QQCODEX_TEST_WORKER_IGNORE_TERM-0} == 1 ]]; then
  exec "$QQCODEX_TEST_WORKER_EXEC" -c \
    'trap "" TERM; trap "exit 0" HUP; printf x >"$QQCODEX_TEST_WORKER_READY"; while :; do /bin/sleep 1; done'
fi
exec "$QQCODEX_TEST_WORKER_EXEC" 60
EOF

  cat >"$FIXTURE/fake-pgrep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
name=${!#}
[[ ${QQCODEX_TEST_PGREP_ERROR-0} == 0 ]] || exit 2
case "$name" in
  napcat-qq) file=$QQCODEX_TEST_QQ_DISCOVERY ;;
  qqcodex-worker) file=$QQCODEX_TEST_WORKER_DISCOVERY ;;
  *) exit 1 ;;
esac
[[ -s $file ]] || exit 1
cat "$file"
EOF

  cat >"$FIXTURE/fake-port-check" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${QQCODEX_TEST_PORT_ERROR-0} == 0 ]] || exit 2
[[ $(<"$QQCODEX_TEST_PORT_STATE") == 1 ]]
EOF
  cat >"$FIXTURE/fake-jq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1-} == --arg && ${2-} == account && ${3-} == "${QQCODEX_TEST_JQ_FAIL_ACCOUNT-}" ]]; then
  exit 1
fi
exec "$QQCODEX_TEST_REAL_JQ" "$@"
EOF
  chmod 0700 "$PROBE" "$NAPCAT_LAUNCHER" "$WORKER_LAUNCHER" \
    "$FIXTURE/fake-pgrep" "$FIXTURE/fake-port-check" "$FIXTURE/fake-jq"

  export QQCODEX_LOCAL_PROJECT_ROOT=$PROJECT_ROOT
  export QQCODEX_LOCAL_NAPCAT_ROOT=$NAPCAT_ROOT
  export QQCODEX_LOCAL_WORKER_ROOT=$WORKER_ROOT
  export QQCODEX_LOCAL_CONFIG=$CONFIG
  export QQCODEX_LOCAL_PROBE=$PROBE
  export QQCODEX_LOCAL_NAPCAT_LAUNCHER=$NAPCAT_LAUNCHER
  export QQCODEX_LOCAL_WORKER_LAUNCHER=$WORKER_LAUNCHER
  export QQCODEX_LOCAL_TOKEN_FILE=$TOKEN_FILE
  export QQCODEX_LOCAL_WEBUI_CONFIG=$WEBUI
  export QQCODEX_LOCAL_ACCOUNT_FILE=$ACCOUNT
  export QQCODEX_LOCAL_RUN_ROOT=$RUN_DIR
  export QQCODEX_LOCAL_QQ_EXECUTABLE=$QQ_EXEC
  export QQCODEX_LOCAL_WORKER_EXECUTABLE=$WORKER_EXEC
  export QQCODEX_LOCAL_ONEBOT_PORT=3001
  export QQCODEX_LOCAL_LOGIN_TIMEOUT=0
  export QQCODEX_LOCAL_STATUS_TIMEOUT=0
  export QQCODEX_LOCAL_TIMEOUT
  QQCODEX_LOCAL_TIMEOUT=$(command -v timeout)
  export QQCODEX_LOCAL_PROCESS_TIMEOUT=3
  export QQCODEX_LOCAL_STOP_TIMEOUT=1
  export QQCODEX_LOCAL_PROBE_INTERVAL=0.02
  export QQCODEX_LOCAL_PROCESS_INTERVAL=0.02
  export QQCODEX_LOCAL_PGREP=$FIXTURE/fake-pgrep
  export QQCODEX_LOCAL_PORT_CHECK=$FIXTURE/fake-port-check

  export QQCODEX_TEST_TOKEN=ultra-secret-token
  export QQCODEX_TEST_PROBE_JSON='{"self_id":"123456","nickname":"Test Bot","group_ids":["70001","70002"],"missing_group_ids":[]}'
  export QQCODEX_TEST_PROBE_MODE=$PROBE_MODE_FILE
  export QQCODEX_TEST_PROBE_COUNT=$PROBE_COUNT_FILE
  export QQCODEX_TEST_PROBE_ARGS=$PROBE_ARGS
  export QQCODEX_TEST_NAPCAT_ARGS=$NAPCAT_ARGS
  export QQCODEX_TEST_NAPCAT_WEBUI_ACCOUNT=$NAPCAT_WEBUI_ACCOUNT
  export QQCODEX_TEST_WORKER_ARGS=$WORKER_ARGS
  export QQCODEX_TEST_WORKER_READY=$WORKER_READY
  export QQCODEX_TEST_NAPCAT_SPAWNED=$FIXTURE/napcat.spawned
  export QQCODEX_TEST_WORKER_SPAWNED=$FIXTURE/worker.spawned
  export QQCODEX_TEST_NAPCAT_ENV_LEAK=$FIXTURE/napcat.env-leak
  export QQCODEX_TEST_WORKER_ENV_LEAK=$FIXTURE/worker.env-leak
  export QQCODEX_TEST_QQ_EXEC=$QQ_EXEC
  export QQCODEX_TEST_WORKER_EXEC=$WORKER_EXEC
  export QQCODEX_TEST_QQ_DISCOVERY=$QQ_DISCOVERY
  export QQCODEX_TEST_WORKER_DISCOVERY=$WORKER_DISCOVERY
  export QQCODEX_TEST_PORT_STATE=$PORT_STATE
  export QQCODEX_TEST_WEBUI=$WEBUI
  export QQCODEX_TEST_REAL_JQ
  QQCODEX_TEST_REAL_JQ=$(command -v jq)
  export NAPCAT_ACCESS_TOKEN=ambient-should-be-cleared
}

capture() {
  local name=$1
  shift
  LAST_OUT=$FIXTURE/$name.out
  LAST_ERR=$FIXTURE/$name.err
  set +e
  "$LAUNCHER" "$@" >"$LAST_OUT" 2>"$LAST_ERR"
  LAST_STATUS=$?
  set -e
}

start_background() {
  local name=$1 command=$2
  LAST_OUT=$FIXTURE/$name.out
  LAST_ERR=$FIXTURE/$name.err
  "$LAUNCHER" "$command" >"$LAST_OUT" 2>"$LAST_ERR" &
  WRAPPER_PID=$!
  track_pid "$WRAPPER_PID"
}

stop_wrapper() {
  local pid=$1 status
  kill -TERM "$pid"
  set +e
  wait "$pid"
  status=$?
  set -e
  [[ $status == 143 || $status == 129 || $status == 130 || $status == 0 ]] || \
    fail "wrapper exited unexpectedly with $status"
}

assert_private_account() {
  local expected=$1
  assert_eq "$expected" "$(<"$ACCOUNT")" 'saved account content'
  assert_eq 600 "$(stat -c '%a' "$ACCOUNT")" 'saved account mode'
}

assert_no_secret_output() {
  local file
  for file in "$FIXTURE"/*.out "$FIXTURE"/*.err "$FIXTURE"/*.args \
    "$FIXTURE"/*.env-leak; do
    [[ -e $file ]] || continue
    assert_not_contains "$file" 'ultra-secret-token' 'token leaked'
    assert_not_contains "$file" 'ambient-should-be-cleared' 'ambient token leaked'
    assert_not_contains "$file" '"group_ids"' 'full probe JSON leaked'
  done
}

test_start_first_account() {
  setup_fixture start-first
  cp "$WEBUI" "$FIXTURE/webui.before"
  chmod 0777 "$RUN_DIR"
  printf 'retry-once\n' >"$PROBE_MODE_FILE"
  export QQCODEX_LOCAL_LOGIN_TIMEOUT=2
  start_background first start
  wait_for_file "$RUN_DIR/worker.pid"
  napcat_pid=$(<"$RUN_DIR/napcat.pid")
  worker_pid=$(<"$RUN_DIR/worker.pid")
  track_pid "$napcat_pid"
  track_pid "$worker_pid"

  assert_private_account 123456
  assert_eq 123456 "$(jq -r '.autoLoginAccount' "$WEBUI")" 'WebUI account'
  assert_eq "$(jq -Sc 'del(.autoLoginAccount)' "$FIXTURE/webui.before")" \
    "$(jq -Sc 'del(.autoLoginAccount)' "$WEBUI")" 'WebUI fields other than account'
  assert_eq 600 "$(stat -c '%a' "$WEBUI")" 'WebUI mode'
  assert_eq 700 "$(stat -c '%a' "$RUN_DIR")" 'run directory mode'
  assert_eq 600 "$(stat -c '%a' "$RUN_DIR/napcat.pid")" 'NapCat PID file mode'
  assert_eq 600 "$(stat -c '%a' "$NAPCAT_START")" 'NapCat identity file mode'
  assert_eq 600 "$(stat -c '%a' "$RUN_DIR/worker.pid")" 'Worker PID file mode'
  assert_eq 600 "$(stat -c '%a' "$WORKER_START")" 'Worker identity file mode'
  assert_eq '<-config>' "$(sed -n '1p' "$PROBE_ARGS")" 'probe flag'
  assert_eq "<$CONFIG>" "$(sed -n '2p' "$PROBE_ARGS")" 'probe config path'
  assert_eq '<>' "$(<"$WORKER_ARGS")" 'Worker receives no token arguments'
  assert_file_absent "$QQCODEX_TEST_NAPCAT_ENV_LEAK" 'NapCat environment'
  assert_file_absent "$QQCODEX_TEST_WORKER_ENV_LEAK" 'Worker environment'

  stop_wrapper "$WRAPPER_PID"
  wait_for_dead "$napcat_pid"
  wait_for_dead "$worker_pid"
  assert_file_absent "$RUN_DIR/napcat.pid" 'NapCat PID cleanup'
  assert_file_absent "$RUN_DIR/worker.pid" 'Worker PID cleanup'
  assert_file_absent "$NAPCAT_START" 'NapCat identity cleanup'
  assert_file_absent "$WORKER_START" 'Worker identity cleanup'
  assert_contains "$LAST_OUT" 'account 123456, nickname "Test Bot"' 'safe account and nickname status'
  assert_no_secret_output
  pass 'start adopts the first account, validates groups, and cleans up managed fakes'
}

test_start_saved_mismatch() {
  setup_fixture start-mismatch
  printf '111111\n' >"$ACCOUNT"
  chmod 0600 "$ACCOUNT"
  capture mismatch start
  assert_eq 1 "$LAST_STATUS" 'saved account mismatch exit status'
  assert_contains "$LAST_ERR" 'switch-account' 'saved account mismatch guidance'
  assert_file_absent "$WORKER_ARGS" 'Worker must not start on mismatch'
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  spawned=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  track_pid "$spawned"
  wait_for_dead "$spawned"
  assert_eq 111111 "$(<"$ACCOUNT")" 'saved account remains unchanged'
  assert_no_secret_output
  pass 'start rejects a different saved account without invoking Worker'
}

test_missing_groups() {
  setup_fixture missing-groups
  export QQCODEX_TEST_PROBE_JSON='{"self_id":"123456","nickname":"Test Bot","group_ids":[],"missing_group_ids":["70001","70002"]}'
  capture groups start
  assert_eq 1 "$LAST_STATUS" 'missing groups exit status'
  assert_contains "$LAST_ERR" '70001' 'first missing group'
  assert_contains "$LAST_ERR" '70002' 'second missing group'
  assert_file_absent "$WORKER_ARGS" 'Worker must not start with missing groups'
  assert_file_absent "$ACCOUNT" 'account must not be adopted with missing groups'
  assert_no_secret_output
  pass 'all missing groups are reported before Worker starts'
}

test_pid_record_failures_cleanup() {
  setup_fixture napcat-pid-record
  export QQCODEX_TEST_NAPCAT_REMOVE_RUN=1
  capture napcat-record start
  assert_eq 1 "$LAST_STATUS" 'NapCat PID recording failure status'
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  napcat_pid=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  track_pid "$napcat_pid"
  wait_for_dead "$napcat_pid"
  assert_file_absent "$WORKER_ARGS" 'Worker after NapCat PID recording failure'

  setup_fixture worker-pid-record
  export QQCODEX_TEST_WORKER_REMOVE_RUN=1
  capture worker-record start
  assert_eq 1 "$LAST_STATUS" 'Worker PID recording failure status'
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  wait_for_file "$QQCODEX_TEST_WORKER_SPAWNED"
  napcat_pid=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  worker_pid=$(<"$QQCODEX_TEST_WORKER_SPAWNED")
  track_pid "$napcat_pid"
  track_pid "$worker_pid"
  wait_for_dead "$napcat_pid"
  wait_for_dead "$worker_pid"
  assert_no_secret_output
  pass 'verified children are cleaned up when atomic PID recording fails'
}

test_first_account_commit_rollback() {
  setup_fixture first-commit-rollback
  cp "$WEBUI" "$FIXTURE/webui.before"
  export QQCODEX_LOCAL_JQ=$FIXTURE/fake-jq
  export QQCODEX_TEST_JQ_FAIL_ACCOUNT=123456

  capture partial start
  assert_eq 1 "$LAST_STATUS" 'first-account partial commit status'
  cmp -s "$FIXTURE/webui.before" "$WEBUI" || fail 'first-account failure changed WebUI bytes'
  assert_file_absent "$ACCOUNT" 'first-account cache rollback'
  assert_file_absent "$WORKER_ARGS" 'Worker after first-account partial commit'
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  napcat_pid=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  track_pid "$napcat_pid"
  wait_for_dead "$napcat_pid"
  assert_no_secret_output
  pass 'first-account state rolls back if either atomic commit fails'
}

test_cleanup_timeout_keeps_managed_pid() {
  setup_fixture cleanup-timeout
  cp /bin/bash "$WORKER_EXEC"
  chmod 0700 "$WORKER_EXEC"
  export QQCODEX_TEST_WORKER_IGNORE_TERM=1
  export QQCODEX_LOCAL_STOP_TIMEOUT=0

  start_background cleanup-timeout start
  wait_for_file "$WORKER_READY"
  wait_for_file "$RUN_DIR/worker.pid"
  wait_for_file "$RUN_DIR/napcat.pid"
  worker_pid=$(<"$RUN_DIR/worker.pid")
  napcat_pid=$(<"$RUN_DIR/napcat.pid")
  track_pid "$worker_pid"
  track_pid "$napcat_pid"
  stop_wrapper "$WRAPPER_PID"

  kill -0 "$worker_pid" 2>/dev/null || fail 'EXIT cleanup escalated beyond TERM'
  [[ -f $RUN_DIR/worker.pid ]] || fail 'EXIT cleanup removed a live Worker PID file'
  assert_eq "$worker_pid" "$(<"$RUN_DIR/worker.pid")" 'EXIT cleanup live Worker PID retention'
  wait_for_dead "$napcat_pid"
  assert_file_absent "$RUN_DIR/napcat.pid" 'EXIT cleanup NapCat PID removal'

  kill -HUP "$worker_pid"
  wait_for_dead "$worker_pid"
  rm -f -- "$RUN_DIR/worker.pid"
  pass 'EXIT cleanup retains a TERM-resistant verified child PID without escalation'
}

test_switch_success() {
  setup_fixture switch-success
  printf '111111\n' >"$ACCOUNT"
  chmod 0600 "$ACCOUNT"
  start_background switched switch-account
  wait_for_file "$RUN_DIR/worker.pid"
  napcat_pid=$(<"$RUN_DIR/napcat.pid")
  worker_pid=$(<"$RUN_DIR/worker.pid")
  track_pid "$napcat_pid"
  track_pid "$worker_pid"

  assert_eq '<--scan>' "$(<"$NAPCAT_ARGS")" 'switch scan argument'
  assert_eq '' "$(<"$NAPCAT_WEBUI_ACCOUNT")" 'WebUI account while scan launcher starts'
  assert_private_account 123456
  assert_eq 123456 "$(jq -r '.autoLoginAccount' "$WEBUI")" 'switched WebUI account'
  assert_eq 600 "$(stat -c '%a' "$WEBUI")" 'switched WebUI mode'
  assert_eq '["80001","80002"]' "$(jq -c '.employee_ids' "$CONFIG")" 'employee IDs unchanged'
  assert_eq '["80002"]' "$(jq -c '.admin_ids' "$CONFIG")" 'admin IDs unchanged'
  assert_eq '["kept-project"]' "$(jq -c '[.projects[].id]' "$CONFIG")" 'projects unchanged'
  assert_file_absent "$RUN_DIR/webui.snapshot" 'committed snapshot cleanup'
  stop_wrapper "$WRAPPER_PID"
  wait_for_dead "$napcat_pid"
  wait_for_dead "$worker_pid"
  assert_no_secret_output
  pass 'switch-account uses exactly --scan and commits only account state'
}

assert_switch_rollback() {
  local case_name=$1 mode=$2 missing=$3 account_state=$4 probe_json=${5:-}
  setup_fixture "$case_name"
  cp "$WEBUI" "$FIXTURE/webui.before"
  if [[ $account_state == present ]]; then
    printf '111111\n' >"$ACCOUNT"
    chmod 0600 "$ACCOUNT"
    cp "$ACCOUNT" "$FIXTURE/account.before"
  fi
  printf '%s\n' "$mode" >"$PROBE_MODE_FILE"
  if [[ -n $probe_json ]]; then
    export QQCODEX_TEST_PROBE_JSON=$probe_json
  fi
  if [[ $missing == yes ]]; then
    export QQCODEX_TEST_PROBE_JSON='{"self_id":"123456","nickname":"Test Bot","group_ids":["70001"],"missing_group_ids":["70002"]}'
  fi
  capture rollback switch-account
  assert_eq 1 "$LAST_STATUS" "$case_name exit status"
  cmp -s "$FIXTURE/webui.before" "$WEBUI" || fail "$case_name did not restore WebUI bytes"
  assert_eq 600 "$(stat -c '%a' "$WEBUI")" "$case_name WebUI rollback mode"
  if [[ $account_state == present ]]; then
    cmp -s "$FIXTURE/account.before" "$ACCOUNT" || fail "$case_name did not restore account content"
  else
    assert_file_absent "$ACCOUNT" "$case_name account existence rollback"
  fi
  assert_file_absent "$WORKER_ARGS" "$case_name Worker invocation"
  assert_file_absent "$RUN_DIR/webui.snapshot" "$case_name WebUI snapshot cleanup"
  assert_file_absent "$RUN_DIR/account.snapshot" "$case_name account snapshot cleanup"
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  spawned=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  track_pid "$spawned"
  wait_for_dead "$spawned"
  assert_no_secret_output
}

test_switch_rollbacks() {
  assert_switch_rollback switch-error error no present
  assert_switch_rollback switch-invalid invalid no absent
  assert_switch_rollback switch-groups success yes present
  assert_switch_rollback switch-invalid-nickname success no present \
    '{"self_id":"123456","nickname":7,"group_ids":[],"missing_group_ids":[]}'
  assert_switch_rollback switch-invalid-groups success no present \
    '{"self_id":"123456","nickname":"Test Bot","group_ids":[7],"missing_group_ids":[]}'
  assert_switch_rollback switch-invalid-missing success no present \
    '{"self_id":"123456","nickname":"Test Bot","group_ids":[],"missing_group_ids":[7]}'
  pass 'failed, timed-out/invalid, and missing-group switches roll back byte-for-byte'
}

test_switch_cancel_rollback() {
  setup_fixture switch-cancel
  printf '111111\n' >"$ACCOUNT"
  chmod 0600 "$ACCOUNT"
  cp "$WEBUI" "$FIXTURE/webui.before"
  cp "$ACCOUNT" "$FIXTURE/account.before"
  printf 'error\n' >"$PROBE_MODE_FILE"
  export QQCODEX_LOCAL_LOGIN_TIMEOUT=10
  export QQCODEX_LOCAL_PROBE_INTERVAL=0.1

  start_background cancel switch-account
  wait_for_file "$RUN_DIR/napcat.pid"
  napcat_pid=$(<"$RUN_DIR/napcat.pid")
  track_pid "$napcat_pid"
  assert_eq '' "$(jq -r '.autoLoginAccount' "$WEBUI")" 'WebUI account before cancellation'
  stop_wrapper "$WRAPPER_PID"

  wait_for_dead "$napcat_pid"
  cmp -s "$FIXTURE/webui.before" "$WEBUI" || fail 'cancel did not restore WebUI bytes'
  cmp -s "$FIXTURE/account.before" "$ACCOUNT" || fail 'cancel did not restore account content'
  assert_eq 600 "$(stat -c '%a' "$WEBUI")" 'cancelled WebUI rollback mode'
  assert_file_absent "$WORKER_ARGS" 'Worker after cancelled switch'
  assert_file_absent "$RUN_DIR/webui.snapshot" 'cancelled WebUI snapshot cleanup'
  assert_file_absent "$RUN_DIR/account.snapshot" 'cancelled account snapshot cleanup'
  assert_no_secret_output
  pass 'cancelled switch restores exact state and stops the new NapCat child'
}

test_hanging_probe_obeys_timeout() {
  setup_fixture hanging-probe
  printf '111111\n' >"$ACCOUNT"
  chmod 0600 "$ACCOUNT"
  cp "$WEBUI" "$FIXTURE/webui.before"
  cp "$ACCOUNT" "$FIXTURE/account.before"
  printf 'hang\n' >"$PROBE_MODE_FILE"
  export QQCODEX_LOCAL_LOGIN_TIMEOUT=0
  LAST_OUT=$FIXTURE/hang.out
  LAST_ERR=$FIXTURE/hang.err

  set +e
  "$(command -v timeout)" 2 "$LAUNCHER" switch-account >"$LAST_OUT" 2>"$LAST_ERR"
  LAST_STATUS=$?
  set -e
  assert_eq 1 "$LAST_STATUS" 'hanging probe timeout status'
  cmp -s "$FIXTURE/webui.before" "$WEBUI" || fail 'hanging probe did not restore WebUI bytes'
  cmp -s "$FIXTURE/account.before" "$ACCOUNT" || fail 'hanging probe did not restore account content'
  assert_file_absent "$WORKER_ARGS" 'Worker after hanging probe'
  wait_for_file "$QQCODEX_TEST_NAPCAT_SPAWNED"
  napcat_pid=$(<"$QQCODEX_TEST_NAPCAT_SPAWNED")
  track_pid "$napcat_pid"
  wait_for_dead "$napcat_pid"
  assert_no_secret_output
  pass 'a hanging probe is bounded by the configured login timeout'
}

test_symlinked_state_is_refused() {
  setup_fixture dangling-account
  ln -s missing-account "$ACCOUNT"
  printf 'error\n' >"$PROBE_MODE_FILE"
  capture dangling switch-account
  assert_eq 1 "$LAST_STATUS" 'dangling account status'
  [[ -L $ACCOUNT ]] || fail 'dangling account link was not preserved'
  assert_eq missing-account "$(readlink "$ACCOUNT")" 'dangling account link target'
  assert_file_absent "$NAPCAT_ARGS" 'NapCat with dangling account state'

  setup_fixture symlinked-webui
  mv "$WEBUI" "$FIXTURE/webui.target"
  ln -s "$FIXTURE/webui.target" "$WEBUI"
  printf 'error\n' >"$PROBE_MODE_FILE"
  capture symlinked start
  assert_eq 1 "$LAST_STATUS" 'symlinked WebUI status'
  [[ -L $WEBUI ]] || fail 'WebUI symlink was replaced'
  assert_file_absent "$NAPCAT_ARGS" 'NapCat with symlinked WebUI state'
  assert_no_secret_output
  pass 'symlinked transactional state is refused without mutation or launch'
}

test_stop_process_identity() {
  setup_fixture stop-identity
  "$QQ_EXEC" 60 &
  matching=$!
  track_pid "$matching"
  /bin/sleep 60 &
  unrelated=$!
  track_pid "$unrelated"
  write_managed_identity "$matching" "$RUN_DIR/napcat.pid" "$NAPCAT_START"
  write_managed_identity "$unrelated" "$RUN_DIR/worker.pid" "$WORKER_START"

  capture stop stop
  assert_eq 0 "$LAST_STATUS" 'stop exit status'
  wait_for_dead "$matching"
  kill -0 "$unrelated" 2>/dev/null || fail 'stop signalled an identity-mismatched process'
  assert_file_absent "$RUN_DIR/napcat.pid" 'matching PID file cleanup'
  assert_file_absent "$RUN_DIR/worker.pid" 'mismatched PID file cleanup'
  assert_contains "$LAST_ERR" 'mismatch' 'mismatched PID report'

  printf '%s\n' 'not-a-pid' >"$RUN_DIR/napcat.pid"
  capture stale stop
  assert_eq 0 "$LAST_STATUS" 'idempotent stale stop status'
  assert_file_absent "$RUN_DIR/napcat.pid" 'invalid PID cleanup'
  kill -0 "$unrelated" 2>/dev/null || fail 'idempotent stop touched unrelated process'

  "$QQ_EXEC" 60 &
  malformed=$!
  track_pid "$malformed"
  printf '%s\n%s\n' "$malformed" 999999 >"$RUN_DIR/napcat.pid"
  process_start_time "$malformed" >"$NAPCAT_START"
  capture malformed stop
  assert_eq 0 "$LAST_STATUS" 'malformed PID file stop status'
  assert_file_absent "$RUN_DIR/napcat.pid" 'multi-line PID cleanup'
  kill -0 "$malformed" 2>/dev/null || fail 'stop trusted a PID file with trailing data'

  "$QQ_EXEC" 60 &
  reused=$!
  track_pid "$reused"
  printf '%s\n' "$reused" >"$RUN_DIR/napcat.pid"
  reused_start=$(process_start_time "$reused")
  printf '%s\n' "$((reused_start + 1))" >"$NAPCAT_START"
  capture reused stop
  assert_eq 0 "$LAST_STATUS" 'reused PID stop status'
  assert_file_absent "$RUN_DIR/napcat.pid" 'reused PID file cleanup'
  assert_file_absent "$NAPCAT_START" 'reused start-time cleanup'
  kill -0 "$reused" 2>/dev/null || fail 'stop signalled a reused PID for the same executable'
  assert_no_secret_output
  pass 'stop signals only matching executables and removes stale PID files safely'
}

test_stop_timeout_keeps_managed_pid() {
  setup_fixture stop-timeout
  stubborn=$FIXTURE/stubborn-qq
  ready=$FIXTURE/stubborn.ready
  cp /bin/bash "$stubborn"
  chmod 0700 "$stubborn"
  STUBBORN_READY=$ready "$stubborn" -c \
    'trap "" TERM; trap "exit 0" HUP; printf x >"$STUBBORN_READY"; while :; do /bin/sleep 1; done' &
  stubborn_pid=$!
  track_pid "$stubborn_pid"
  wait_for_file "$ready"
  export QQCODEX_LOCAL_QQ_EXECUTABLE=$stubborn
  export QQCODEX_LOCAL_STOP_TIMEOUT=0
  write_managed_identity "$stubborn_pid" "$RUN_DIR/napcat.pid" "$NAPCAT_START"

  capture timeout stop
  assert_eq 1 "$LAST_STATUS" 'TERM-resistant stop status'
  kill -0 "$stubborn_pid" 2>/dev/null || fail 'stop escalated beyond TERM'
  assert_eq "$stubborn_pid" "$(<"$RUN_DIR/napcat.pid")" 'live managed PID retention'
  assert_contains "$LAST_ERR" 'did not stop after TERM' 'bounded stop report'

  kill -HUP "$stubborn_pid"
  wait "$stubborn_pid" 2>/dev/null || true
  rm -f -- "$RUN_DIR/napcat.pid"
  pass 'TERM-resistant managed processes keep their PID file without escalation'
}

assert_conflict() {
  local case_name=$1 kind=$2 message=$3
  setup_fixture "$case_name"
  /bin/sleep 60 &
  conflict_pid=$!
  track_pid "$conflict_pid"
  case "$kind" in
    qq) printf '%s\n' "$conflict_pid" >"$QQ_DISCOVERY" ;;
    worker) printf '%s\n' "$conflict_pid" >"$WORKER_DISCOVERY" ;;
    port) printf '1\n' >"$PORT_STATE" ;;
  esac
  capture conflict start
  assert_eq 1 "$LAST_STATUS" "$case_name exit status"
  assert_contains "$LAST_ERR" "$message" "$case_name conflict report"
  assert_file_absent "$NAPCAT_ARGS" "$case_name NapCat invocation"
  assert_file_absent "$WORKER_ARGS" "$case_name Worker invocation"
  kill -0 "$conflict_pid" 2>/dev/null || fail "$case_name signalled the conflict process"
  assert_no_secret_output
}

test_unmanaged_conflicts() {
  assert_conflict ordinary-qq qq 'unmanaged QQ'
  assert_conflict unmanaged-worker worker 'unmanaged Worker'
  assert_conflict listener port '127.0.0.1:3001'
  pass 'ordinary QQ, unmanaged Worker, and listener conflicts are refused safely'
}

test_discovery_errors_fail_closed() {
  setup_fixture pgrep-error
  export QQCODEX_TEST_PGREP_ERROR=1
  printf 'error\n' >"$PROBE_MODE_FILE"
  capture pgrep-error start
  assert_eq 1 "$LAST_STATUS" 'process discovery error status'
  assert_contains "$LAST_ERR" 'process discovery failed' 'process discovery error report'
  assert_file_absent "$NAPCAT_ARGS" 'NapCat after process discovery error'

  setup_fixture port-error
  export QQCODEX_TEST_PORT_ERROR=1
  printf 'error\n' >"$PROBE_MODE_FILE"
  capture port-error start
  assert_eq 1 "$LAST_STATUS" 'port discovery error status'
  assert_contains "$LAST_ERR" 'port discovery failed' 'port discovery error report'
  assert_file_absent "$NAPCAT_ARGS" 'NapCat after port discovery error'
  assert_no_secret_output
  pass 'preflight fails closed when process or port discovery fails'
}

test_status_states() {
  setup_fixture status-stopped
  capture stopped status
  assert_eq 0 "$LAST_STATUS" 'stopped status exit code'
  assert_contains "$LAST_OUT" 'saved account: none' 'stopped saved account status'
  assert_contains "$LAST_OUT" 'NapCat: stopped' 'stopped NapCat status'
  assert_contains "$LAST_OUT" 'Worker: stopped' 'stopped Worker status'

  printf '123456\n' >"$ACCOUNT"
  "$QQ_EXEC" 60 &
  managed=$!
  track_pid "$managed"
  write_managed_identity "$managed" "$RUN_DIR/napcat.pid" "$NAPCAT_START"
  capture online status
  assert_eq 0 "$LAST_STATUS" 'online status exit code'
  assert_contains "$LAST_OUT" 'NapCat: running' 'online NapCat status'
  assert_contains "$LAST_OUT" 'detected account: 123456' 'online detected account'
  assert_contains "$LAST_OUT" 'Test Bot' 'online nickname'
  kill -TERM "$managed"
  wait "$managed" 2>/dev/null || true
  rm -f "$RUN_DIR/napcat.pid"

  /bin/sleep 60 &
  mismatch=$!
  track_pid "$mismatch"
  write_managed_identity "$mismatch" "$RUN_DIR/napcat.pid" "$NAPCAT_START"
  capture mismatch status
  assert_eq 0 "$LAST_STATUS" 'mismatch status exit code'
  assert_contains "$LAST_ERR" 'mismatch' 'status mismatch report'
  assert_file_absent "$RUN_DIR/napcat.pid" 'status mismatch PID cleanup'
  kill -0 "$mismatch" 2>/dev/null || fail 'status signalled mismatched process'

  printf '%s\n' "$mismatch" >"$QQ_DISCOVERY"
  capture unmanaged status
  assert_eq 0 "$LAST_STATUS" 'unmanaged status exit code'
  assert_contains "$LAST_OUT" 'unmanaged QQ' 'status unmanaged QQ report'
  assert_no_secret_output
  pass 'status reports stopped, online, mismatch, and unmanaged states safely'
}

test_usage() {
  setup_fixture usage
  local args expected
  expected='usage: qqcodex-local {start|switch-account|status|stop}'
  for args in '' 'unknown' 'start extra' 'status extra' 'stop extra'; do
    # shellcheck disable=SC2086
    capture usage $args
    assert_eq 2 "$LAST_STATUS" "usage exit status for [$args]"
    assert_eq "$expected" "$(<"$LAST_ERR")" "usage text for [$args]"
  done
  assert_no_secret_output
  pass 'missing, unknown, and extra arguments return exact usage with exit 2'
}

test_start_first_account
test_start_saved_mismatch
test_missing_groups
test_pid_record_failures_cleanup
test_first_account_commit_rollback
test_cleanup_timeout_keeps_managed_pid
test_switch_success
test_switch_rollbacks
test_switch_cancel_rollback
test_hanging_probe_obeys_timeout
test_symlinked_state_is_refused
test_stop_process_identity
test_stop_timeout_keeps_managed_pid
test_unmanaged_conflicts
test_discovery_errors_fail_closed
test_status_states
test_usage

printf '1..%d\n' "$TEST_COUNT"
