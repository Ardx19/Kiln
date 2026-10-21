#!/bin/sh
# M0 detach spike: does a setsid'd background process (what `kiln build
# --detach` will start) survive the ways a session can end?
#
# usage: spikes/detach/heartbeat.sh start [MINUTES]   (default 60, then it exits by itself)
#        spikes/detach/heartbeat.sh check
#        spikes/detach/heartbeat.sh stop
#        spikes/detach/heartbeat.sh hangup-test     (automated: closes a pseudo-terminal)
#
# The heartbeat appends the time to out/beats every 5 s. `check` reports
# whether it's alive and the largest gap between beats; a gap well over 5 s
# means it was frozen or killed (for example, WSL stopped the distro).
set -eu

dir=$(cd "$(dirname "$0")" && pwd)/out
mkdir -p "$dir"

start() {
	minutes=${1:-60}
	: > "$dir/beats"
	setsid sh -c '
		end=$(( $(date +%s) + 60 * '"$minutes"' ))
		while [ "$(date +%s)" -lt "$end" ]; do date +%s >> "'"$dir/beats"'"; sleep 5; done
	' < /dev/null > /dev/null 2>&1 &
	echo $! > "$dir/pid"
	echo "heartbeat $(cat "$dir/pid") started for $minutes min"
}

check() {
	pid=$(cat "$dir/pid" 2>/dev/null || true)
	if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then state=alive; else state=gone; fi
	awk -v state="$state" -v now="$(date +%s)" '
		NR > 1 && $1 - prev > gap { gap = $1 - prev; at = prev }
		{ prev = $1; n++ }
		END {
			printf "heartbeat %s: %d beats, last %ds ago, largest gap %ds", state, n, now - prev, gap
			if (gap > 15) printf " (starting %s)", strftime("%H:%M:%S", at)
			print ""
		}' "$dir/beats"
}

stop() {
	pid=$(cat "$dir/pid" 2>/dev/null || true)
	[ -n "$pid" ] && kill "$pid" 2>/dev/null && echo "stopped $pid" || echo "not running"
}

# hangup_test starts a stand-in login session, launches the heartbeat and
# an ordinary background job from it, then sends SIGHUP to every process in
# that session, which is what a closing terminal or SSH teardown amounts
# to. The ordinary job should die; the setsid'd heartbeat should not.
hangup_test() {
	setsid sh -c '"$0" start 2; sleep 300 & echo $! > "$1"; sleep 30' "$0" "$dir/control.pid" \
		< /dev/null > /dev/null 2>&1 &
	sleep 2
	control=$(cat "$dir/control.pid")
	pkill -HUP -s "$(ps -o sid= -p "$control" | tr -d ' ')"
	sleep 2
	if kill -0 "$control" 2>/dev/null; then echo "control job $control: still running"; kill "$control"; else echo "control job $control: killed by the hangup"; fi
	check
	stop > /dev/null
}

case ${1:-} in
start) start "${2:-}" ;;
check) check ;;
stop) stop ;;
hangup-test) hangup_test ;;
*) echo "usage: $0 start [MINUTES] | check | stop | hangup-test" >&2; exit 2 ;;
esac
