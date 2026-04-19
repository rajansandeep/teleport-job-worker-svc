#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

CLI="${ROOT}/jobworker-cli"
SERVER="${ROOT}/jobworker-server"
ADDR="${ADDR:-localhost:50051}"
LISTEN="${LISTEN:-:50051}"

TMP_DIR="$(mktemp -d)"
SERVER_LOG="${TMP_DIR}/server.log"
BG_PIDS=()
SERVER_PID=""
FAILED=0

cleanup() {
  for pid in "${BG_PIDS[@]:-}"; do
    kill "${pid}" 2>/dev/null || true
  done

  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi

  # Dump the server log to stderr on failure so it is not lost with TMP_DIR.
  if [[ "${FAILED}" -eq 1 && -f "${SERVER_LOG}" ]]; then
    printf '\n==> Server log (dumped on failure):\n' >&2
    cat "${SERVER_LOG}" >&2
  fi

  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

info()  { printf '\n==> %s\n' "$*"; }
pass()  { printf 'PASS: %s\n' "$*"; }
fail()  { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="$3"
  [[ "${got}" == "${want}" ]] || fail "${msg}: got='${got}' want='${want}'"
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local msg="$3"
  grep -Fq "${needle}" <<<"${haystack}" || fail "${msg}: missing '${needle}'"
}

assert_file_contains() {
  local file="$1"
  local needle="$2"
  local msg="$3"
  grep -Fq "${needle}" "${file}" || fail "${msg}: missing '${needle}' in ${file}"
}

assert_file_line_count() {
  local file="$1"
  local want="$2"
  local msg="$3"
  local got
  got="$(wc -l < "${file}" | tr -d ' ')"
  [[ "${got}" == "${want}" ]] || fail "${msg}: got=${got} want=${want}"
}

# assert_job_id validates that the value looks like a UUID and fails with a
# clear message if the CLI wrote an error to stdout instead of a job ID.
assert_job_id() {
  local id="$1"
  local msg="${2:-job id}"
  [[ -n "${id}" ]] || fail "${msg}: empty"
  [[ "${id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] \
    || fail "${msg}: invalid UUID: '${id}'"
}

status_output() {
  "${CLI}" --addr "${ADDR}" status "$1"
}

status_state() {
  status_output "$1" | sed -n 's/^State:[[:space:]]*//p'
}

status_exit_code() {
  status_output "$1" | sed -n 's/^Exit Code:[[:space:]]*//p'
}

wait_for_state() {
  local job_id="$1"
  local want="$2"
  local deadline=$((SECONDS + 20))

  while (( SECONDS < deadline )); do
    local state
    state="$(status_state "${job_id}")"
    if [[ "${state}" == "${want}" ]]; then
      return 0
    fi
    sleep 0.1
  done

  fail "timed out waiting for job ${job_id} to reach state ${want}"
}

start_job() {
  "${CLI}" --addr "${ADDR}" start -- "$@"
}

run_cli_expect_fail() {
  set +e
  local output
  output="$("$@" 2>&1)"
  local code=$?
  set -e

  if [[ ${code} -eq 0 ]]; then
    fail "expected command to fail but it succeeded: $*"
  fi

  printf '%s' "${output}"
}

run_viewer_cli() {
  JOBWORKER_CERT="${ROOT}/certs/viewer.crt" \
  JOBWORKER_KEY="${ROOT}/certs/viewer.key" \
  JOBWORKER_CA="${ROOT}/certs/ca.crt" \
  "${CLI}" --addr "${ADDR}" "$@"
}

wait_for_server() {
  local deadline=$((SECONDS + 10))
  while (( SECONDS < deadline )); do
    if [[ -f "${SERVER_LOG}" ]] && grep -q "listening" "${SERVER_LOG}"; then
      return 0
    fi
    if [[ -n "${SERVER_PID}" ]] && ! kill -0 "${SERVER_PID}" 2>/dev/null; then
      cat "${SERVER_LOG}" >&2 || true
      fail "server exited early"
    fi
    sleep 0.1
  done

  cat "${SERVER_LOG}" >&2 || true
  fail "server did not start in time"
}

bounded_loop_cmd() {
  local total_lines="$1"
  local sleep_secs="$2"

  cat <<EOF
i=1
while [ \$i -le ${total_lines} ]; do
  echo "line-\$i"
  i=\$((i+1))
  sleep ${sleep_secs}
done
EOF
}

info "Checking prerequisites"
require_cmd go
require_cmd sh
require_cmd sed
require_cmd grep
require_cmd cmp
require_cmd wc
require_cmd head
require_cmd tail
require_cmd make
require_cmd openssl

info "Running race detector"
go test -race ./...
pass "go test -race ./... passed"

info "Building binaries"
go build -o "${SERVER}" ./cmd/jobworker-server
go build -o "${CLI}" ./cmd/jobworker-cli
pass "binaries built"

info "Generating local certs"
make certs >/dev/null
pass "certs generated"

info "Starting server"
"${SERVER}" \
  --cert "${ROOT}/certs/server.crt" \
  --key  "${ROOT}/certs/server.key" \
  --ca   "${ROOT}/certs/ca.crt" \
  --listen "${LISTEN}" >"${SERVER_LOG}" 2>&1 &
SERVER_PID="$!"
wait_for_server
pass "server started"

export JOBWORKER_CERT="${ROOT}/certs/admin.crt"
export JOBWORKER_KEY="${ROOT}/certs/admin.key"
export JOBWORKER_CA="${ROOT}/certs/ca.crt"

###############################################################################
# 1. Basic successful job
###############################################################################
info "Scenario 1: successful job completes and streams output"

JOB1="$(start_job sh -c 'echo hello; echo world')"
assert_job_id "${JOB1}" "scenario 1 job id"

wait_for_state "${JOB1}" "Completed"
assert_eq "$(status_exit_code "${JOB1}")" "0" "successful job exit code"

OUT1="${TMP_DIR}/job1.out"
"${CLI}" --addr "${ADDR}" stream "${JOB1}" > "${OUT1}"
assert_file_contains "${OUT1}" "hello" "successful job output"
assert_file_contains "${OUT1}" "world" "successful job output"
pass "successful job scenario"

###############################################################################
# 2. Failed job
###############################################################################
info "Scenario 2: failed job reports Failed and non-zero exit"

JOB2="$(start_job sh -c 'echo boom 1>&2; exit 7')"
assert_job_id "${JOB2}" "scenario 2 job id"
wait_for_state "${JOB2}" "Failed"
assert_eq "$(status_exit_code "${JOB2}")" "7" "failed job exit code"

OUT2="${TMP_DIR}/job2.out"
"${CLI}" --addr "${ADDR}" stream "${JOB2}" > "${OUT2}"
assert_file_contains "${OUT2}" "boom" "failed job output"
pass "failed job scenario"

###############################################################################
# 3. Stop running job
###############################################################################
info "Scenario 3: stop a long-running job"

JOB3="$(start_job sh -c 'echo started; sleep 30')"
assert_job_id "${JOB3}" "scenario 3 job id"
wait_for_state "${JOB3}" "Running"
"${CLI}" --addr "${ADDR}" stop "${JOB3}"
wait_for_state "${JOB3}" "Stopped"

OUT3="${TMP_DIR}/job3.out"
"${CLI}" --addr "${ADDR}" stream "${JOB3}" > "${OUT3}"
assert_file_contains "${OUT3}" "started" "stopped job initial output"
pass "stop scenario"

###############################################################################
# 4. Authorization
###############################################################################
info "Scenario 4: viewer authz"

DENIED_START="$(run_cli_expect_fail run_viewer_cli start -- echo hello)"
assert_contains "${DENIED_START}" "permission denied" "viewer start should be denied"

JOB4="$(start_job sh -c 'echo visible; sleep 2')"
assert_job_id "${JOB4}" "scenario 4 job id"
wait_for_state "${JOB4}" "Running"

DENIED_STOP="$(run_cli_expect_fail run_viewer_cli stop "${JOB4}")"
assert_contains "${DENIED_STOP}" "permission denied" "viewer stop should be denied"

VIEWER_STATUS="$(run_viewer_cli status "${JOB4}")"
assert_contains "${VIEWER_STATUS}" "ID:" "viewer status should be allowed"

VIEWER_STREAM="${TMP_DIR}/viewer-stream.out"
run_viewer_cli stream "${JOB4}" > "${VIEWER_STREAM}"
assert_file_contains "${VIEWER_STREAM}" "visible" "viewer stream should be allowed"
pass "authorization scenario"

###############################################################################
# 5. Multi-stream late joiners
###############################################################################
info "Scenario 5: multiple concurrent streams with different join times"

TOTAL_LINES=120
SLEEP_SECS=0.02
JOB5_CMD="$(bounded_loop_cmd "${TOTAL_LINES}" "${SLEEP_SECS}")"
JOB5="$(start_job sh -c "${JOB5_CMD}")"
assert_job_id "${JOB5}" "scenario 5 job id"

S1="${TMP_DIR}/stream1.txt"
S2="${TMP_DIR}/stream2.txt"
S3="${TMP_DIR}/stream3.txt"

"${CLI}" --addr "${ADDR}" stream "${JOB5}" > "${S1}" &
P1=$!
BG_PIDS+=("${P1}")

sleep 0.5
"${CLI}" --addr "${ADDR}" stream "${JOB5}" > "${S2}" &
P2=$!
BG_PIDS+=("${P2}")

sleep 0.8
"${CLI}" --addr "${ADDR}" stream "${JOB5}" > "${S3}" &
P3=$!
BG_PIDS+=("${P3}")

wait "${P1}"
wait "${P2}"
wait "${P3}"

assert_file_line_count "${S1}" "${TOTAL_LINES}" "stream1 line count"
assert_file_line_count "${S2}" "${TOTAL_LINES}" "stream2 line count"
assert_file_line_count "${S3}" "${TOTAL_LINES}" "stream3 line count"

assert_eq "$(head -1 "${S1}")" "line-1" "stream1 should replay from offset 0"
assert_eq "$(head -1 "${S2}")" "line-1" "stream2 should replay from offset 0"
assert_eq "$(head -1 "${S3}")" "line-1" "stream3 should replay from offset 0"

assert_eq "$(tail -1 "${S1}")" "line-${TOTAL_LINES}" "stream1 final line"
assert_eq "$(tail -1 "${S2}")" "line-${TOTAL_LINES}" "stream2 final line"
assert_eq "$(tail -1 "${S3}")" "line-${TOTAL_LINES}" "stream3 final line"

cmp "${S1}" "${S2}" >/dev/null || fail "stream1 and stream2 differ"
cmp "${S1}" "${S3}" >/dev/null || fail "stream1 and stream3 differ"

pass "multi-stream late joiner scenario"

###############################################################################
# 6. Early reader exit does not affect job or other readers
###############################################################################
info "Scenario 6: one reader exits early, another still gets full output"

TOTAL_LINES2=150
SLEEP_SECS2=0.02
JOB6_CMD="$(bounded_loop_cmd "${TOTAL_LINES2}" "${SLEEP_SECS2}")"
JOB6="$(start_job sh -c "${JOB6_CMD}")"
assert_job_id "${JOB6}" "scenario 6 job id"

EARLY="${TMP_DIR}/early.txt"
FULL="${TMP_DIR}/full.txt"

"${CLI}" --addr "${ADDR}" stream "${JOB6}" > "${EARLY}" 2>/dev/null &
P4=$!
BG_PIDS+=("${P4}")

"${CLI}" --addr "${ADDR}" stream "${JOB6}" > "${FULL}" &
P5=$!
BG_PIDS+=("${P5}")

sleep 0.5
kill "${P4}" 2>/dev/null || true
wait "${P5}"

EARLY_LINES="$(wc -l < "${EARLY}" | tr -d ' ')"
FULL_LINES="$(wc -l < "${FULL}" | tr -d ' ')"

[[ "${EARLY_LINES}" -gt 0 ]]          || fail "early reader should have received some output before being killed"
[[ "${EARLY_LINES}" -lt "${TOTAL_LINES2}" ]] || fail "early reader should have partial output"
[[ "${FULL_LINES}" == "${TOTAL_LINES2}" ]]   || fail "full reader should have complete output"
assert_eq "$(tail -1 "${FULL}")" "line-${TOTAL_LINES2}" "full reader final line"
wait_for_state "${JOB6}" "Completed"

pass "early reader exit scenario"

###############################################################################
# 7. Concurrent stop storm
###############################################################################
info "Scenario 7: concurrent Stop requests"

JOB7="$(start_job sh -c 'sleep 30')"
assert_job_id "${JOB7}" "scenario 7 job id"
wait_for_state "${JOB7}" "Running"

STOP_OK=0
STOP_FAIL=0
STOP_PIDS=()

for i in $(seq 1 10); do
  (
    if "${CLI}" --addr "${ADDR}" stop "${JOB7}" >/dev/null 2>"${TMP_DIR}/stop-${i}.err"; then
      echo ok
    else
      echo fail
    fi
  ) > "${TMP_DIR}/stop-${i}.result" &
  STOP_PIDS+=("$!")
done

for pid in "${STOP_PIDS[@]}"; do
  wait "${pid}"
done

for i in $(seq 1 10); do
  RESULT="$(cat "${TMP_DIR}/stop-${i}.result")"
  if [[ "${RESULT}" == "ok" ]]; then
    STOP_OK=$((STOP_OK + 1))
  else
    STOP_FAIL=$((STOP_FAIL + 1))
    ERRMSG="$(cat "${TMP_DIR}/stop-${i}.err")"
    assert_contains "${ERRMSG}" "job is not running" "concurrent stop loser message"
  fi
done

assert_eq "${STOP_OK}" "1" "exactly one stop should succeed"
assert_eq "${STOP_FAIL}" "9" "remaining stops should fail"
wait_for_state "${JOB7}" "Stopped"

pass "concurrent stop scenario"

###############################################################################
# 8. Concurrent status polling under load
###############################################################################
info "Scenario 8: concurrent status polling while job runs"

TOTAL_LINES3=80
SLEEP_SECS3=0.03
JOB8_CMD="$(bounded_loop_cmd "${TOTAL_LINES3}" "${SLEEP_SECS3}")"
JOB8="$(start_job sh -c "${JOB8_CMD}")"
assert_job_id "${JOB8}" "scenario 8 job id"

STATUS_PIDS=()

for i in $(seq 1 20); do
  (
    deadline=$((SECONDS + 10))
    while (( SECONDS < deadline )); do
      OUT="$("${CLI}" --addr "${ADDR}" status "${JOB8}" 2>&1)" || exit 1
      grep -q "^ID:" <<<"${OUT}" || exit 1
      STATE="$(sed -n 's/^State:[[:space:]]*//p' <<<"${OUT}")"
      if [[ "${STATE}" == "Completed" || "${STATE}" == "Failed" || "${STATE}" == "Stopped" ]]; then
        exit 0
      fi
      sleep 0.05
    done
    exit 1
  ) &
  STATUS_PIDS+=("$!")
done

for pid in "${STATUS_PIDS[@]}"; do
  wait "${pid}"
done

wait_for_state "${JOB8}" "Completed"
pass "concurrent status polling scenario"

###############################################################################
# 9. Not-found behavior
###############################################################################
info "Scenario 9: invalid job id behavior"

MISSING="00000000-0000-4000-8000-000000000000"

NF_STATUS="$(run_cli_expect_fail "${CLI}" --addr "${ADDR}" status "${MISSING}")"
assert_contains "${NF_STATUS}" "job not found" "status missing job"

NF_STOP="$(run_cli_expect_fail "${CLI}" --addr "${ADDR}" stop "${MISSING}")"
assert_contains "${NF_STOP}" "job not found" "stop missing job"

NF_STREAM="$(run_cli_expect_fail "${CLI}" --addr "${ADDR}" stream "${MISSING}")"
assert_contains "${NF_STREAM}" "job not found" "stream missing job"

pass "not-found scenario"

###############################################################################
# 10. Stream a job that has already completed (replay from offset 0)
###############################################################################
info "Scenario 10: stream a job that completed before stream is called"

JOB10="$(start_job sh -c 'echo line1; echo line2; echo line3')"
assert_job_id "${JOB10}" "scenario 10 job id"
wait_for_state "${JOB10}" "Completed"

OUT10="${TMP_DIR}/job10.out"
"${CLI}" --addr "${ADDR}" stream "${JOB10}" > "${OUT10}"
assert_file_line_count "${OUT10}" "3" "completed job replay line count"
assert_eq "$(head -1 "${OUT10}")" "line1" "completed job replay first line"
assert_eq "$(tail -1 "${OUT10}")" "line3" "completed job replay last line"
pass "stream completed job scenario"

###############################################################################
# 11. Job that produces no output
###############################################################################
info "Scenario 11: job with no output exits cleanly"

JOB11="$(start_job sh -c 'exit 0')"
assert_job_id "${JOB11}" "scenario 11 job id"
wait_for_state "${JOB11}" "Completed"
assert_eq "$(status_exit_code "${JOB11}")" "0" "no-output job exit code"

OUT11="${TMP_DIR}/job11.out"
"${CLI}" --addr "${ADDR}" stream "${JOB11}" > "${OUT11}"
assert_file_line_count "${OUT11}" "0" "no-output job stream should be empty"
pass "no-output job scenario"

###############################################################################
# 12. Non-existent binary
###############################################################################
info "Scenario 12: start with a non-existent binary"

NF_BIN_ERR="$(run_cli_expect_fail start_job /no/such/binary/xyz)"
[[ -n "${NF_BIN_ERR}" ]] || fail "non-existent binary: expected error output, got none"
pass "non-existent binary scenario"

###############################################################################
# 13. Invalid (empty) command
###############################################################################
info "Scenario 13: start with an empty command is rejected"

EMPTY_CMD_ERR="$(run_cli_expect_fail "${CLI}" --addr "${ADDR}" start -- '')"
assert_contains "${EMPTY_CMD_ERR}" "invalid command" "empty command should be rejected"
pass "invalid command scenario"

###############################################################################
# 14. Stop a job that has already completed
###############################################################################
info "Scenario 14: stop a completed job returns not-running"

JOB14="$(start_job sh -c 'echo done')"
assert_job_id "${JOB14}" "scenario 14 job id"
wait_for_state "${JOB14}" "Completed"

STOP_COMPLETED_ERR="$(run_cli_expect_fail "${CLI}" --addr "${ADDR}" stop "${JOB14}")"
assert_contains "${STOP_COMPLETED_ERR}" "job is not running" "stop completed job should return not-running"
pass "stop completed job scenario"

###############################################################################
# 15. Viewer accessing a non-existent job (authz fires before existence check)
###############################################################################
info "Scenario 15: viewer status on a non-existent job returns not-found"

VIEWER_NF="$(run_cli_expect_fail run_viewer_cli status "${MISSING}")"
assert_contains "${VIEWER_NF}" "job not found" "viewer status missing job should return not-found"
pass "viewer not-found scenario"

info "All scenarios passed"
echo "Server log: ${SERVER_LOG}"
