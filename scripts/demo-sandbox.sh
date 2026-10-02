#!/usr/bin/env bash
# Demonstrates defence in depth: approving a dangerous command does not make it
# work.
#
# This is the demo worth recording second. Most assistants that can run commands
# have exactly one layer — a confirmation prompt — which means a user who clicks
# through it has no protection left. NeelKIT has two, and they are independent:
#
#   1. The policy gate escalates. A read is normally allowed outright, but a
#      path that leaves the project turns it into a question.
#   2. The sandbox refuses. It resolves every path against an os.Root handle, so
#      the kernel enforces the boundary and consent cannot widen it.
#
#   NEELKIT_API_KEY=nk_… ./scripts/demo-sandbox.sh
set -euo pipefail

URL=${NEELKIT_URL:-http://127.0.0.1:8420}
TARGET=${1:-../../etc/passwd}

bold=$'\033[1m'; dim=$'\033[2m'; off=$'\033[0m'
red=$'\033[31m'; green=$'\033[32m'; yellow=$'\033[33m'

if [[ -z "${NEELKIT_API_KEY:-}" ]]; then
  echo "Set NEELKIT_API_KEY to the key neelkitd printed on first run." >&2
  exit 1
fi
for cmd in curl python3; do
  command -v "$cmd" >/dev/null || { echo "need $cmd" >&2; exit 1; }
done

auth=(-H "Authorization: Bearer ${NEELKIT_API_KEY}" -H 'Content-Type: application/json')
field() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

printf '%sDefence in depth%s\n' "$bold" "$off"
printf '%sAsking to read a file outside the project: %s%s\n\n' "$dim" "$TARGET" "$off"

turn=$(curl -sf "$URL/v1/assistant/turns" "${auth[@]}" \
  -d "$(python3 -c "
import json,sys
print(json.dumps({'text': 'read the file ' + sys.argv[1]}))" "$TARGET")")

status=$(printf '%s' "$turn" | field "['status']")
if [[ "$status" != "awaiting_approval" ]]; then
  printf '%sLayer 1 did not escalate (status: %s).%s\n' "$red" "$status" "$off"
  printf 'That is a regression — a path leaving the workspace must always ask.\n'
  exit 1
fi

call_id=$(printf '%s' "$turn" | field "['calls'][0]['call_id']")
reason=$(printf '%s' "$turn" | field "['calls'][0]['reason']")
preview=$(printf '%s' "$turn" | field "['calls'][0]['preview']")

printf '%s1. The policy gate escalated%s\n' "$yellow" "$off"
printf '   a read is normally allowed outright, but:\n'
printf '   %s%s%s\n' "$bold" "$reason" "$off"
printf '   will run: %s%s%s\n\n' "$dim" "$preview" "$off"
sleep 1.5

printf '%s2. Approving it anyway — the worst thing a user can do%s\n\n' "$yellow" "$off"
sleep 1

result=$(curl -sf "$URL/v1/assistant/approvals" "${auth[@]}" \
  -d "$(python3 -c "
import json,sys
print(json.dumps({'call_id': sys.argv[1], 'decision': 'approve'}))" "$call_id")")

final=$(printf '%s' "$result" | field "['status']")
err=$(printf '%s' "$result" | field ".get('error','')")

if [[ "$final" == "failed" && -n "$err" ]]; then
  printf '%s3. The sandbox refused it regardless%s\n' "$green" "$off"
  printf '   %s%s%s\n\n' "$bold" "$err" "$off"
  printf '%sConsent cannot widen the sandbox. The gate asks; the kernel decides.%s\n' "$dim" "$off"
  exit 0
fi

printf '%sThe file was read after approval. This is a serious regression.%s\n' "$red" "$off"
printf 'status=%s\n' "$final"
exit 1
