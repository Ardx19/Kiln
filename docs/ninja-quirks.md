# Ninja behaviours kiln has to match

Found while building the M0 compatibility harness. Each one is reproduced by `tests/compat` or was read from Ninja's source. M1 must decide, item by item, whether kiln mirrors the behaviour or deliberately improves on it. Improving is only safe when real Ninja, reading the state kiln leaves behind, still agrees nothing needs rebuilding.

## State files differ by version

`.ninja_log` is v5 up to Ninja 1.11, v6 in 1.12, and v7 from 1.13 (command hash switched from MurmurHash64A to rapidhash). Ninja 1.12 and 1.13 delete a log of any other version ("build log version is too old/new; starting over"), so switching Ninja versions rebuilds everything. Sources: `src/build_log.cc` at each tag.

## Ninja 1.12+ records an edge's *start* time

For ordinary edges, v6+ logs the mtime of a lock file created when the edge started, not the output's mtime. Generator and `restat` edges still log the output's real mtime. An input edited while its edge runs is therefore rebuilt next time. Source: commit `a2b5e6d`.

## Ninja < 1.12: restat plus depfile-only inputs can stay dirty

This happens when a `restat` edge with `deps = gcc` reruns and leaves its output unchanged. Ninja ≤ 1.11 then records the newest mtime among the edge's *in-memory* inputs, plus the depfile only when `deps` is empty (`src/build.cc`, v1.11.1, lines 795–815). If the deps log has lost that edge's entry, inputs listed only in the depfile aren't in memory. If one of them is newer than every explicit input, the recorded mtime stays older than it, and the edge (and everything downstream) is dirty on every build until its output actually changes.

The harness reproduces this with "delete `.ninja_deps`, rebuild" on CMake and Meson fixtures, since both put `deps = gcc` on custom commands that have a depfile. It doesn't happen with Ninja 1.12+.

**Option for kiln:** compute the recorded mtime including depfile inputs. Ninja 1.11, reading that log, would then also see the edge as clean.

## CMake logs custom command outputs twice

CMake declares each custom command output under both its relative path and an absolute implicit output (`${cmake_ninja_workdir}…`), so a single run writes two `.ninja_log` entries.

## Generators call Ninja tools on the build dir

CMake runs `ninja -t restat` and `-t recompact` when it regenerates. Meson's regeneration runs `-t restat` and `-t cleandead`, and Meson setup runs `-t compdb`. Drop-in mode (M2) must provide these.

## The environment around Ninja (WSL)

- **The wall clock steps backwards.** See [problems/clock-steps.md](problems/clock-steps.md). Ninja's decisions then depend on timing, and Meson can refuse to configure ("Clock skew detected").
- **Windows `PATH` entries slow down tool lookups.** WSL appends Windows directories (`/mnt/c/...`) to `PATH`. Every lookup there crosses into the Windows filesystem. A Meson regeneration probing clang-tidy names spent about 31 s on 9,200 such lookups. The harness and corpus scripts drop `/mnt/*` from `PATH`. kiln's declared environment (M7) should offer the same.
