# kiln

**A Ninja-compatible build runner that knows about memory, environments and shared output.**

kiln runs the `build.ninja` files that CMake, Meson and other generators already write, so there is no new build language to learn. It shares Ninja's state files, so you can switch between `ninja` and `kiln` in any tree with no rebuild. On top of that it keeps parallel jobs within a RAM budget, runs builds detached from your terminal, installs without mixing files from different build trees, applies a declared environment to every command, and manages the compiler cache.

It is aimed at big C and C++ trees on small machines: a laptop, several git worktrees, a CI runner with a tight memory limit. Linux only (including WSL and containers).

> **Status: feature-complete for v0.3; first release v0.1.0.** Everything below is implemented and tested against real Ninja (1.10.1, 1.11.1, 1.12.1 and 1.13.2) on generated projects and on real ones: fmt, GLib, protobuf, DuckDB, RDKit, LLVM and the C++20 modules sample. Cold full builds of all three large projects, and their heaviest compiles, are benchmarked in [Benchmarks](#benchmarks).

## Why

| Problem | Plain Ninja | kiln |
|---|---|---|
| Out-of-memory builds: heavy translation units or parallel links exhaust RAM | You hand-tune `-j` (or `LLVM_PARALLEL_LINK_JOBS`, or DuckDB's `GEN= make`) | **Memory-aware scheduling.** Jobs start only while their predicted peak memory fits a budget, learned from past runs; a job that would be OOM-killed is stopped and retried |
| Long builds tied to a terminal | `setsid nohup ninja … &`, then `pgrep` and `tail` | **Detached builds:** `kiln build --detach`, `status`, `wait`, `log -f`, `stop` |
| Several build trees installing into one prefix | Variants overwrite each other and leave stale files | **Ownership-tracked install:** staged, planned against per-file records, committed through a journal that survives a crash |
| Wrong environment leaking into builds and tests | A stale `~/.local` package or stray `PKG_CONFIG_PATH` is picked up silently | **Declared environment** applied to every command, rebuilding what was built under another one; `kiln test` with an expected-failure list |
| Cache misses across worktrees | ccache misses unless `CCACHE_BASEDIR` and friends are set up | **Managed ccache** settings, optional injection, hit-rate reporting |

## Quick start

```bash
cmake -S . -B build -G Ninja
kiln -C build                      # build, like ninja -C build
```

Use it as Ninja for CMake or Meson (no rebuild when you switch back):

```bash
kiln shim ~/.local/share/kiln/bin                                   # makes a `ninja` symlink to kiln
cmake -S . -B build -G Ninja -DCMAKE_MAKE_PROGRAM=$HOME/.local/share/kiln/bin/ninja
NINJA=$HOME/.local/share/kiln/bin/ninja meson setup build
```

Everyday commands:

```bash
kiln -C build -d explain app       # why does app rebuild?
kiln -C build --mem-budget 6G      # cap the memory jobs may use
kiln -C build build --detach       # keep building after the terminal closes
kiln -C build status               # progress, running jobs, memory
kiln -C build log -f               # follow the output until the build ends
kiln -C build wait                 # block; exits with the build's exit code
kiln -C build install --dry-run    # what an install would change
kiln -C build install --swap       # replace another tree's install of this project
kiln -C build test                 # run the tests in the declared environment
kiln -C build cache stats          # compiler cache size, hit rate, cleanups
```

Configuration lives in `<build>/.kiln/config.toml`, `<source>/.kiln.toml` (shared by every build tree and worktree) and `~/.config/kiln/config.toml`:

```toml
[memory]
budget = "8GiB"            # default: 90% of what's free, or of the container's limit

[env]
unset = ["PYTHONPATH", "CPATH"]
[env.set]
PYTHONNOUSERSITE = "1"

[cache]
base_dir = "/work"         # the common parent of your worktrees
inject   = true            # put ccache in front of compilers nothing launches it for
```

## What it guarantees

- **Same decisions as Ninja.** Dirty checks, command hashes (MurmurHash64A and rapidhash), restat, depfiles, dyndep and response files are ported from Ninja and checked against it. A differential test suite runs the same edit scripts under each Ninja version and under kiln and compares what they rebuild, across CMake (single- and multi-config), Meson and hand-written manifests.
- **Shared state.** `.ninja_log` (v5, v6 and v7) and `.ninja_deps` (v4) are read and written in the tree's own format. After a kiln build `ninja` finds nothing to do, and the reverse. kiln's own state lives in `.kiln/`.
- **Drop-in mode.** Run as `ninja`, kiln is Ninja 1.13.2's command line and messages, including the `-t` tools generators call (`restat`, `recompact`, `cleandead`, `clean`, `compdb`).
- **Never loses a half-installed prefix.** A crash at any point of an install is rolled back to the old state or finished to the new one.

Where kiln deliberately differs from Ninja (and why) is listed in [docs/ninja-quirks.md](docs/ninja-quirks.md).

## Benchmarks

**Machines short of memory.** On a laptop (WSL2, 12 threads, 11.7 GiB) and in a container with 4 CPUs and 6 GiB, Ninja's default `-j` failed every time: it was OOM-killed, or a memory guard stopped it before it could freeze the machine. kiln with no flags finished every run. Against the best hand-tuned `-j` (medians of 3 runs, interleaved with Ninja, unless marked as one run):

| Benchmark | Best hand-tuned Ninja | kiln, first run | kiln, once it has learned |
|---|---|---|---|
| DuckDB's 12 heaviest compiles, container | `-j3`: 322 s (the default, `-j4` and `-j5` are OOM-killed) | 379 s | **300 s** (7% faster) |
| RDKit, whole cold build, container (one run) | `-j4`: 2,608 s | **2,562 s** | **2,513 s** (4% faster) |
| LLVM's 7 heaviest compiles, laptop | `-j4`: 150 s | 161 s | 155.5 s (3% slower) |
| RDKit's 12 heaviest compiles, laptop † | `-j7`: 143 s | **133 s** | **128 s** (10% faster) |
| RDKit, whole cold build, laptop (one run) † | `-j7`: 2,073 s | **1,924 s** | **1,887 s** (9% faster) |

† kiln as of `e0c8f40`, before the two latest scheduler fixes (a default `-j` equal to a container's CPU quota, and memory reserved over time for jobs with history). Those don't change a laptop's `-j`; these rows have not been rerun.

**A big machine.** On 24 CPUs of a shared 32-thread Xeon with 251 GiB (3 runs each; kiln `a036408`, no memory pressure, so speed only): DuckDB's whole build takes 280 s with kiln against 320 s for the best Ninja (13% faster); LLVM's 1,153 s against 1,154 s and RDKit's 486 s against 486 s are level.

| Other checks (laptop) | Ninja 1.13.2 | kiln |
|---|---|---|
| No-op build of a 500k-edge manifest | 3.49 s | 3.14 s |
| No-op build of protobuf (1,945 edges) | 44.5 ms | 57.1 ms |
| Eight 400 MiB compile jobs in a 2 GiB container | `-j8` fails (a compile is killed) | finishes with no flags |
| Second checkout of a project, compiler cache hits | 0 of 23 with plain ccache | 23 of 23 with kiln's settings |

In short: kiln never failed for lack of memory where Ninja's default did, and once it has built a project it matches or beats the best hand-tuned `-j` on all but one benchmark. Its weak spot is a project's first build, before it has any memory history: it is behind the best `-j` on DuckDB's (18%) and LLVM's (7%) heaviest compiles. [docs/benchmarks.md](docs/benchmarks.md) has every run, the limits and how to reproduce them.

## Documentation

- [DESIGN.md](DESIGN.md): architecture, state compatibility, the scheduler and the install protocol.
- [docs/development.md](docs/development.md): building, testing, and how each feature is tested.
- [docs/ninja-quirks.md](docs/ninja-quirks.md): behaviours kiln matches or changes on purpose.
- [docs/problems/](docs/problems/): one write-up per problem, with measurements.
- [docs/plans/](docs/plans/): the implementation plan and progress notes.

## Honest positioning

kiln builds on existing work: Ninja itself, its rewrites ([n2](https://github.com/evmar/n2), [samurai](https://github.com/michaelforney/samurai), Siso), resource-aware schedulers such as Bazel's `--local_resources`, and compiler caches (ccache, sccache). What is different here is narrow: memory cost **learned from past runs** instead of static job counts, and **install ownership** so one prefix can't hold a mix of files from different build trees. The environment and cache features are putting existing ideas in one tool.

## Licence

Apache-2.0, like Ninja, whose code kiln ports. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
