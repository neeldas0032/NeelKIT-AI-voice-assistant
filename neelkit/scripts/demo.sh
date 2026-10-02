#!/usr/bin/env bash
# A scripted run for recording the README demo.
#
# The point of a recording is that the same thing happens every time, at a pace
# a viewer can read. Driving the CLI by hand produces typos, dead air, and a
# thirty-second GIF where fifteen seconds are someone finding the right key.
#
#   NEELKIT_API_KEY=nk_… ./scripts/demo.sh
#
# Record it with any terminal recorder, for example:
#   asciinema rec demo.cast -c ./scripts/demo.sh
#   agg demo.cast docs/demo.gif --theme asciinema --speed 1.2
set -euo pipefail

NK=${NK:-./bin/nk}
PAUSE=${PAUSE:-1.4}   # beat between steps; raise it if the GIF reads too fast
TYPE=${TYPE:-0.035}   # seconds per character

bold=$'\033[1m'; dim=$'\033[2m'; off=$'\033[0m'
cyan=$'\033[36m'; green=$'\033[32m'

if [[ -z "${NEELKIT_API_KEY:-}" ]]; then
  echo "Set NEELKIT_API_KEY to the key neelkitd printed on first run." >&2
  exit 1
fi
if ! "$NK" doctor >/dev/null 2>&1; then
  echo "neelkitd is not reachable. Start it first, then run this again." >&2
  exit 1
fi

# Types a line out one character at a time, so a viewer can follow it.
say() {
  printf '%s$%s ' "$dim" "$off"
  local s=$1
  for (( i=0; i<${#s}; i++ )); do
    printf '%s' "${s:i:1}"
    sleep "$TYPE"
  done
  printf '\n'
  sleep 0.3
}

beat() { sleep "$PAUSE"; }

clear
printf '%sNeelKIT%s — speak, and it does the thing.\n' "$bold" "$off"
printf '%sEverything below runs on this machine. No API key, no cloud.%s\n\n' "$dim" "$off"
beat

# 1. A read. No prompt, because nothing changes.
printf '%s# A read runs immediately — nothing changes, so nothing to approve%s\n' "$cyan" "$off"
say "nk ask \"list the files\""
"$NK" ask "list the files"
echo
beat

# 2. Normalisation. This is the line that makes voice usable at all.
printf '%s# \"feature dash auth\" is what a recogniser hears. It becomes feature-auth%s\n' "$cyan" "$off"
say "nk plan \"create a git branch called feature dash auth\""
"$NK" plan "create a git branch called feature dash auth"
echo
beat

# 3. The gate. The whole product is this moment.
printf '%s# A write stops and shows the exact command before it runs%s\n' "$cyan" "$off"
say "nk ask \"make a note to refactor the audio ring buffer\""
"$NK" ask "make a note to refactor the audio ring buffer"
echo
beat

# 4. The audit trail, including anything that was denied.
printf '%s# Every attempt is recorded before it runs — including the denied ones%s\n' "$cyan" "$off"
say "nk audit 1"
"$NK" audit 1
echo
beat

printf '%sNo audio, transcript or source file left this machine.%s\n' "$green" "$off"
