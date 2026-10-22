#!/bin/sh
# RDKit's 12 slowest translation units (by the cold Ninja build's log):
# bench/heavy.sh for RDKit. See there and docs/benchmarks.md.
#
#   bench/rdkit-heavy.sh laptop|container [tools/bench flags...]
exec "$(dirname "$0")/heavy.sh" rdkit "$@"
