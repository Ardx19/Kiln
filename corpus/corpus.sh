#!/bin/sh
# Fetches, configures and builds kiln's validation corpus with plain Ninja,
# at pinned commits. Later milestones run kiln against the same trees.
#
# usage: corpus/corpus.sh list
#        corpus/corpus.sh fetch|configure|build|all PROJECT...
#
# Environment:
#   KILN_CORPUS        work dir (default .work/corpus); each project gets
#                      $KILN_CORPUS/<project>/{src,build}
#   KILN_TOOLS         tool dir from scripts/tools.sh (default .tools)
#   NINJA CMAKE MESON  tool overrides (default: newest in KILN_TOOLS, else PATH)
#   JOBS               passed to ninja as -j (default: Ninja's own default)
#   BUILD_NAME         build dir name under $KILN_CORPUS/<project> (default
#                      build), e.g. build-kiln for a second tree to build with kiln
#   MODULES_CXX        compiler for cxx-modules (default: clang++-18, g++-14)
#   RDKIT_CMAKE_ARGS   extra configure args for rdkit (it needs Boost and
#                      Python development files, e.g. from a conda env)
#   LLVM_CMAKE_ARGS    extra configure args for llvm
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=${KILN_CORPUS:-$root/.work/corpus}
tools=${KILN_TOOLS:-$root/.tools}

newest() { ls -d $1 2>/dev/null | sort -V | tail -1; }
NINJA=${NINJA:-$(newest "$tools/ninja/*/ninja")}
NINJA=${NINJA:-ninja}
CMAKE=${CMAKE:-$(newest "$tools/cmake/*/bin/cmake")}
CMAKE=${CMAKE:-cmake}
MESON=${MESON:-$tools/meson/bin/meson}
[ -x "$MESON" ] || MESON=meson
export NINJA # Meson finds Ninja through this variable

# Drop WSL's /mnt/* entries from PATH: lookups there cross into the
# Windows filesystem and made a Meson regeneration take 35 s instead of 3.
PATH=$(printf '%s' "$PATH" | tr ':' '\n' | grep -v '^/mnt/' | paste -sd: -)

# name         generator  size    repository                                       tag                commit
PINS="
ninja          configure  small   https://github.com/ninja-build/ninja.git         v1.13.2            3441b633c2fe2c494e958780ba0f4227b1327634
fmt            cmake      small   https://github.com/fmtlib/fmt.git                12.2.0             1be298e1bd68957e4cd352e1f676f00e07dcfb57
fmt-multi      cmake      small   -                                                -                  -
cxx-modules    cmake      tiny    -                                                -                  -
glib           meson      medium  https://gitlab.gnome.org/GNOME/glib.git          2.90.1             e05063ccc6c8f222465a1080927f4c14349f3de6
protobuf       cmake      medium  https://github.com/protocolbuffers/protobuf.git  v36.2              2c74169b34066ceb8ddb6b882fcb3fb32d737a55
duckdb         cmake      medium  https://github.com/duckdb/duckdb.git             v2.0.0             d54cbbe3df17e8e5ce68312f483b4155836112aa
rdkit          cmake      large   https://github.com/rdkit/rdkit.git               Release_2026_09_1  fece8caa860bdf6c9c82bdb9253f22a0cf87150c
llvm           cmake      large   https://github.com/llvm/llvm-project.git         llvmorg-23.1.4     7a941dfb2877a0b8cf30323fefa85ad019e0b0f5
"

die() { echo "corpus: $*" >&2; exit 1; }

pin() { # pin NAME FIELD  (FIELD: 2 generator, 3 size, 4 url, 5 tag, 6 commit)
	echo "$PINS" | awk -v n="$1" -v f="$2" '$1 == n { print $f }'
}

src_dir() {
	case $1 in
	fmt-multi) echo "$work/fmt/src" ;;
	cxx-modules) echo "$root/corpus/samples/cxx-modules" ;;
	*) echo "$work/$1/src" ;;
	esac
}

fetch() {
	case $1 in
	fmt-multi) fetch fmt; return ;;
	cxx-modules) return ;;
	esac
	url=$(pin "$1" 4) tag=$(pin "$1" 5) commit=$(pin "$1" 6) dir=$(src_dir "$1")
	if [ ! -d "$dir/.git" ]; then
		echo "corpus: cloning $1 $tag"
		case $1 in
		protobuf) git clone --quiet --depth 1 --branch "$tag" --recurse-submodules --shallow-submodules "$url" "$dir" ;;
		*) git clone --quiet --depth 1 --branch "$tag" "$url" "$dir" ;;
		esac
	fi
	have=$(git -C "$dir" rev-parse HEAD)
	[ "$have" = "$commit" ] || die "$1 is at $have, expected $commit ($tag)"
}

modules_cxx() {
	for c in ${MODULES_CXX:-} clang++-18 g++-14; do
		command -v "$c" >/dev/null 2>&1 && { echo "$c"; return; }
	done
	die "cxx-modules needs clang++ >= 16 or g++ >= 14 (set MODULES_CXX)"
}

configure() {
	src=$(src_dir "$1") build=$work/$1/${BUILD_NAME:-build}
	mkdir -p "$work/$1"
	cm() { "$CMAKE" -S "$src" -B "$build" -G Ninja -DCMAKE_MAKE_PROGRAM="$NINJA" "$@"; }
	case $1 in
	ninja) mkdir -p "$build" && (cd "$build" && python3 "$src/configure.py") ;;
	fmt) cm -DCMAKE_BUILD_TYPE=Release -DFMT_TEST=ON ;;
	fmt-multi) "$CMAKE" -S "$src" -B "$build" -G "Ninja Multi-Config" -DCMAKE_MAKE_PROGRAM="$NINJA" -DFMT_TEST=ON ;;
	cxx-modules) cm -DCMAKE_CXX_COMPILER="$(modules_cxx)" ;;
	glib) "$MESON" setup "$build" "$src" --wrap-mode=default ;;
	protobuf) cm -DCMAKE_BUILD_TYPE=Release -Dprotobuf_BUILD_TESTS=ON ;;
	# shellcheck disable=SC2086 # RDKIT_CMAKE_ARGS is a list of arguments
	rdkit) cm -DCMAKE_BUILD_TYPE=Release -DRDK_INSTALL_INTREE=OFF -DCMAKE_INSTALL_PREFIX="$work/rdkit/install" ${RDKIT_CMAKE_ARGS:-} ;;
	duckdb) cm -DCMAKE_BUILD_TYPE=Release ;;
	# Slim: Release without debug info or test suites keeps the tree near
	# 10 GB while keeping LLVM's many large links (the LLVM_PARALLEL_LINK_JOBS
	# case). LLVM_CMAKE_ARGS adds to it, e.g. -DLLVM_PARALLEL_LINK_JOBS=2.
	# shellcheck disable=SC2086 # LLVM_CMAKE_ARGS is a list of arguments
	llvm) "$CMAKE" -S "$src/llvm" -B "$build" -G Ninja -DCMAKE_MAKE_PROGRAM="$NINJA" -DCMAKE_BUILD_TYPE=Release \
		-DLLVM_ENABLE_PROJECTS=clang -DLLVM_ENABLE_ASSERTIONS=ON -DLLVM_TARGETS_TO_BUILD=X86 \
		-DLLVM_INCLUDE_TESTS=OFF -DLLVM_INCLUDE_BENCHMARKS=OFF -DLLVM_INCLUDE_EXAMPLES=OFF \
		-DLLVM_INCLUDE_DOCS=OFF -DCLANG_INCLUDE_TESTS=OFF -DCLANG_INCLUDE_DOCS=OFF ${LLVM_CMAKE_ARGS:-} ;;
	*) die "unknown project $1" ;;
	esac
}

uptime() { cut -d' ' -f1 /proc/uptime; } # monotonic, unlike date(1) when the clock steps

build() {
	build=$work/$1/${BUILD_NAME:-build}
	start=$(uptime)
	"$NINJA" -C "$build" ${JOBS:+-j"$JOBS"}
	# Multi-config trees also have one manifest per configuration.
	[ "$1" != fmt-multi ] || "$NINJA" -C "$build" -f build-Release.ninja ${JOBS:+-j"$JOBS"}
	edges=$("$NINJA" -C "$build" -t commands | wc -l)
	secs=$(awk -v a="$start" -v b="$(uptime)" 'BEGIN { printf "%.0f", b - a }')
	echo "corpus: $1 built in ${secs}s, $edges commands, ninja $("$NINJA" --version)"
	echo "corpus: $1 uses $(du -sh "$(src_dir "$1")" | cut -f1) of source and $(du -sh "$build" | cut -f1) of build"
}

[ $# -ge 1 ] || die "usage: corpus.sh list | fetch|configure|build|all PROJECT..."
action=$1
shift
if [ "$action" = list ]; then
	echo "$PINS" | awk 'NF { printf "%-12s %-10s %-7s %s\n", $1, $2, $3, $5 }'
	exit 0
fi
[ $# -ge 1 ] || die "name at least one project (see: corpus.sh list)"
for p in "$@"; do
	[ -n "$(pin "$p" 1)" ] || die "unknown project $p (see: corpus.sh list)"
	case $action in
	fetch) fetch "$p" ;;
	configure) configure "$p" ;;
	build) build "$p" ;;
	all) fetch "$p" && configure "$p" && build "$p" ;;
	*) die "unknown action $action" ;;
	esac
done
