#!/bin/sh
# Watches the server's benchmark queue from the laptop. Every 2 minutes it
# reads run/events.log and run/heartbeat on kiln-bench over one shared SSH
# connection, and exits, printing the new events, as soon as one needs
# attention (FAIL HALT ALERT DONE STOPPED FINISHED REFUSED); when the
# heartbeat is over 5 minutes old while the queue should be running; or
# after 15 minutes without reaching the server. Run it in the background
# and handle what it prints. --once prints the queue's status and the new
# events, and exits.
#
#   bench/server/watch.sh [--once]
set -eu
host=${KILN_BENCH_HOST:-kiln-bench}
root=${KILN_BENCH_ROOT:-/home/swarnavas/Work/PhD_Work/Covaln_Dev_work/testing_btp/kiln-bench}
mode=${1:-}
here=$(cd "$(dirname "$0")/../.." && pwd)
cursor=$here/.work/server-watch.cursor
mkdir -p "$here/.work"
seen=$(cat "$cursor" 2>/dev/null || echo 0)
remote() {
	ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 \
		-o ControlMaster=auto -o ControlPath="$HOME/.ssh/cm-%r@%h:%p" -o ControlPersist=30m "$host" "$@"
}
# poll prints the events after line $seen, then "@@ AGE LAST TOTAL": the
# heartbeat's age in seconds, the last event's level and the log's lines.
poll() {
	remote "f=$root/run/events.log; tail -n +$((seen + 1)) \$f 2>/dev/null; \
		hb=\$(stat -c %Y $root/run/heartbeat 2>/dev/null || echo 0); \
		last=\$(tail -n 1 \$f 2>/dev/null | cut -d' ' -f2); \
		echo \"@@ \$((\$(date +%s) - hb)) \${last:-none} \$(cat \$f 2>/dev/null | wc -l)\""
}
[ "$mode" != --once ] || remote "$root/bin/benchq -root $root status" || true
down=0
while :; do
	if ! out=$(poll 2>&1) || ! printf '%s\n' "$out" | grep -q '^@@ '; then
		down=$((down + 1))
		[ $down -lt 8 ] || { echo "watch: can't reach $host for 15 minutes: $out"; exit 3; }
		sleep 120
		continue
	fi
	down=0
	# shellcheck disable=SC2046 # split the @@ line into its fields
	set -- $(printf '%s\n' "$out" | grep '^@@ ' | tail -n 1)
	age=$2 last=$3 total=$4
	if [ "$total" -lt "$seen" ]; then # the log was archived: start over
		seen=0
		continue
	fi
	new=$(printf '%s\n' "$out" | grep -v '^@@ ' || true)
	if [ -n "$new" ]; then
		seen=$((seen + $(printf '%s\n' "$new" | wc -l)))
		echo "$seen" >"$cursor"
	fi
	if [ "$mode" = --once ]; then
		printf '%s\n' "$new"
		exit 0
	fi
	if printf '%s\n' "$new" | grep -qE '^[^ ]+ (FAIL|HALT|ALERT|DONE|STOPPED|FINISHED|REFUSED) '; then
		printf '%s\n' "$new"
		exit 0
	fi
	case $last in
	FINISHED | HALT | STOPPED | REFUSED | none)
		echo "watch: the queue is not running (last event: $last)"
		exit 0
		;;
	esac
	if [ "$age" -gt 300 ]; then
		echo "watch: the heartbeat is ${age}s old (last event: $last); is benchq alive?"
		exit 2
	fi
	sleep 120
done
