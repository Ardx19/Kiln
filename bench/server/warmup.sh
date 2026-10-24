#!/bin/sh
# Warm-up build for the server benchmarks: cleans a tree and builds all of
# it once with plain Ninja, outside bench's tables. It proves the
# toolchain before hours of timed runs, gives a first build time to set
# the caps from, and reads the sources into the page cache, so the first
# timed run of a round doesn't pay alone for reading them from the disk.
#
#   bench/server/warmup.sh DIR CAP    DIR under $KILN_CORPUS, CAP for timeout(1)
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
dir=${KILN_CORPUS:?}/$1 cap=$2
ninja=$root/.tools/ninja/1.13.2/ninja
uptime() { cut -d' ' -f1 /proc/uptime; } # monotonic, unlike date(1)
"$ninja" -C "$dir" -t clean >/dev/null
start=$(uptime)
CCACHE_DISABLE=1 timeout "$cap" "$ninja" -C "$dir" all
secs=$(awk -v a="$start" -v b="$(uptime)" 'BEGIN { printf "%.0f", b - a }')
echo "warmup $1: built in ${secs}s, $("$ninja" -C "$dir" -t commands all | wc -l) commands, $(nproc) CPUs"
