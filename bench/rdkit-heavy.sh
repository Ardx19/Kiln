#!/bin/sh
# RDKit's 12 slowest translation units (by the cold Ninja build's log) in a
# small-runner container: 4 CPUs and a memory limit. Runs ninja at its
# default -j, ninja at a hand-tuned -j, kiln with no memory history, and
# kiln again with the history the first run left. See docs/benchmarks.md.
#
#   bench/rdkit-heavy.sh [MEMORY] [TUNED_J]     default: 6g 3
#
# Needs: the RDKit corpus tree (corpus/corpus.sh all rdkit, with
# RDKIT_CMAKE_ARGS from the conda env), the kiln-bench image
# (docker build -f bench/Dockerfile -t kiln-bench bench), and Go to build
# the tools.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
mem=${1:-6g}
tuned=${2:-3}
conda=${CONDA_ENV_ROOT:-$HOME/miniconda3}
mkdir -p "$root/bench/results"
go build -o "$root/.work/bench" "$root/tools/bench"
go build -o "$root/.work/kiln" "$root/cmd/kiln"
docker run --rm --user "$(id -u):$(id -g)" --cpus=4 --memory="$mem" --memory-swap="$mem" \
	-e HOME=/tmp -e CCACHE_DISABLE=1 \
	-v "$root:$root" -v "$conda:$conda:ro" -w "$root" kiln-bench \
	.work/bench -dir .work/corpus/rdkit/build-kiln \
	-ninja "$root/.tools/ninja/1.13.2/ninja" -kiln "$root/.work/kiln" -tuned "$tuned" -guard 1 \
	-project rdkit-12-heaviest -profile "container-4cpu-$mem" \
	-out bench/results/rdkit-heavy.jsonl \
	$(cat "$root/bench/rdkit-heavy-targets.txt")
