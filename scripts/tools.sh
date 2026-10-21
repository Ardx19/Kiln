#!/bin/sh
# Installs the pinned tool matrix used by the compat harness and corpus
# scripts: several Ninja releases (one per .ninja_log format), the oldest
# and newest CMake we support, and Meson.
#
# usage: scripts/tools.sh [DEST]      (default: .tools)
#
# Layout:
#   DEST/ninja/<version>/ninja
#   DEST/cmake/<version>/bin/cmake
#   DEST/meson/bin/meson            (a Python venv)
#
# Downloads are checked against the SHA-256 sums below. To bump a version,
# change it here and record the new sum from `sha256sum`.
set -eu

DEST=${1:-.tools}

# version  log-format  sha256 of ninja-linux.zip
NINJAS="
1.11.1 v5 b901ba96e486dce377f9a070ed4ef3f79deb45f4ffe2938f8e7ddc69cfb3df77
1.12.1 v6 6f98805688d19672bd699fbbfa2c2cf0fc054ac3df1f0e6a47664d963d530255
1.13.2 v7 5749cbc4e668273514150a80e387a957f933c6ed3f5f11e03fb30955e2bbead6
"
# version  sha256 of cmake-<version>-linux-x86_64.tar.gz
CMAKES="
3.22.6 09e1b34026c406c5bf4d1b053eadb3a8519cb360e37547ebf4b70ab766d94fbc
4.4.4 e5bb807f7728cb60cd8b27ebc97a2edb469b68655f21e844a600c3575b76f5bb
"
MESON_VERSION=1.12.1

if [ "$(uname -m)" != x86_64 ]; then
	echo "tools.sh: only x86_64 is pinned so far" >&2
	exit 1
fi

dl="$DEST/dl"
mkdir -p "$dl"

# fetch URL FILE SHA256: download once, then verify.
fetch() {
	if [ ! -f "$2" ]; then
		curl -fsSL --retry 3 -o "$2.part" "$1"
		mv "$2.part" "$2"
	fi
	echo "$3  $2" | sha256sum -c --quiet - || {
		echo "tools.sh: checksum mismatch for $2" >&2
		exit 1
	}
}

echo "$NINJAS" | while read -r v _ sum; do
	[ -n "$v" ] || continue
	[ -x "$DEST/ninja/$v/ninja" ] && continue
	zip="$dl/ninja-$v-linux.zip"
	fetch "https://github.com/ninja-build/ninja/releases/download/v$v/ninja-linux.zip" "$zip" "$sum"
	mkdir -p "$DEST/ninja/$v"
	unzip -oq "$zip" -d "$DEST/ninja/$v"
	echo "ninja $v -> $DEST/ninja/$v/ninja"
done

echo "$CMAKES" | while read -r v sum; do
	[ -n "$v" ] || continue
	[ -x "$DEST/cmake/$v/bin/cmake" ] && continue
	tgz="$dl/cmake-$v-linux-x86_64.tar.gz"
	fetch "https://github.com/Kitware/CMake/releases/download/v$v/cmake-$v-linux-x86_64.tar.gz" "$tgz" "$sum"
	mkdir -p "$DEST/cmake/$v"
	tar -xzf "$tgz" -C "$DEST/cmake/$v" --strip-components=1
	echo "cmake $v -> $DEST/cmake/$v/bin/cmake"
done

if [ "$("$DEST/meson/bin/meson" --version 2>/dev/null)" != "$MESON_VERSION" ]; then
	python3 -m venv "$DEST/meson"
	"$DEST/meson/bin/pip" install --quiet --disable-pip-version-check "meson==$MESON_VERSION"
	echo "meson $MESON_VERSION -> $DEST/meson/bin/meson"
fi
