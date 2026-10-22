#!/bin/sh
# Times a project's heaviest compiles from clean on one machine profile:
# hand-tuned Ninja against kiln with no flags, in rounds, one chunk of at
# most -budget per invocation. Run it again to continue the series: runs
# already in the results file are skipped. See docs/benchmarks.md.
#
#   bench/heavy.sh PROJECT PROFILE [tools/bench flags...]
#
#   PROJECT  rdkit, llvm or duckdb: its corpus tree and the targets in
#            bench/PROJECT-heavy-targets.txt
#   PROFILE  laptop     this machine; a run is stopped when under 600 MiB remain
#            container  the kiln-bench image with 4 CPUs and 6 GiB, no swap
#
# Flags after PROFILE go to tools/bench and override the defaults
# (-rounds 3 -budget 15m -cap 6m -series rounds), for example
# -tuned 5,6,7, -only ninja-tuned,kiln-first,kiln-learned, -plan or
# -summary (-plan and -summary run on the host, even for the container).
#
# Needs: the corpus tree (corpus/corpus.sh all PROJECT; RDKit with
# RDKIT_CMAKE_ARGS from the conda env), Go, and for the container profile
# the kiln-bench image (docker build -f bench/Dockerfile -t kiln-bench bench).
set -eu
usage="usage: bench/heavy.sh rdkit|llvm|duckdb laptop|container [flags...]"
root=$(cd "$(dirname "$0")/.." && pwd)
[ $# -ge 2 ] || { echo "$usage" >&2; exit 2; }
project=$1 profile=$2
shift 2
case $project in
rdkit) dir=.work/corpus/rdkit/build-kiln name=rdkit-12-heaviest ;;
llvm) dir=.work/corpus/llvm/build name=llvm-heaviest ;;
duckdb) dir=.work/corpus/duckdb/build name=duckdb-12-heaviest ;;
*) echo "$usage" >&2; exit 2 ;;
esac
case $profile in
laptop) pname=laptop-12threads-11.7g guard=600 ;;
container) pname=container-4cpu-6g guard=1 ;;
*) echo "$usage" >&2; exit 2 ;;
esac
host=
for a; do
	case $a in -plan | -summary) host=1 ;; esac
done
cd "$root"
mkdir -p bench/results
go build -o .work/bench ./tools/bench
go build -o .work/kiln ./cmd/kiln
set -- -dir "$dir" -ninja "$root/.tools/ninja/1.13.2/ninja" -kiln "$root/.work/kiln" \
	-project "$name" -profile "$pname" -guard "$guard" -out "bench/results/$project-heavy.jsonl" \
	-rounds 3 -budget 15m -cap 6m -series rounds "$@" $(cat "bench/$project-heavy-targets.txt")
if [ "$profile" = laptop ] || [ -n "$host" ]; then
	exec env CCACHE_DISABLE=1 .work/bench "$@"
fi
conda=${CONDA_ENV_ROOT:-$HOME/miniconda3}
exec docker run --rm --user "$(id -u):$(id -g)" --cpus=4 --memory=6g --memory-swap=6g \
	-e HOME=/tmp -e CCACHE_DISABLE=1 \
	-v "$root:$root" -v "$conda:$conda:ro" -w "$root" kiln-bench \
	.work/bench "$@"
