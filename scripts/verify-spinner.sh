#!/usr/bin/env bash
#
# Verify the sidebar spinner against a RUNNING gateway, in a real browser, with a
# simulated LLM slow enough to watch. Writes screenshots and a numeric verdict.
#
#   ./verify-spinner.sh [cdp-port]
#
# The check is about a thing that happens OVER TIME - a ring that fades in while
# the agent works, turns, and fades out when the answer lands - so it needs a run
# whose duration it can observe. The simulated LLM is therefore held back
# (MOCK_DELAY_MS), and the gateway it drives is one this script starts and owns:
# pointing it at a live gateway would mean either waiting for whatever the user
# happens to be doing or injecting turns into their real conversations.
#
# Everything it measures is a property of the RENDERED page, which is why it
# needs a browser: the defects it exists to catch (a ring that never turns, one
# that pushes the row it sits on, one that resolves against the sidebar, one that
# outlives its turn) are all invisible in the markup.
set -uo pipefail

CDP_PORT="${1:-9355}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SCRATCH="${SCRATCH:-/home/madkoding/.hermes/cache/scratch}"
OUT="${OUT:-${TMPDIR:-/tmp}/motita-spinner}"
PY="${PY:-$SCRATCH/.cdp/bin/python}"
CHROME="${CHROME:-$HOME/.hermes/cache/chrome/chrome-headless-shell-linux64/chrome-headless-shell}"
PORT_LLM="${PORT_LLM:-8611}"
PORT_GW="${PORT_GW:-8612}"
BASE="http://127.0.0.1:$PORT_GW"
HOME_DIR="$OUT/home"
MOCK_DELAY_MS="${MOCK_DELAY_MS:-900}"
mkdir -p "$OUT"

fail=0
ok()  { printf '  ok    %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }

# The gateway and the simulated model are children of this script: they are
# started here and stopped here, whatever the outcome.
LLM_PID=""
GW_PID=""
CHROME_STARTED=""
cleanup() {
  [ -n "$GW_PID" ]  && kill "$GW_PID"  2>/dev/null
  [ -n "$LLM_PID" ] && kill "$LLM_PID" 2>/dev/null
  # A bare `wait` here waits for EVERY child - and the browser is one, and never
  # exits, so the script would hang after printing its verdict. Only the two
  # processes above are waited for, and the browser is killed outright (and only
  # if this script is the one that started it).
  [ -n "$CHROME_STARTED" ] && pkill -f "remote-debugging-port=$CDP_PORT" 2>/dev/null
  return 0
}
trap cleanup EXIT

export PATH="${PATH}:/home/madkoding/.hermes/cache/go/bin"
command -v go >/dev/null 2>&1 || { echo "ERROR: go is not on the PATH"; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "ERROR: curl is not on the PATH"; exit 2; }
# A browser this machine does not have is a MISSING TOOL, not a defect in the
# spinner. Exit 2 is the convention verify.sh reads as "skipped" - the same one
# check-binary-size.sh uses for a binary that is not built - so a contributor
# without the browser gets an honest skip instead of a red gate that says the
# feature is broken.
#
# NOTE: this runs in the LOCAL gate. Like the other browser probes here
# (verify-tasks-layout.sh, verify-modal-blur.sh), CI does not run it and does not
# install a browser; only e2e-gateway.sh and e2e-agent.sh run there.
if [ ! -x "$CHROME" ]; then
  echo "SKIP: no chrome-headless-shell at $CHROME"
  echo "      set CHROME=... to the one on this machine"
  exit 2
fi

echo "==> Building the agent and the simulated LLM"
go build -o "$OUT/motita" ./cmd/agent || { echo "ERROR: the agent does not build"; exit 2; }
go build -o "$OUT/mockllm" ./tools/mockllm || { echo "ERROR: the simulated LLM does not build"; exit 2; }

echo "==> Starting them, with a reply slow enough to watch (${MOCK_DELAY_MS} ms)"
# The gateway resolves its state directory as $HOME/.motita - config.Dir() reads
# HOME, and hardcodes the ".motita" leaf. There is no MOTITA_HOME. So the way to
# keep this check off the user's real conversations is to give these processes a
# HOME of their own: without it the gateway loads and WRITES ~/.motita/sessions,
# and a check about a spinner would be injecting turns into real conversations.
rm -rf "$HOME_DIR" && mkdir -p "$HOME_DIR/.motita/sessions" "$OUT/work"
printf 'Say something, so that a turn actually runs.\n' > "$OUT/task.txt"
# sandbox.kind=none: the check is about the UI, and a container would need
# privileges that say nothing about whether a ring turns.
cat > "$HOME_DIR/config.yaml" <<YAML
task_source:
  kind: file
  path: $OUT/task.txt
anchor:
  kind: command
  command: sh
  args: ["-c", "echo ANCHOR_OK"]
  expect_exit: 0
  expect_output: "ANCHOR_OK"
  timeout: 30s
sandbox:
  kind: none
llm:
  provider: openai
  model: simulated
  base_url: http://127.0.0.1:$PORT_LLM/v1
  max_tokens: 512
  temperature: 0.0
  timeout: 30s
  max_attempts: 1
agent:
  max_retries: 1
  workspace_dir: $OUT/work
  log_level: info
  log_console: false
gateway:
  enabled: true
  listen: "127.0.0.1:$PORT_GW"
  token_file: "$HOME_DIR/.motita/gateway.token"
  allow: []
  max_body_kb: 256
YAML
HOME="$HOME_DIR" "$OUT/mockllm" -port "$PORT_LLM" -delay-ms "$MOCK_DELAY_MS" \
  > "$OUT/mock.log" 2>&1 &
LLM_PID=$!
sleep 0.4
# The agent's own HOME too, so the log it writes and the state it loads stay here.
NO_COLOR=1 HOME="$HOME_DIR" MOTITA_LLM_API_KEY=test \
  "$OUT/motita" -config "$HOME_DIR/config.yaml" -serve \
  > "$OUT/gateway.log" 2>&1 &
GW_PID=$!

# The token file AND a 200 from /v1/health together are the readiness signal: the
# token is written before the bind, so a token alone is not enough, and an
# answering port alone is not enough.
TOKEN_FILE="$HOME_DIR/.motita/gateway.token"
deadline=$((SECONDS + 45))
while [ "$SECONDS" -lt "$deadline" ]; do
  if [ -s "$TOKEN_FILE" ] && curl -fsS -o /dev/null "$BASE/v1/health" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
if ! curl -fsS -o /dev/null "$BASE/v1/health" 2>/dev/null; then
  echo "ERROR: the gateway never came up; its log holds:"
  sed -n 1,40p "$OUT/gateway.log" | sed 's/^/    /'
  exit 2
fi
ok "the gateway is answering on $BASE"

echo
echo "== 1. the served stylesheet carries the spinner's own rules =="
# The spinner is plain CSS in index.css precisely because the Tailwind utilities
# this project disables emit nothing - `animate-spin` is all over App.tsx and
# produces ZERO rules in the built file. So the first thing to check is that the
# stylesheet the page actually serves defines them at all.
CSS_NAME="$(curl -fsS "$BASE/" | grep -o 'assets/index-[A-Za-z0-9_-]*\.css' | head -1)"
if [ -z "$CSS_NAME" ]; then
  bad "the page names no stylesheet"
else
  ok "stylesheet named by the page: $CSS_NAME"
  css="$(curl -fsS "$BASE/$CSS_NAME")"
  for c in session-spinner session-row is-running; do
    case "$css" in
      *".$c"*) ok "served CSS defines .$c" ;;
      *) bad "served CSS has no .$c" ;;
    esac
  done
  # The rotation must be a real keyframe rule. A class name alone would pass over
  # a static icon, which is exactly the bug this replaced.
  case "$css" in
    *"@keyframes"*) ok "the served CSS declares keyframes" ;;
    *) bad "the served CSS declares no @keyframes: nothing can rotate" ;;
  esac
  case "$css" in
    *"opacity:0"*|*"opacity: 0"*) ok "the spinner's hidden state is emitted" ;;
    *) bad "the served CSS never hides the spinner" ;;
  esac
fi

echo
echo "== 2. the lifecycle in a real browser =="
if ! curl -fsS -o /dev/null "http://127.0.0.1:$CDP_PORT/json/version"; then
  echo "  (starting chrome-headless-shell on port $CDP_PORT)"
  "$CHROME" --headless --remote-debugging-port="$CDP_PORT" \
    --user-data-dir="$OUT/cdp-profile" --no-sandbox --disable-gpu about:blank \
    > "$OUT/chrome.log" 2>&1 &
  CHROME_STARTED=1
  # -f, not -fsS: the probe is waiting for the browser to come up, and the
  # connection refused in the meantime is expected, not a problem to report.
  for _ in $(seq 1 40); do
    curl -sf -o /dev/null "http://127.0.0.1:$CDP_PORT/json/version" && break
    sleep 0.25
  done
fi
if [ ! -x "$PY" ]; then
  echo "  (creating the probe venv at $SCRATCH/.cdp)"
  uv venv "$SCRATCH/.cdp" --python /usr/bin/python3 >/dev/null 2>&1
  uv pip install --python "$PY" websockets pillow >/dev/null 2>&1
fi
export GATEWAY_URL="$BASE" GATEWAY_STATE="$TOKEN_FILE" CDP_PORT SHOTS_DIR="$OUT"
"$PY" "$REPO/scripts/cdp_spinner.py" > "$OUT/probe.out" 2>&1
probe_rc=$?
# The probe's own exit code IS the verdict: it counts its failed assertions and
# returns non-zero if there are any. Its report is re-emitted here so the whole
# check reads as one output. A run that printed no VERDICT line crashed rather
# than judged, which is a failure of the check itself and must not pass silently.
sed -n '1,200p' "$OUT/probe.out" | sed 's/^/  /'
if grep -q '^VERDICT:' "$OUT/probe.out"; then
  if [ "$probe_rc" != "0" ]; then fail=$((fail+1)); fi
else
  bad "the probe printed no verdict (exit $probe_rc); full output in $OUT/probe.out"
fi

echo
if [ "$fail" = "0" ]; then
  echo "VERDICT: the spinner appears while the agent works, turns, and clears when the answer lands"
else
  echo "VERDICT: FAILED ($fail) - screenshots and the probe log are in $OUT"
fi
exit "$fail"
