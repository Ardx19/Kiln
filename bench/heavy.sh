#!/bin/sh
# Times a project's heaviest compiles from clean on one machine profile:
# hand-tuned Ninja against kiln with no flags, in rounds, one chunk of at
# most -budget per invocation. Run it again to continue the series: runs
# already in the results file are skipped. See docs/benchmarks.md.
#
#   bench/heavy.sh PROJECT PROFILE [tools/bench flags...]
#
#   PROJECT  rdkit, llvm, duckdb (heaviest compiles) or rdkit-full,
#            llvm-full, duckdb-full (the whole build, cold): its corpus tree
#            and the targets in bench/NAME-targets.txt
#   PROFILE  laptop     this machine; a run is stopped when under 600 MiB remain
#            container  the kiln-bench image with 4 CPUs and 6 GiB, no swap
#            server     the shared kiln-bench server, run through
#                       bench/server/start.sh: 24 of its 32 CPUs at nice 19;
#                       a run is stopped when under 48 GiB remain, and the
#                       peak counts the run's own processes (-peak tree)
#
# Flags after PROFILE go to tools/bench and override the defaults
# (-rounds 3 -budget 15m -cap 6m -series rounds), for example
# -tuned 5,6,7, -only ninja-tuned,kiln-first,kiln-learned, -plan or
# -summary (-plan and -summary run on the host, even for the container).
#
# Needs: the corpus tree (corpus/corpus.sh all PROJECT; RDKit with
# RDKIT_CMAKE_ARGS from the conda env), Go, and for the container profile
# the kiln-bench image (docker build -f bench/Dockerfile -t kiln-bench bench).
# The container gets $HOME/.local read-only, at the same path: DuckDB's full
# build runs a uv-installed Python from there in its last steps.
#
# Environment: KILN_CORPUS for the corpus trees (default .work/corpus),
# KILN_BIN for a kiln to time instead of building this checkout's, and
# KILN_RESULTS for the results directory (default bench/results).
set -eu
usage="usage: bench/heavy.sh rdkit|llvm|duckdb|rdkit-full|llvm-full|duckdb-full laptop|container|server [flags...]"
root=$(cd "$(dirname "$0")/.." && pwd)
[ $# -ge 2 ] || { echo "$usage" >&2; exit 2; }
project=$1 profile=$2
shift 2
corpus=${KILN_CORPUS:-.work/corpus}
case $project in
rdkit) dir=$corpus/rdkit/build-kiln name=rdkit-12-heaviest ;;
llvm) dir=$corpus/llvm/build name=llvm-heaviest ;;
duckdb) dir=$corpus/duckdb/build name=duckdb-12-heaviest ;;
rdkit-full) dir=$corpus/rdkit/build-kiln name=rdkit-full ;;
llvm-full) dir=$corpus/llvm/build name=llvm-full ;;
duckdb-full) dir=$corpus/duckdb/build name=duckdb-full ;;
*) echo "$usage" >&2; exit 2 ;;
esac
extra=
case $profile in
laptop) pname=laptop-12threads-11.7g guard=600 ;;
container) pname=container-4cpu-6g guard=1 ;;
server)
	pname=server-24cpu-251g-hdd guard=49152
	extra="-peak tree -series server-full -tuned 20,24 -budget 0"
	# Other people work on this machine: refuse to run without the
	# CPU mask and the low priority that bench/server/start.sh sets.
	if [ "$(nproc)" -gt 24 ] || [ "$(ps -o ni= -p $$)" -lt 10 ]; then
		echo "heavy.sh: the server profile needs at most 24 CPUs and nice >= 10 (run it through bench/server/start.sh)" >&2
		exit 2
	fi
	;;
*) echo "$usage" >&2; exit 2 ;;
esac
host=
for a; do
	case $a in -plan | -summary) host=1 ;; esac
done
cd "$root"
mkdir -p bench/results
go build -o .work/bench ./tools/bench
kiln=${KILN_BIN:-}
if [ -z "$kiln" ]; then
	go build -o .work/kiln ./cmd/kiln
	kiln=$root/.work/kiln
fi
# shellcheck disable=SC2086 # $extra is a list of flags
set -- -dir "$dir" -ninja "$root/.tools/ninja/1.13.2/ninja" -kiln "$kiln" \
	-project "$name" -profile "$pname" -guard "$guard" -out "${KILN_RESULTS:-bench/results}/$name.jsonl" \
	-rounds 3 -budget 15m -cap 6m -series rounds $extra "$@" $(cat "bench/$name-targets.txt")
if [ "$profile" != container ] || [ -n "$host" ]; then
	exec env CCACHE_DISABLE=1 .work/bench "$@"
fi
conda=${CONDA_ENV_ROOT:-$HOME/miniconda3}
exec docker run --rm --user "$(id -u):$(id -g)" --cpus=4 --memory=6g --memory-swap=6g \
	-e HOME=/tmp -e CCACHE_DISABLE=1 \
	-v "$root:$root" -v "$conda:$conda:ro" -v "$HOME/.local:$HOME/.local:ro" -w "$root" kiln-bench \
	.work/bench "$@"
