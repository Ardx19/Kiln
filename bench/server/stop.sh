#!/bin/sh
# Stops the benchmark supervisor on the kiln-bench server. Within a tick
# it kills the running step with its whole process tree (bench records
# nothing for it) and exits; bench/server/start.sh later carries on from
# there. --now kills the supervisor's whole session at once instead.
#
#   bench/server/stop.sh [--now]
set -eu
root=$(cd "$(dirname "$0")/../../.." && pwd)
touch "$root/run/STOP"
[ "${1:-}" = --now ] || exit 0
for pid in $(pgrep -u "$(id -u)" -x benchq); do
	# Only a supervisor leads its own session; never kill the session of
	# a benchq status run from someone's login.
	[ "$(ps -o sid= -p "$pid" | tr -d ' ')" = "$pid" ] && pkill -KILL -s "$pid"
done
exit 0
