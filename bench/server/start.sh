#!/bin/sh
# Starts the benchmark supervisor (tools/benchq) on the shared kiln-bench
# server, detached: on 24 of its 32 CPUs, at the lowest priority, and as
# the leader of its own session, so that it outlives the SSH login and
# its processes can be told apart from everyone else's. Run it again
# after a halt, a stop or a reboot: it carries on from run/state.json.
# See docs/benchmarks.md, "Server profile".
#
#   bench/server/start.sh [benchq flags...]     (in the clone at $ROOT/kiln)
set -eu
root=$(cd "$(dirname "$0")/../../.." && pwd)
mask=0-11,16-27
. "$root/activate.sh"
export KILN_CORPUS="$root" KILN_RESULTS="$root/results"
# The kiln under test, pinned for the whole series.
export KILN_BIN="${KILN_BIN:-$root/bin/kiln-a036408}"
[ -x "$KILN_BIN" ] || { echo "start.sh: no kiln at $KILN_BIN" >&2; exit 1; }
cd "$root/kiln"
mkdir -p "$root/run" "$root/results/logs"
taskset -c "$mask" nice -n 19 go build -o "$root/bin/benchq" ./tools/benchq
lines=0
[ -f "$root/run/events.log" ] && lines=$(wc -l <"$root/run/events.log")
setsid -f taskset -c "$mask" nice -n 19 "$root/bin/benchq" -root "$root" "$@" run </dev/null >>"$root/run/benchq.log" 2>&1
sleep 3
tail -n +"$((lines + 1))" "$root/run/events.log"
