---
title: "kiln: Ninja-compatible build runner — implementation plan"
type: feat
status: active
date: 2026-10-21
updated: 2026-10-22 (all milestones through M8 built; benchmarks and the v0.1.0 tag remain)
scope: ../../project_scope.md.txt
---

# kiln — implementation plan

## Summary

kiln is a runner for the Ninja manifests of C and C++ projects in general. That covers CMake (single- and multi-config, including C++20 modules), Meson, and hand-written or other generated `build.ninja` files, in any directory layout. RDKit is the case that motivated kiln and one of several validation projects. It is not the target.

The plan has nine milestones plus a stretch list:

- **v0.1 (M0–M5):** a native engine that shares state with whichever Ninja version a tree uses, drop-in use from `cmake --build` and `meson compile`, memory-aware scheduling, detached runs, and benchmarks on several projects.
- **v0.2 (M6–M7):** transactional install, the declared environment and `kiln test`, for CMake and Meson trees.
- **v0.3 (M8):** the shared cache.

It targets Linux only, including WSL and containers.

**Where it stands (2026-10-22).** M0-M4 and M6-M8 are built and tested, and so is everything in M5 except the benchmark runs and the `v0.1.0` tag. See each milestone's *Progress* note below, [DESIGN.md](../../DESIGN.md) and [the overnight log](2026-10-22-overnight-execution-plan.md). The README no longer marks features as planned.

## What a general tool has to handle

These findings come from Ninja's source (v1.10.1, v1.11.1, v1.12.1, v1.13.1), CMake 3.22's binary, this machine, and the local RDKit trees.

| Area | Finding | Consequence |
|---|---|---|
| Ninja state versions | `.ninja_log` comes in three versions:<br>• **v5** up to Ninja 1.11: MurmurHash64A, records the output's mtime after the command.<br>• **v6** in 1.12: records the mtime of a lock file taken when the edge *started* (generator and `restat` rules excepted), so an input edited mid-build triggers a rebuild.<br>• **v7** from 1.13: command hash switched to rapidhash.<br>1.12 and 1.13 each discard a log of any other version ("starting over"), which rebuilds everything. `.ninja_deps` is v4 throughout | kiln must speak all three log versions, each with its own hash and mtime rules, and write back the version the tree already has |
| Generators | CMake runs `ninja -t restat` and `-t recompact` on the build dir when it (re)generates. Meson locates Ninja through the `NINJA` variable and runs `ninja -t compdb` during setup. CMake's C++20 modules need Ninja ≥ 1.11 `dyndep`. CMake and Meson both emit `install` and `test` targets | The engine needs `dyndep`, `subninja`, `rspfile`, pools and `builddir`. Drop-in use needs those `-t` tools. kiln's subcommand names clash with real targets |
| Machines | Laptops (often SMT with little RAM), WSL, and CI runners in containers. Inside a container, `/proc/meminfo` shows the **host's** memory; the real limit is the cgroup's. Some kernels have no cgroup v2 `memory.peak` and no PSI; this WSL 5.15 kernel has neither | Measure memory from `/proc` alone. Budget and pressure use the tighter of host memory and kiln's own cgroup limit. Default `-j` respects CPU affinity and cgroup CPU quota |
| Directory layouts | Build dirs can be in-source, siblings, elsewhere, or several per source tree or git worktree. Manifests may have other names (`-f`). `builddir` moves the state files. Paths can contain spaces. CI source trees may be read-only. Some filesystems are slow or coarse (`/mnt/c` on WSL, NFS) | kiln assumes no layout. It writes only to the build dir, and to the install prefix during install. It warns on known-bad filesystems |
| Install prefixes | Prefixes come in four kinds:<br>• dedicated (`~/opt/proj`)<br>• shared with other packages (`/usr/local`, a conda env, `~/.local`)<br>• in-tree, next to tracked sources (RDKit's `RDK_INSTALL_INTREE`)<br>• root-owned | Ownership must be **per file**, with rules that tell a variant of the same project apart from a different package or an unowned file. The commit is per file, with a journal |
| Caching | ccache only helps when compile commands go through a launcher. Some projects set one up (`CMAKE_<LANG>_COMPILER_LAUNCHER`; Meson picks up ccache automatically), many don't | Manage ccache's settings, and optionally inject the launcher |

**The motivating case, as one data point.** On this machine (Ryzen 5 5500U, 6 cores and 12 threads, 11.7 GiB RAM, WSL2), each RDKit tree has about 5,600 edges and about 10 job-hours per cold build, with single compiles up to 4 minutes. Three lessons generalize:

- **The speed gain has a ceiling.** On SMT machines the hand-tuned `-j` is usually the physical core count, so memory-aware scheduling can only add SMT throughput (tens of percent). The headline is **no OOM without hand-tuning**.
- **Variant trees really do leave files behind.** RDKit's Boost and nanobind trees share 286 installed paths. Switching between them leaves 66 or 57 stale files, and 142 of the paths are symlinks.
- **Big projects already hand-tune memory.** LLVM documents `LLVM_PARALLEL_LINK_JOBS` for machines that run out of memory while linking, which is the same problem in another project.

## Decisions

These are recommendations. Confirm or override them before starting M0. The rest of the plan assumes them.

- **D1. Language: Go.** It's installed (1.25.1) and you already use it (`agent_eval_harness`). Siso, Chromium's Ninja-compatible runner, is written in Go. `golang.org/x/sys/unix` covers `Wait4`, `Setpgid`/`Setsid`, `Flock` and `Renameat2`, and the output is a single static binary for CI runners. Keep dependencies to the stdlib, `x/sys` and one TOML parser.
  *Alternatives:* Rust (n2 is a good reference, but needs a toolchain install), or a C++ fork of Ninja (the quickest way to exact compatibility, but its design fights the daemon, install and test features).
- **D2. Share Ninja's state files, in every version.**
  - Read `.ninja_log` v5, v6 and v7 and `.ninja_deps` v4. Write back the version found, using that version's hash (MurmurHash64A or rapidhash) and mtime rule.
  - For a tree with no log yet, use the version of the Ninja the tree is configured with: `CMAKE_MAKE_PROGRAM`, then Meson's `NINJA`, then `ninja` on `PATH`. Default to v7.
  - Honour `builddir` for where the files live. Never write a version kiln doesn't know; fall back to private state with a warning instead.
  - Why: switching between `ninja` and `kiln` must cost nothing in any tree, generators run real Ninja tools against these files, and `ninja -t …` keeps working without kiln reimplementing it.
- **D3. Linux only**, including WSL and containers. macOS and Windows aren't planned.
- **D4. Two support tiers.**
  - **Tier 1, full support:** CMake (`Ninja` and `Ninja Multi-Config`, including C++20 modules) and Meson. This covers the engine and the install and test integration.
  - **Tier 2, engine only:** any other Ninja manifest (hand-written, GN and others). Build, memory scheduling, detach and cache all work. `kiln install` and `kiln test` need a configured command; for install, that command must honour `DESTDIR`.
- **D5. Shared cache = managed ccache, with optional launcher injection.**
  - kiln can prepend `ccache` at run time to compile edges that call a known compiler directly (gcc, g++, cc, c++, clang, clang++ and cross variants) and have no launcher. The Ninja command hash stays computed on the manifest's own command, so the state remains valid for Ninja.
  - sccache comes later. A native content-addressed cache waits for traced execution (stretch), because caching arbitrary edges safely needs complete input knowledge.
- **D6. Install = per-file ownership with a journaled commit.** Stage with `DESTDIR` through `cmake --install` or `meson install`, then commit through a journal. M6 has the full rules.
- **D7. Licence: Apache-2.0** (decided 2026-10-21). It matches Ninja, whose code kiln ports. `NOTICE` credits Ninja, plus rapidhash (BSD-2-Clause) and MurmurHash64A (public domain).

## Scope notes

These are small adjustments to the scope, proposed here so they don't surprise anyone later:

1. **README status markers.** See the Summary.
2. **Generalize the README.** "kiln runs the `build.ninja` files that CMake already generates" should become "CMake, Meson, or any Ninja manifest". Take the "Why" examples and the benchmarks from more than one project, for instance LLVM's link-memory problem next to RDKit's heavy compiles, so the README doesn't read as an RDKit tool.
3. **Name clash.** CMake and Meson trees both have `install` and `test` targets. Reserved subcommand names win, and `kiln build <target>` builds a target with that name.
4. **Additions.**
   - `kiln stop`
   - `kiln exec -- <cmd>`, to configure under the declared environment (`cmake`, `meson setup`, `./configure`)
   - `status --json` and `log --failures`
   - drop-in mode (M2)
5. **Benchmarks.** Report per project and per machine profile, with separate *first run* and *learned* rows.
6. **Cache wording** follows D5.

## Architecture

```
cmd/kiln/            argv parsing (Ninja flags -C -f -j8 -k0 -l -n -v -d -t), subcommand dispatch
internal/manifest/   lexer, parser, scopes (include/subninja), evaluation, path canonicalisation
internal/graph/      node/edge arena, pools, phony, validations, dyndep, cycle detection
internal/state/      .ninja_log v5/v6/v7 (MurmurHash64A, rapidhash), .ninja_deps v4, recompaction
internal/dirty/      Ninja's dirty rules for each state version + explain reasons
internal/plan/       wanted set, ready queue, critical-path priority
internal/sched/      admission: pools, -j ceiling, --min-jobs floor, memory budget, brake
internal/exec/       /bin/sh -c in own process group, output capture, rspfile, depfile, restat
internal/memprobe/   /proc sampler, per-job peak, host + cgroup memory, OOM counters
internal/memdb/      .kiln/mem.log history + predictor
internal/tools/      -t restat/recompact/compdb natively; other -t passed to a real ninja
internal/generator/  detect CMake / Meson / other; project name, install + test backends
internal/run/        lock, run dirs, daemon (re-exec + setsid), status.json, log follow
internal/install/    staging, ownership records, journal                (M6)
internal/envdecl/    declared environment, per-edge env hash            (M7)
internal/testrun/    CTest / meson test / custom runner, JUnit, xfail   (M7)
internal/cache/      ccache settings, launcher injection, stats         (M8)
tools/genfixture/    synthetic project generator (CMake, Meson, raw ninja)
tests/compat/        differential suite against several Ninja versions
bench/               benchmark scripts and results
```

**On-disk layout**

```
<build dir>/                       anywhere, any name
  build.ninja (or -f <file>)       the generator's; kiln only reads it
  .ninja_log, .ninja_deps          shared with ninja (under $builddir if set)
  .kiln/
    lock                           flock held by whoever is building (also the liveness signal)
    mem.log, env.log               per-output memory history and env hash
    config.toml, xfail.txt         tree settings (M7)
    tree-id                        UUID identifying this tree (M6)
    runs/<id>/{status.json,output.log,failures/}, runs/latest
<install prefix>/
  .kiln/owners.json, owners.lock   per-file ownership records (M6)
  .kiln/txn/<id>/{stage/,backup/,journal.json}
~/.config/kiln/config.toml         machine defaults: budget, cache dir
```

**One build, end to end**

1. Take `.kiln/lock`. If another build holds it, fail fast and name its run id.
2. Load the manifest. If the manifest is itself out of date, build it first, reload the manifest and the state, and repeat with a bound on the number of tries.
3. Load the state files in their own version, then compute the dirty set for the requested targets.
4. Admit ready edges in critical-path order while there's a pool slot, `-j` headroom and memory budget.
5. Run each edge with `/bin/sh -c` in its own process group (console-pool edges get the terminal) while the sampler tracks its process tree.
6. When an edge finishes:
   - ingest the depfile or dyndep file
   - restat its outputs
   - append to the logs
   - release its reservation
   - update the status
7. On failure, stop admitting (unless `-k`), drain the running jobs and exit non-zero. On SIGINT, kill the job process groups and flush the logs.

## Validation corpus

Built in M0 and used by every milestone after it.

| Project | Focus | Size | Runs in |
|---|---|---|---|
| `genfixture` output | CMake, Meson and raw-ninja fixtures covering every feature, plus random edit scripts | tiny | CI, every push |
| Ninja's own repo (`configure.py`) | A non-CMake, hand-written-style manifest | small | CI |
| fmt | CMake, single- and multi-config | small | CI |
| C++20 modules sample | CMake ≥ 3.28, Ninja ≥ 1.11 `dyndep`, Clang ≥ 16 or GCC ≥ 14 | tiny | CI container |
| GLib 2.90.1 | Meson, code generation, `meson install` and `meson test` | medium | nightly |
| protobuf v36.2 | CMake, heavy code generation, many targets | medium | nightly |
| DuckDB v2.0.0 | CMake, heavy unity-build compiles. Its own docs say Ninja runs out of memory on small machines | medium | local |
| RDKit `Release_2026_09_1` (kiln's own copy) | CMake, compile-heavy translation units, in-tree install, Python | large | local |
| LLVM + Clang 23.1.4, slim | CMake, many large links (the `LLVM_PARALLEL_LINK_JOBS` case). Release, X86, no test suites, to stay near 10 GB | large | local |

**Your 11 RDKit trees** are used only for read-only checks (`-n` with a version-matched Ninja). The large builds use kiln's own copies under `.work/corpus`, about 15 GB in total, with deps from the `rdkit-dev` conda env.

**Tool matrix:**

- Ninja 1.11 (v5), 1.12 (v6) and 1.13 (v7), pinned with release binaries or the PyPI `ninja` wheels
- CMake 3.22 (local) and the newest release
- current Meson
- GCC and Clang

## Milestones

Rough sizes for one developer: **S** ≤ 1 week, **M** ≈ 2 weeks, **L** ≈ 4+ weeks.

| Milestone | Size | Depends on | Release |
|---|---|---|---|
| M0 Foundations and corpus | M | — | — |
| M1 Core engine | L | M0 | v0.1 |
| M2 Drop-in integration | S | M1 | v0.1 |
| M3 Memory-aware scheduling | M–L | M1 | v0.1 |
| M4 Detached runs | S–M | M1 | v0.1 |
| M5 Benchmarks, docs, release | M | M2–M4 | v0.1 |
| M6 Transactional install | M–L | M1 | v0.2 |
| M7 Declared environment + `kiln test` | M | M1 | v0.2 |
| M8 Shared cache | S–M | M7 | v0.3 |
| M9 Stretch | — | — | — |

M2, M3 and M4 depend only on M1, so they can run in parallel, for example in separate worktrees or by separate agents.

### Roadmap to v0.3 (agreed 2026-10-21, after M1e)

The finish line is **v0.3** (M1–M8). M9 stays a prioritized backlog.

| Phase | Work | Machine time | Release |
|---|---|---|---|
| A | Licence, M1 close-out (DuckDB, RDKit and slim LLVM built with Ninja and kiln), M0 leftovers, memory-spike follow-up | ~1.5 days, unattended | — |
| B | M2 drop-in integration | small | — |
| C | M3 memory-aware scheduling | ~1 day | — |
| D | M4 detached runs | small | — |
| E | M5 benchmarks (reduced matrix), docs | ~2 days | v0.1.0 |
| F | M6 transactional install | ~0.5 day | — |
| G | M7 declared environment and `kiln test` | small | v0.2.0 |
| H | M8 shared cache | ~0.5 day | v0.3.0 |

**Why this order:**
- M2 lets `cmake --build` and `meson compile` drive kiln in every later test.
- M4's status reports per-job memory, so it follows M3.
- M6 reuses M2's generator detection, and M7 extends M3's config.

**Reduced benchmark matrix (M5):**
- **Profiles:** the laptop runs RDKit and slim LLVM, and a 4-CPU/8 GiB container runs RDKit and DuckDB.
- **Runners**, for each: Ninja at its default `-j`, the best hand-tuned setting, kiln's first run and kiln's learned run.
- **DuckDB** adds a row for its documented workaround, `GEN= make`.

### M0 — Foundations and corpus (M)

**Tasks**
- **Repo and CI.** `git init`, a Go module, and CI running `gofmt`, `go vet`, `staticcheck` and `go test -race ./...`, in container images that carry the tool matrix.
- **`tools/genfixture`.** Emits CMake, Meson and raw-ninja projects covering:
  - static, shared and module libraries and executables, with a random header DAG
  - code generation with `restat` and depfiles
  - response files, custom pools and the console pool
  - phony targets, `default` and order-only deps
  - `include`/`subninja` and `builddir`
  - an install rule, plus a variant B that installs a partly different file set
  - tests with known pass and fail results
- **`tests/compat` harness.**
  - Runs an edit script under a pinned Ninja and under kiln, starting from the same snapshot.
  - Restores between runs with `cp -a` at the **same absolute path**, because generators bake absolute paths into trees.
  - Compares two things: the dry-run edge sets, and the edges actually run (the new log lines).
  - Self-test: Ninja against Ninja shows zero differences, for every Ninja version.
- **Corpus scripts.** Fetch, configure (pinned commits and options) and build each corpus project with each relevant tool combination.
- **Memory spike (½ day).** Measure peak RSS of the 8–20 slowest compiles in a large tree (RDKit is on hand), twice each, with `CCACHE_DISABLE=1` and outputs sent to scratch paths. This confirms that peak RSS is stable from run to run and sizes the default estimate. Add one big link, such as clang, once an LLVM tree exists.
- **Detach spike (1 h).** Check whether a `setsid` process survives:
  - closing every WSL terminal
  - logging out on a systemd host with `KillUserProcesses=yes`

  The result picks the approach: `setsid` alone, `systemd-run --user --scope`, or documenting `loginctl enable-linger`.

**Acceptance** (status as of 2026-10-21)
- [x] CI is green on GitHub, for every push from M0 to M1e.
  - The image carries Ninja 1.11.1, 1.12.1 and 1.13.2, CMake 3.22.6 and 4.4.4, Meson 1.12.1, GCC 14 and Clang 18.
  - `lint`, `test` and `compat` all pass inside it.
- [x] genfixture's CMake, Meson and raw-ninja outputs build under every Ninja in the matrix, plus the system Ninja 1.10.1.
- [x] The harness self-test passes for every Ninja version. Clock-step retries are needed on WSL ([problems/clock-steps.md](../problems/clock-steps.md)), and one Ninja < 1.12 behaviour is logged as a known quirk ([ninja-quirks.md](../ninja-quirks.md)).
- [ ] Every corpus project configures and builds with plain Ninja from the scripts. *Partly done.*
  - Built: `ninja`, `fmt`, `fmt-multi`, GLib 2.90.1 and protobuf v36.2 (locally), and `cxx-modules` (in the CI image).
  - Pins verified but not built yet (hours each): `rdkit` and `llvm`.
- [ ] Spike results are written up in `docs/problems/`. *Nearly done.*
  - Memory: [oom.md](../problems/oom.md).
  - Detach: [detached.md](../problems/detached.md). The manual "close every WSL window" check is still open.

### M1 — Core engine (L)

**Progress (2026-10-21).** M1a is done: tasks 1–2 and the graph construction part of task 3.

- **Code.** `internal/manifest` holds the lexer and parser, ported statement by statement from Ninja 1.13.2. `internal/graph` holds nodes, edges, scopes, evaluation, path canonicalisation and shell escaping.
- **Checked against Ninja** by `tests/compat` (`TestManifestCases`, `TestRealManifests`):
  - **55 tricky manifests** match Ninja exactly. On the 27 Ninja accepts, kiln lists the same outputs and rules and evaluates the same commands. The other 28, both tools reject with the same first line of the error message.
  - **49 real trees:** all 11 local RDKit trees, the corpus (fmt, fmt multi-config, GLib, protobuf, ninja, C++20 modules) and the compat fixtures. kiln lists the same outputs with the same rules in the same order, and evaluates the same command for every non-phony edge, about 33,700 in total.
  - **Mutation check:** three injected bugs (edge-binding scope, shell escaping, path canonicalisation) are each caught.
- **Speed.** The largest RDKit manifest (5,636 edges) loads in about 40 ms. Ninja's whole `-t targets all` process takes 42–47 ms on the same tree.
- **Still to do in M1:** `dyndep` loading (the parser records the binding and checks it's an input), and the LLVM tree, which hasn't been built yet.

**M1b is done:** the reading half of task 4.

- **Code.** `internal/state` reads `.ninja_log` v5/v6/v7 and `.ninja_deps` v4, and hashes commands with MurmurHash64A (v5/v6) or rapidhash (v7).
  - An unknown version is reported, never deleted. A corrupt deps tail is reported, never truncated.
  - Writing the logs comes with the executor (M1d).
- **Checked against Ninja:**
  - **Hash vectors.** 50 command lengths, each hashed both ways, match what Ninja 1.12 and 1.13 wrote.
  - **Real trees.** `TestRealState` covers 48 trees: the 11 RDKit trees (v5), the corpus (v7) and the compat fixtures (v5, v6 and v7).
  - **Hashes.** About 14,300 log entries of edges Ninja calls clean have exactly kiln's command hash.
  - **Deps.** For about 11,800 outputs, kiln's reading of `.ninja_deps` reproduces `ninja -t deps` byte for byte.
- **Safety.** Ninja runs only with `-n`, only with a Ninja that writes the tree's log version, and only after kiln has checked the deps log for corruption. `.ninja_log` and `.ninja_deps` are fingerprinted before and after each check, and no check changed them.

**M1c is done:** task 5, the planning half of task 3 (dyndep) and a dry-run-only task 6, plus `kiln -n`.

- **Code.**
  - `internal/build` ports Ninja's dependency scan, deps-log/depfile/dyndep loading, plan and dry-run build loop.
  - `internal/depfile` parses depfiles in the 1.10, 1.12 and 1.13 dialects, chosen from the tree's log version.
  - `cmd/kiln` parses Ninja's command line and does `-n [-v] [-d explain]`, including the "regenerate the manifest first" step.
- **Checked against Ninja** (`TestKilnDryRun`, `TestRealDryRun`, `TestDepfileCases`):
  - **Every compat script step.** `kiln -n` and `ninja -n` choose the same commands before and after each real build. That's 16 scripts (4 Ninja versions × 4 fixture types, including a dyndep chain), 384 steps and 2,322 commands.
  - **64 real trees** agree on 8,504 commands to run, 8,187 of them across the 11 RDKit trees.
  - **The C++20 modules tree in the CI image** agrees both dirty (8 commands, with dyndep files regenerated and reloaded mid-plan) and clean.
  - **Depfiles.** 25 tricky depfiles parse as each Ninja version parses them.
- **Still to do in M1:** running commands (M1d), the clock-step warning, critical-path order and the explain/status polish (M1e).

**M1d is done:** the rest of task 4, and tasks 6–7.

- **kiln builds for real.**
  - Commands run in their own process groups. Response files and depfiles are handled, restat pruning works, and pools are respected.
  - `-j` and `-k` work. Interrupts kill the running process groups and clean up half-written outputs.
  - Manifest regeneration restarts the build, closing the log first so the generator can run `ninja -t restat`.
- **It writes `.ninja_log` and `.ninja_deps` like the Ninja that owns the tree** (`build.Profile`): log format and hash, which mtime is recorded (v5 output mtimes, v6+ lock-file start times), exit codes, and depfile dialect. A tree with no log yet gets the format of its configured Ninja.
- **Checked against Ninja:**
  - **`TestKilnBuild`:** kiln as runner B, 16 scripts (4 Ninja versions × 4 fixture types), all 384 steps. kiln runs the same commands as Ninja with the same exit codes and log version. Ninja, dry-running kiln's tree before and after every kiln build, sees exactly what kiln sees, so the tool-switch invariant holds at every step.
  - **`TestKilnInterruptResume`:** SIGINT mid-build, then a resumed build. Neither tool sees leftover work, in all 16 combinations.
  - **Corpus builds from scratch with kiln:**
    - fmt: all 21 tests pass, and Ninja finds nothing to do.
    - Ninja's own sources: the resulting binary works.
    - GLib: Ninja finds nothing to do.
  - **Speed.** Warm-cache GLib builds take the same time with either tool (11.8–12.4 s).
- **Still to do in M1 (M1e):**
  - critical-path order
  - the clock-step warning
  - smarter status output
  - full RDKit and LLVM builds with kiln. Those need your go-ahead to rebuild in your trees.

**M1e is done:** tasks 8–11, the layout matrix and the speed targets.

- **Scheduling.** Ready edges start in critical-path order. Each edge weighs its last duration from `.ninja_log` (or the average known duration if it has never run), where Ninja 1.12+ count every edge as 1. With history, the fixture's one-second edge starts first.
- **Status output** matches Ninja byte for byte in a terminal and in a pipe: the overwritten line with middle elision, the `FAILED:` block, colour stripping unless `CLICOLOR_FORCE`, every `NINJA_STATUS` placeholder and its error, and `--quiet`.
- **Clock steps.** kiln warns once when the wall clock steps back during a build. Forward steps keep mtimes in order and are ignored. The live check found the WSL steps have flipped to +3 s every 32 s ([problems/clock-steps.md](../problems/clock-steps.md)).
- **Defaults.** `-j` counts CPUs as Ninja 1.13 does (affinity mask, capped by the cgroup v2 or v1 quota), but also reads the v1 quota on hybrid systems, where 1.13 doesn't. kiln warns when the build dir is on NFS, 9p (`/mnt/c`), virtiofs or SMB.
- **New harness coverage.**
  - **Ninja Multi-Config** (`cmake-multi`, Debug and Release in one tree) is a fifth fixture kind in every script test.
  - **`TestEditDuringBuild`.** An input edited while its command runs rebuilds on 1.12+ and not on 1.10–1.11, for both tools.
  - **`TestLayouts`** covers the whole layout matrix below.
- **Speed.** Best of 5–11 runs on this machine (12 CPUs, WSL2, ext4):

  | Tree | Ninja | kiln | Ratio |
  |---|---|---|---|
  | 500k-edge synthetic, `-n` no-op | 3.37 s | 3.16 s | 0.94× |
  | 500k-edge synthetic, real no-op | 3.49 s | 3.14 s | 0.90× |
  | protobuf (largest corpus tree), real no-op | 44.5 ms | 57.1 ms | 1.28× |
  | GLib, real no-op | 32.2 ms | 40.4 ms | 1.25× |
  | RDKit `rdkit-props-batch-v2/build`, `-n` (12 pending) | 167 ms | 189 ms | 1.13× |

  Two changes got kiln there. It stats the files a scan will need in parallel before the scan (Ninja stats them one by one). It also leaves the garbage collector off until the heap reaches 64 MiB. kiln uses more memory than Ninja on the 500k tree (810 MB vs 497 MB).
- **C++20 modules, built for real.** In the CI image (clang 18, CMake 4.4.4, Ninja 1.13.2), kiln builds the modules sample from scratch. After an edit to a module interface, it regenerates the dyndep file mid-build and rebuilds the importer. Neither kiln nor Ninja finds work left after either build.
- **Full test run.** The whole compat suite passes with the RDKit trees included: 544 tests, 20 script runs (5 fixture kinds × 4 Ninja versions), all real trees, and four clock-step retries. So do `make lint test` and the CI image's `lint test compat-short`.
- **Corpus builds with kiln, tests compared with Ninja's trees.**
  - **protobuf v36.2**, from scratch at `-j8`: 1,139 edges in 19.3 min, against about 19 min for Ninja at its default `-j14`. The build was interrupted at edge 62 and resumed. All 4 ctest entries pass in both trees. Afterwards neither kiln nor Ninja finds work left.
  - **GLib**: all 403 tests give the same result in the kiln-built and Ninja-built trees: 393 pass, 8 skip, `gtester-xmllint-check` fails, and `threadtests` times out.
- **The big corpus builds with kiln (2026-10-22).**
  - **DuckDB v2.0.0** (699 edges, `-j6`): built by Ninja and by kiln; all 7,648 test cases pass in both trees.
  - **RDKit `Release_2026_09_1`** (1,911 edges, `-j6`): built by Ninja and by kiln; `ctest` gives the same result for all 278 tests in both trees (229 pass, and the same 49 Python-wrapper tests fail).
  - **LLVM 23.1.4 with clang, X86, Release** (4,392 edges, `-j8`, `LLVM_PARALLEL_LINK_JOBS=2`): all 124 binaries in `bin/` are byte-identical to the Ninja-built ones, and both tools find nothing left to do.
  - **DuckDB's two always-dirty edges** (`.cache/clangd` and `repository`) are utility targets with no output file, so they are dirty under Ninja too.
- **Found along the way:** the fixture's code generator didn't escape spaces in its depfile, and kiln didn't recognise `CMAKE_MAKE_PROGRAM:UNINITIALIZED=` in `CMakeCache.txt`. Both are fixed.

**Tasks**
1. **Lexer and parser** for the full Ninja language, fuzzed:
   - variables, `rule`, `default` and `pool` (with depth)
   - `build` with explicit inputs, `|` implicit inputs, `||` order-only inputs, `|` implicit outputs and `|@` validations
   - `include` (shared scope) and `subninja` (child scope)
   - escapes, `${var}`, `ninja_required_version` and `builddir`
2. **Evaluation.** Use Ninja's variable lookup order. Shell-escape `$in` and `$out`, and support `$in_newline` and `rspfile`/`rspfile_content`. Canonicalise paths exactly as Ninja does, without resolving symlinks.
3. **Graph.**
   - Phony semantics, validations, and cycle reports that print the cycle.
   - `dyndep`: load the dyndep file once its edge has run, and splice in the implicit inputs and outputs it lists. CMake's C++20 modules and Fortran depend on this.
4. **State.**
   - Port the v5, v6 and v7 build-log code (MurmurHash64A and rapidhash), the v4 deps log, and recompaction from Ninja.
   - For v6+, implement the start-time lock file that sets the recorded mtimes.
   - Never mix versions within one file.
5. **Dirty computation**, following each version's rules:
   - which mtime counts: the output's for v5, the recorded start mtime for v6+
   - the generator and `restat` exemptions
   - missing log entries, command changes, missing or stale deps, `restat` pruning, and order-only inputs
6. **Executor.**
   - Run `/bin/sh -c` in its own process group. Console-pool edges get the terminal.
   - Give each job one buffered pipe for its output, and read `wait4` rusage when it exits.
   - Depfiles: with `deps = gcc`, ingest the depfile into the deps log and then delete it. A depfile without `deps` is re-read when the graph loads.
   - Write each rspfile before its job and remove it after.
   - Support `-k`, `-n`, `-v` and `-l`, and handle SIGINT and SIGTERM.
7. **Manifest regeneration.** Build edges that produce the manifest first, then reload the manifest **and the state**, because generators run Ninja tools during regeneration. Bound the number of rounds. In dry-run mode, return after the "regeneration" as Ninja does.
8. **Output.** A status line that honours the `NINJA_STATUS` format, `-d explain`, `-d keepdepfile` and `-d keeprsp`.
9. **Critical-path priority** from log durations.
10. **Defaults.** `-j` = available CPUs + 2, where "available" respects CPU affinity and the cgroup CPU quota. Warn when the build dir is on drvfs (`/mnt/c`) or NFS.
11. **Clock steps.** Watch the wall clock against the monotonic clock during a build, and warn when it steps back. On this WSL machine it has jumped back 1.8 s, and later forward 3 s, about every 30 s, which makes any mtime-based decision depend on timing ([problems/clock-steps.md](../problems/clock-steps.md)).
12. **Known Ninja quirks.** For each behaviour in [ninja-quirks.md](../ninja-quirks.md), decide whether kiln mirrors it or improves on it. An example is Ninja ≤ 1.11 leaving a `restat` edge dirty forever when the deps log has lost the entry for its depfile-only inputs. Improving is only allowed where real Ninja, reading kiln's state, still sees nothing to rebuild.

**Acceptance**
- [x] Every corpus manifest parses without errors, for all generators and Ninja versions.
- [x] **Hashes match.** For every edge that Ninja considers clean, kiln's command hash equals the logged one. This must hold for MurmurHash64A trees (v5/v6) and rapidhash trees (v7).
- [x] **Deps match.** kiln's reading of each `.ninja_deps` equals `ninja -t deps` output.
- [x] **Dry runs agree.** On every corpus tree as it stands, kiln and Ninja choose the same edges, for each Ninja version.
- [x] **Differential suite** shows zero divergences across edit scenarios × generators × Ninja versions. The scenarios are:
  - touch a source, touch a header, edit a header
  - add or remove an `#include`
  - change a flag (regeneration)
  - delete an output, delete `.ninja_deps`
  - interrupt mid-build and resume
  - mass mtime change (branch switch)
  - a `restat` no-op
  - a module interface change (`dyndep`)
  - a multi-config build
  - `rspfile` edges
  - an input edited while its edge runs (v6+ semantics)
- [x] **Tool-switch invariant**, for each Ninja version:
  - after a Ninja build, `kiln` has no work to do
  - after a kiln build, `ninja` has no work to do
  - the log version never changes
- [x] **Layout matrix passes:**
  - in-source and out-of-source build dirs, including one outside the source tree
  - several build dirs per source, and git worktrees
  - relative `-C`, `-f` with a non-default manifest, and `builddir`
  - paths with spaces, a symlinked build dir, and a read-only source tree
- [x] Full builds of the large corpus projects at a conservative `-j` succeed, and test results match Ninja-built trees. *(fmt, Ninja, GLib, protobuf, the C++20 modules sample, DuckDB, RDKit and LLVM; see the M1e progress note.)*
- [x] A no-op build of the largest corpus tree is within 1.5× of Ninja's time. Load time on a 500k-edge synthetic manifest is within 1.5× of Ninja's.

### M2 — Drop-in integration (S)

**Design (revised 2026-10-21)**
- **Drop-in mode is chosen by invocation name.**
  - When kiln runs under the name `ninja` (a symlink), it is exactly Ninja's CLI: `--version` prints the emulated Ninja version, and there are no subcommands.
  - **Why:** generators parse `<make program> --version`, and `cmake --build --target install|test` passes target names that clash with kiln's subcommands. So pointing `CMAKE_MAKE_PROGRAM` at `kiln` itself can't work.
- **Reported version: 1.13.2,** the newest Ninja whose manifest, state and CLI kiln supports.
- **Reserved subcommands.** Every planned subcommand name is reserved now: `build status wait log stop mem install test exec cache shim`. `kiln build <name>` builds a target with that name, so later milestones never change what a command line means.
- **Choosing a fresh tree's log version** skips candidates that are kiln itself (compared by file identity), then tries `$NINJA`, then a real `ninja` on `PATH`, then defaults to v7.

**Tasks**
- Record the argv CMake and Meson pass to the make program (configure, `try_compile`, build, regeneration, `clean`, `install`, `test`) as test data.
- Implement natively the `-t` tools that generators call: `restat`, `recompact`, `cleandead`, `clean` and `compdb`.
  - CMake calls `restat` and `recompact`, and its `clean` target calls `clean`.
  - Meson's regeneration calls `restat` and `cleandead`, and its setup calls `compdb` (seen in M0).
- Pass any other `-t` tool to a real Ninja that isn't kiln. Its output is correct because the state is shared. With none available, give a clear error.
- Finish the CLI: `-l`, `-d stats`, `-d list`, `-w dupbuild=`/`phonycycle=`, `-h` and `-t list`.
- Document the ways in. `kiln shim <dir>` creates `<dir>/ninja`. Then use `-DCMAKE_MAKE_PROGRAM=<dir>/ninja`, or `NINJA=<dir>/ninja meson setup`, or put `<dir>` first on `PATH`.

**Progress (2026-10-21): M2 is done.**
- **Code.**
  - `cmd/kiln` picks drop-in mode from its invocation name.
  - In kiln mode it reserves the subcommands (`build` and `shim` work now).
  - It finishes Ninja's command line: `-l`, `-d stats`/`list`, `-w`, `-h`, getopt's error messages and `-t list`.
- **Subtools.** `internal/tools` holds the cleaner and compdb. `restat`, `recompact` and `cleandead` reuse the state writers. Other `-t` tools go to a real Ninja, and the log-reading ones only to one that writes the tree's log version.
- **Checked against Ninja:**
  - **`TestToolsMatchNinja`:** 12 tool scenarios × 5 fixture kinds × 4 Ninja versions give the same output, remaining files and logs. The only allowed differences are the two older `cleandead` behaviours in `ninja-quirks.md`.
  - **`TestDropIn`:** CMake (both Ninja generators) and Meson drive kiln through every recorded generator call.
  - **The command line** matches Ninja 1.13.2 byte for byte for `-h`, `--help`, bad flags, `-d list`, `-w list`, `-t list`, `--version` and unknown tools.
- **Corpus:**
  - fmt, protobuf and GLib switch to kiln with no rebuild.
  - An edited fmt source rebuilds through `cmake --build`, and all 21 tests pass.
  - An edited GLib file rebuilds through `meson compile`, with `restat` pruning all but 4 of 487 steps.

**Acceptance**
- [x] With `CMAKE_MAKE_PROGRAM` pointing at kiln (through `kiln shim`), `cmake --build` builds the CMake corpus projects, including a regeneration after editing `CMakeLists.txt`.
- [x] With `NINJA` pointing at the shim, `meson setup` and `meson compile` build the Meson corpus project, and `compile_commands.json` matches what real Ninja produces.
- [x] Switching `CMAKE_MAKE_PROGRAM` between ninja and kiln in one tree causes no rebuild.

### M3 — Memory-aware scheduling (M–L)

**Refinements (2026-10-21), from what this machine's kernel offers:**
- **No `/proc/<pid>/task/<tid>/children` on WSL**, so the probe scans `/proc` every tick. A `pid → pgid` cache means it only reads `stat` for new processes.
- **Working set, not usage.** The cgroup working set is usage minus inactive file pages (`inactive_file` on v2, `total_inactive_file` on v1).
- **Thrash signal.** With no PSI, swap-in and swap-out deltas from `/proc/vmstat` stand in for memory pressure.
- **Config arrives here.** `internal/config` comes in with M3, for the user's machine defaults, and M7 extends it.
- **Command kinds** for no-history predictions: compile, link, archive, codegen and other.

**Design**
- **Probe.**
  - Each job has its own process group. A sampler ticks every 100 ms (configurable) and walks each job's tree via `/proc/<pid>/task/<tid>/children`, falling back to scanning `/proc` by pgid.
  - Each tick it sums RSS from `statm` and notes process names (`comm`), so compiler backends can be told apart.
  - Job peak = max of three values: the sampled tree sum, the largest per-process `VmHWM` seen, and `wait4` `ru_maxrss`.
  - Sampling catches processes running at the same time (`-pipe`, LTO). `VmHWM` and `ru_maxrss` catch spikes between ticks.
- **Memory sources.**
  - kiln always reads host `MemAvailable`. When it runs in a memory-limited cgroup (containers, CI, systemd slices), it also reads that cgroup's limit and usage: `memory.max`/`memory.current` on v2, `memory.limit_in_bytes`/`usage_in_bytes` on v1.
  - Budget: `--mem-budget`, or config. The default is 90% of the tighter headroom at start.
  - OOM signal: the cgroup's `memory.events` `oom_kill` when available, else `/proc/vmstat` `oom_kill`.
- **History.** `.kiln/mem.log` holds append-only lines keyed by first output: peak KiB, duration, command hash and flags. It is compacted like the Ninja log.
- **Predictor.**
  - **Per output:** keep a guarded maximum plus a 10% margin. Any observation can raise it. Only full-size runs can lower it, and only slowly. A full-size run is one with a usual duration, or one where a compiler backend appeared in the tree.
  - **Why the guard:** a ccache hit uses about 10 MB and must not erase what a real compile needs.
  - **No history:** fall back, in generator-agnostic order:
    1. same rule and same output directory (which approximates "same target" in CMake, Meson and GN layouts)
    2. same rule
    3. same kind, classified from the command: compile, link, archive, codegen or other
    4. a default sized from the M0 spike
  - **Cold builds:** update these aggregates during the build, so a first build learns as it goes.
- **Admission.**
  - Take ready edges in priority order. Start one if its pool has a slot, fewer than `-j` jobs are running, and reserved + predicted ≤ budget.
  - `--min-jobs` (default 1) always runs, so nothing deadlocks. An edge predicted above the whole budget waits until it can run alone.
  - Smaller jobs may backfill past a blocked big one until the big one has waited too long or is on the critical path.
  - Hold each reservation until its job exits. In the M0 spike, compile peaks came at 83–99% of runtime, so releasing early is unsafe.
- **Pressure brake.**
  - Each tick, check the tighter of the memory sources and how much swap is in use.
  - Below a low watermark, stop admitting.
  - Below a critical watermark, kill the youngest job's process group and requeue it with a raised estimate. A misprediction then costs one job's progress, instead of the kernel killing a random compiler.
- **OOM accounting.** A failed job counts as OOM-killed if the OOM counter rose while it ran, or its output says `Killed signal terminated program`. Retry it once with 1.5× its observed peak.
- **CLI.**
  - `--mem-budget`, `--min-jobs`, `-j`
  - `--mem=off` (plain Ninja behaviour, for A/B runs)
  - `kiln mem top [N]` and `kiln mem forget <pattern>`

**Progress (2026-10-22): M3 is built; the headline benchmark waits for M5.**
- **Code.** `internal/memprobe` (sampler, 0.2% of a core with 14 jobs), `internal/memdb` (history and predictor), `internal/sched` (admission), `internal/sysinfo` (host and cgroup memory, v1 and v2), `internal/config` (TOML), `internal/build/memory.go` (the brake, requeue and OOM retry), and `kiln mem top|forget`. It's on by default, also in drop-in mode (`KILN_MEM=off` or `--mem=off` turns it off).
- **Tests.** The admission simulation, the predictor, the probe, and `tests/compat/mem_test.go` (budget, brake, OOM retry, and the container test).
- **First headline result (2026-10-22), in [docs/benchmarks.md](../benchmarks.md):** RDKit's 12 heaviest compiles in a 4-CPU / 6 GiB container. Default Ninja fails (OOM kill, 1,108 s); the best hand-tuned `-j4` takes 175 s; kiln with no flags takes 201 s on the first run and 159 s once it has learned, with no OOM kills. Single runs of a subset on one machine.
- **Still to do:** the cold full builds of RDKit, LLVM and DuckDB, and the laptop profile (part of M5's benchmarks).

**Acceptance**
- [x] **Simulation suite.** The admission policy is a pure function, tested with a simulated clock and synthetic job profiles. It never exceeds the budget by prediction, bounds starvation, respects `--min-jobs` and `-j`, and runs oversized jobs alone.
- [x] **Container test, no flags.** Inside `docker run --memory=2g`, kiln finds the 2 GiB limit by itself and completes the memhog fixture (jobs that touch N MiB for T seconds), with measured peak below the limit. `ninja -j8` gets OOM-killed.
- [x] Sampler overhead is under 1% of one core with 14 concurrent jobs.
- [ ] **Headline**, on two machine profiles: the WSL laptop and a small-runner container (for example 4 CPUs and 8 GiB).
  - At default settings, RDKit (compile-heavy) and LLVM (link-heavy) see **zero OOM kills** on both the first run and the learned run.
  - The learned run is no slower than the best hand-tuned setting (`-jN`, or `LLVM_PARALLEL_LINK_JOBS` for LLVM).

### M4 — Detached runs (S–M)

**Design**
- **Run directory.** Every build, foreground too, gets `.kiln/runs/<id>/` with:
  - `status.json`, rewritten atomically about once a second: state, pid, targets, done/total, running jobs with their RSS, budget/reserved, failures
  - `output.log`

  `runs/latest` points at the newest run. Keep the last N runs.
- **`kiln build --detach [targets]`.**
  - Re-execs `kiln __daemon` with `Setsid`, stdin from `/dev/null`, and output to the run log. When the M0 spike shows session teardown kills such processes, it starts inside `systemd-run --user --scope` instead.
  - The parent waits until the daemon holds the lock and has written its first status, so errors like a bad manifest are still reported synchronously.
  - Then it prints the run id and exits.
- **Liveness is the lock.** `status` reports *running* while the lock is held, *finished* (with the exit code) from `status.json` otherwise, and *died* if `status.json` says running but the lock is free. A lock can't go stale, because `flock` is released when the process dies.
- **Commands.**
  - `kiln wait [--timeout]` blocks on the lock, then exits with **the build's exit code**.
  - `kiln log [-f] [--failures]`: `-f` stops when the build ends.
  - `kiln stop` sends SIGTERM to the daemon.
  - `kiln status --json` is for scripts and agents.

**Progress (2026-10-22): M4 is built.** `internal/run` (lock, run dirs, atomic `status.json`, liveness, safe `stop`), an `Observer` hook on the builder, `kiln build --detach` with a handshake, and `status`, `wait`, `log [-f] [--failures]` and `stop`. `tests/compat/detach_test.go` covers the acceptance items below. Still open: the manual check with every WSL window closed ([problems/detached.md](../problems/detached.md)), which needs you.

**Acceptance**
- [x] A detached build survives SIGHUP to its session (scripted), and `status` shows progress while `wait` returns the build's exit code. *The SSH-disconnect and every-WSL-window-closed cases are the manual check from the M0 spike; they are still to do.*
- [x] After `kill -9` of the daemon, `status` says *died* and the next build starts normally.
- [x] `log -f` returns when the build ends.

### M5 — Benchmarks, docs, v0.1 (M)

**Tasks**
- **`bench/`** reproduces the tables per project (RDKit, LLVM and one medium project) and per machine profile (WSL laptop, small-runner container).
  - Runners: plain Ninja at default `-j`, the best hand-tuned setting, kiln first run, kiln learned.
  - Measurements: wall time, system peak RAM, and OOM kills, with a cold cache.
- **Running them.** Cold builds of the large projects take hours, so run the full matrix overnight and iterate on heavy subsets (the heaviest targets by history).
- **`DESIGN.md`:** architecture, state compatibility across Ninja versions, the scheduler model (probe → predictor → admission → brake), and the run lifecycle.
- **`docs/problems/`:** one write-up per problem, with examples from more than one project.
- **README:** general wording, status markers, the name clash, drop-in usage, and the licence.
- **Release:** tag `v0.1.0` with static Linux amd64 and arm64 binaries.

**Progress (2026-10-22).** Done: `README.md`, `DESIGN.md`, `CHANGELOG.md`, the Apache-2.0 licence, `docs/benchmarks.md`, the benchmark driver `tools/bench` (four runners, memory watchdog, container-aware), the release workflow (`make dist`: static amd64 and arm64 binaries with checksums, smoke-tested on both architectures, published on a `v*` tag) and a test that runs the README's own command lines. **Not done:** the benchmark runs themselves (hours each, and an idle machine), and tagging `v0.1.0`, which waits for those tables.

**Acceptance**
- [ ] Benchmark tables are filled for at least two large projects and two machine profiles, with the commands that reproduce them.
- [x] Every README usage line for shipped features works as written. *(`TestReadme` runs them.)*

### M6 — Transactional install (M–L)

**Ownership model**
- **Install set** = (project, tree).
  - Project is CMake's `CMAKE_PROJECT_NAME` or Meson's project name, and can be overridden.
  - Tree is the UUID in `<build>/.kiln/tree-id`.
- **Records** live in `<prefix>/.kiln/owners.json`: for each install set, its files with hashes and symlink targets.
- **Rules when tree T of project P installs:**
  1. Files T installed last time but no longer produces are removed. Files left behind after a rename are a problem even with only one tree.
  2. Files owned by another tree of P (a variant: Debug vs Release, Boost vs nanobind, another worktree) are refused unless `--swap`. `--swap` removes that tree's whole set, including files T doesn't produce.
  3. Files owned by a different project are refused. `--force` takes over just those files.
  4. Existing files that nobody owns (installed before kiln, or by a package manager) are adopted silently if the content is identical. Otherwise they're refused unless `--adopt`.
  5. A file about to be deleted must still match its recorded hash. Modified files block the install and are listed, unless `--force`.

**Protocol**
1. Build.
2. **Stage** with `DESTDIR=<stage> cmake --install <build>` or `meson install -C <build> --destdir <stage>`, on the prefix's filesystem so the commit is all renames. Snapshot the prefix before and after, to catch install scripts that ignore `DESTDIR`.
3. **Plan** by applying the rules above. `--dry-run` prints the plan.
4. **Commit** through `journal.json`:
   - move each existing destination into `backup/`
   - rename each staged file or symlink into place
   - move stale files into `backup/`
   - write `owners.json` atomically
   - fsync, mark the journal committed, and delete `backup/`
5. **Recover.** Roll back or forward on the next run, or explicitly with `kiln install --recover`.
6. **Root-owned prefixes.** Run `kiln install --stage` as the user, then `sudo kiln install --commit <txn>`. The commit step runs no build commands.
7. **Safety.**
   - `flock` on `owners.lock`.
   - Deduplicate manifests.
   - Install symlinks as symlinks.
   - Remove directories only if they're empty.
   - Never delete git-tracked files when the prefix is a checkout, and add `.kiln/` to `.git/info/exclude` there.

**Progress (2026-10-22): M6 is built.** `internal/generator` (CMake and Meson detection), `internal/install` (ownership records, the five rules, journaled commit, recovery, crash hooks) and `kiln install` with `--dry-run`, `--swap`, `--adopt`, `--force`, `--stage`, `--commit`, `--recover`, `--project`, `--config`. Tested with the rules unit-tested, a crash at every commit point, and fixtures under both generators. Still open: a `--commit` run by actual root. RDKit's in-tree swap is done (below).

**Acceptance**
- [x] Fixture variants A and B, under both CMake and Meson: installing B over A is refused, and `--swap` leaves exactly B's set.
- [x] Reinstalling after renaming an installed library removes the old file.
- [x] Two different projects coexist in one shared prefix, and a file both would install is refused.
- [x] **Crash injection.** A test hook can `kill -9` kiln at every commit step. Recovery always leaves the prefix exactly in the old or the new state, never a mix.
- [x] A root-owned prefix works through `--stage` and `--commit`. *(Tested as a non-root user: a read-only prefix is staged with instructions, and `--commit` is a separate step; not yet run as root.)*
- [x] RDKit's in-tree Boost↔nanobind swap leaves none of the 66 or 57 stale paths, and `import rdkit.Chem` works in both directions. *(kiln's own `Release_2026_09_1` checkout, both variants built with kiln: the two variants differ by exactly 66 and 57 paths; after each swap none of the other variant's paths remain, the ownership record equals the variant's installed set, `import rdkit.Chem` works, and `git status` of the checkout stays clean.)*

### M7 — Declared environment and `kiln test` (M)

**Design**
- **Config** lives in `<build>/.kiln/config.toml`, with machine defaults in `~/.config/kiln/config.toml`. Sketch:
  ```toml
  [env.set]
  PYTHONNOUSERSITE = "1"
  PKG_CONFIG_PATH = "/opt/deps/lib/pkgconfig"
  [env.prepend]
  PATH = ["/opt/toolchains/gcc-13/bin"]
  LD_LIBRARY_PATH = ["/opt/deps/lib"]
  [env]
  unset = ["PYTHONPATH", "CPATH", "LIBRARY_PATH"]
  ```
- **Where it applies.** Every process kiln spawns, and `kiln exec -- <cmd>`, which exists so configure (`cmake`, `meson setup`, `./configure`) sees the same environment as the build.
- **Rebuild on change.** `.kiln/env.log` records the env hash each output was built with, and an edge is dirty if it differs. The hash deliberately stays **out of** `.ninja_log`, so the Ninja no-op invariant survives.
- **`kiln test` backends:**
  - **CTest:** `ctest --test-dir <build> --output-junit …` (needs CMake ≥ 3.21)
  - **Meson:** `meson test -C <build>`, reading the JUnit file in `meson-logs/`
  - **custom:** a `[test] command` with an optional JUnit path
- **Results.** Compare against `.kiln/xfail.txt` (test name or regex, plus a reason). Exit 0 when only expected failures fail. An XPASS (expected failure that passes) is a warning, or an error with `--strict`.

**Progress (2026-10-22): M7 is built.** `internal/envdecl` (merge, apply, hash, `.kiln/env.log`), `internal/testrun` (JUnit, expected failures), config tables `[env]` and `[test]`, `kiln exec` and `kiln test`. The config chain is user, then `<source>/.kiln.toml`, then `<build>/.kiln/config.toml`.

**Acceptance**
- [x] **Leak test.** Plant a stub package in a fake user site, plus stray `PKG_CONFIG_PATH` and `CPATH` entries. Builds and tests under kiln never see them; plain `ninja` and `ctest` do.
- [x] Changing the environment rebuilds the affected edges, and Ninja still reports no work afterwards.
- [x] `kiln test` handles xfail correctly on one CMake and one Meson project. *(The fixtures under both generators; GLib as a real Meson project: see the progress log.)*
- [x] Tests run without a hand-set `LD_LIBRARY_PATH`.

### M8 — Shared cache (S–M)

**Design**
- **Settings.** `base_dir`, `dir`, `max_size` and `hash_dir` come from config and are exported to compile edges. kiln can suggest `base_dir` from the common parent in `git worktree list`.
- **Launcher injection** is opt-in (`[cache] inject = true`) and follows D5. Edges kiln can't classify are reported as uncacheable.
- **Stats.** Each build reports its hit rate from ccache stats deltas. `kiln cache stats|trim` shows size against the limit and eviction churn.

**Progress (2026-10-22): M8 is built.** `internal/cache` (settings, ccache statistics, the injection rules, base_dir suggestion), the `[cache]` config table, a command-rewrite hook on the builder that leaves the logged command alone, per-build hit-rate reporting, and `kiln cache stats|trim`. Measured on the fixture: plain ccache hit 0 of 23 on a second checkout, kiln-managed hit 23 of 23. Still to do: the same comparison on fmt and protobuf (corpus projects), which is part of M5's benchmark runs.

**Acceptance**
- [ ] Cold build of a second worktree, on two corpus projects: hit rates for plain ccache vs kiln-managed. *Done on the fixture (plain ccache 0 of 23 hits, kiln-managed 23 of 23); the fmt and protobuf runs come with M5's benchmarks.*
- [x] Injection produces cache hits for a project configured without a launcher, and Ninja still sees the tree as up to date afterwards.
- [x] A deliberately undersized cache shows churn in `kiln cache stats`.

### M9 — Stretch (unscheduled)

- **Content hashing instead of mtimes.** kiln has to bump output mtimes so Ninja keeps agreeing.
- **Traced execution** to catch undeclared dependencies. It's the prerequisite for a native action cache.
- **CPU-aware admission** for LTO and multi-threaded links, using learned CPU time per edge.
- **A per-job cgroup v2 probe and enforcement**, on hosts that support it.
- **A jobserver that hands out tokens by memory**, so nested builds (ExternalProject, superbuilds) respect the budget.
- **Memory-aware test scheduling** and EASY backfilling.
- **macOS.**

## Testing strategy

- **Unit tests and fuzzing:** the lexer, parser, depfile parser and dyndep parser, plus state-file round-trips for v5, v6 and v7.
- **Differential tests:** corpus × edit scripts × Ninja versions, in both dry-run and real-run mode, plus the tool-switch invariant for each version.
- **Real-tree checks** on every corpus tree: command hashes, deps and dry runs. They're read-only and cheap, so run them often.
- **End-to-end tests:**
  - memhog in memory-limited containers
  - the detached-run lifecycle
  - install crash injection, shared prefixes and root-owned prefixes
  - environment leaks

## Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Ninja semantics differ by version (v5 vs v6+ mtimes, hash functions) | Wrong rebuilds, or Ninja discarding the log | Separate state code for each version. Differential suite on every version. Never mix versions in one file |
| A future Ninja changes the formats again | kiln can't share state | Version guard with private-state fallback. Track upstream releases |
| Generator quirks (Meson, GN, hand-written) | Parse or semantic gaps | Support tiers. Corpus covers each generator. Clear errors |
| Containers hide the real memory limit | OOM in CI | cgroup-aware budget, plus the container acceptance test |
| Memory isn't predictable (ccache hits, flag changes) | OOMs, or needless serialisation | M0 spike checks the premise. Guarded maximum. Brake plus kill-and-requeue as the safety net |
| Sampling misses short spikes | Underestimates | Per-process `VmHWM` and `wait4` maxrss as floors, plus a 10% margin |
| The wall clock steps (seen on WSL: −1.8 s every 30 s) | Missed or endless rebuilds, for Ninja too | Detect steps and warn (M1). Document the timesyncd fix. Content hashing (M9) |
| Session teardown kills detached builds (WSL, logind) | `--detach` fails | M0 spike, a systemd scope, documentation |
| SMT caps the speed gain | Underwhelming speed numbers | Lead with "zero OOM, no tuning". Report speed honestly |
| Install scripts ignore `DESTDIR` | Staging bypassed | Snapshot the prefix before and after staging, and warn |
| Deleting files in shared or in-tree prefixes | Data loss | Per-file ownership, hash checks, journal and backup, git-tracked guard, `--dry-run` |
| Launcher injection misreads a command | Broken compiles | Opt-in, direct compiler calls only, uncacheable edges reported |
| Large corpus builds are slow | Slow iteration | Overnight runs and heavy subsets |
| Scope creep | v0.1 slips | v0.1 is M0–M5 only |

## Open questions

All three were resolved on 2026-10-21:

- **Tier 2 install and test:** through configured commands (`[install] command`, which must honour `DESTDIR`, and `[test] command`).
- **Config:** `<build>/.kiln/config.toml` first, then an optional `<source>/.kiln.toml` shared by every worktree, then `~/.config/kiln/config.toml`.
- **Drop-in version:** 1.13.2, the newest Ninja whose manifest, state and CLI kiln supports.

## References

- Scope: [`project_scope.md.txt`](../../project_scope.md.txt)
- Ninja manual: https://ninja-build.org/manual.html
- Ninja source: https://github.com/ninja-build/ninja
  - `src/build_log.cc` at v1.11.1, v1.12.1 and v1.13.1
  - commit `a2b5e6d` "Introduce mechanism to provide resiliency for inputs changing while the build runs"
  - commit `c266b21` "Increment ninja_log version"
  - commit `32e7e23` "Switch hash to rapidhash"
- n2: https://github.com/evmar/n2 · samurai: https://github.com/michaelforney/samurai · Siso: Chromium's Go build runner
- CMake: the Ninja generators, `cmake-cxxmodules(7)`, `cmake --install`, `ctest --output-junit`
- Meson: `NINJA` variable, `meson install --destdir`, `meson test`
- LLVM, "Building LLVM with CMake": `LLVM_PARALLEL_LINK_JOBS`, `LLVM_PARALLEL_COMPILE_JOBS`
- ccache manual: https://ccache.dev/manual/latest.html
- Linux man pages: `proc(5)`, `cgroups(7)` (`memory.max`/`current`/`events`, `memory.limit_in_bytes`), `wait4(2)`, `flock(2)`, `rename(2)`
