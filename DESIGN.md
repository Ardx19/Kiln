# kiln: design

kiln is a Go program that runs Ninja manifests. This document explains how it is put together and why. The implementation plan and its progress notes are in [docs/plans/](docs/plans/); where kiln differs from Ninja on purpose is in [docs/ninja-quirks.md](docs/ninja-quirks.md).

## Layout

```
cmd/kiln/            command line: Ninja's flags, drop-in mode, subcommands
internal/manifest/   lexer, parser, scopes (include/subninja), evaluation
internal/graph/      nodes, edges, pools, path canonicalisation, shell escaping
internal/state/      .ninja_log v5/v6/v7, .ninja_deps v4, hashes, recompaction
internal/build/      dependency scan, plan, builder, runner, status line
internal/tools/      the -t tools generators call (clean, compdb, ...)
internal/memprobe/   /proc sampler: per-job peak memory
internal/memdb/      .kiln/mem.log and the memory predictor
internal/sched/      admission: which ready edges may start
internal/sysinfo/    CPUs, host and cgroup memory, filesystem types
internal/run/        lock, run directories, status.json (detached builds)
internal/generator/  detects CMake / Meson trees; staging commands
internal/install/    ownership records, install plan, journaled commit
internal/envdecl/    declared environment, its hash, .kiln/env.log
internal/testrun/    JUnit reports and expected failures
internal/cache/      ccache settings, injection, statistics
internal/config/     the TOML configuration
tests/compat/        differential tests against real Ninja versions
```

## Sharing Ninja's state

The most important decision: kiln does not keep its own idea of what is built. It reads and writes `.ninja_log` and `.ninja_deps` in the format the tree already has.

- Ninja's log has three versions. v5 (Ninja up to 1.11) hashes commands with MurmurHash64A and records each output's mtime. v6 (1.12) records the time the edge *started*, taken from the mtime of a lock file, so an input edited during the command forces a rebuild. v7 (1.13) switches the hash to rapidhash. Ninja 1.12 and later discard a log of any other version. kiln therefore never mixes versions: an existing log keeps its version, and a new tree takes the version of the Ninja it is configured with.
- Because the files are shared, `ninja -t restat` and `-t recompact` (which generators run) keep working on kiln's trees, and a tool switch costs nothing.
- Everything kiln-specific lives in `.kiln/` (memory history, environment hashes, run directories) where Ninja never looks. This is also what keeps the invariant checkable: after any kiln build, every Ninja version's `ninja -n` must find nothing to do, and the compat tests assert it after each new behaviour.

## One build

1. Take `.kiln/lock`; a second build in the tree fails at once and names the first.
2. Load the manifest. If the manifest is itself out of date, build it first and start over (up to 100 times), re-reading both logs, because generators run Ninja tools on them.
3. Scan: for each wanted output compare mtimes, the logged command hash, depfile and deps-log inputs, and dyndep files, exactly as the tree's Ninja version would. Nodes are stat'ed in parallel up front.
4. Plan: edges are ordered by critical path, each weighted by its last logged duration (Ninja 1.12+ weigh every edge 1).
5. Admit ready edges in that order while a pool slot, `-j` headroom and the memory budget allow.
6. Run each with `/bin/sh -c` in its own process group, under the declared environment, with `ccache` in front of direct compiler calls if injection is on.
7. When one finishes, ingest its depfile, apply restat pruning, record it in the logs, free its memory reservation, and update `status.json`.

## Memory-aware scheduling

Premise, measured on RDKit: an edge's peak memory is stable from run to run (within 0.4%), and it arrives late in the job (83-99% of its run), so a reservation is held until the job exits.

- **Probe.** Every 200 ms the sampler lists `/proc`, learns each new process's group from its `stat`, and sums resident memory per job group. `wait4`'s `ru_maxrss` is a floor for spikes between ticks. A compiler backend or linker in the group marks the job as having done real work.
- **History and prediction.** `.kiln/mem.log` keeps a guarded maximum per output: a bigger run raises the estimate at once, a smaller one lowers it by a quarter of the gap, and only if a backend ran (so a ccache hit can't erase what a compile needs). With no history, an edge is predicted from the same rule in the same directory, the same rule, the same kind of command, then a default.
- **Admission** is a pure function (`internal/sched`) tested with a simulated clock: an edge starts if a slot is free and the predicted total fits the budget; `--min-jobs` always run; an edge bigger than the whole budget runs alone; smaller edges may backfill past a blocked one until it has waited 10 s, then it holds a reservation and takes the next free slot, so nothing starves.
- **Budget.** 90% of the tighter of the host's `MemAvailable` and the cgroup's limit minus its working set. Containers report the host's memory in `/proc/meminfo`, so the cgroup (v1 or v2) is read as well.
- **Brake.** Below a low watermark, or while swapping heavily, nothing new starts. Below a critical one, the youngest jobs' process groups are stopped (as many as it takes to win back the low watermark) and they run again later with raised estimates. A job the kernel OOM-killed is retried once at 1.5 times its peak.

## Detached builds

`kiln build --detach` starts a copy of itself in a new session (stdin from `/dev/null`, output to the run's `output.log`) that ignores SIGHUP. The parent returns once the daemon holds the lock and has begun, or relays its output and exit code if it ended first (a bad manifest). Liveness is the lock itself: `flock` is released when the process dies, so it can't go stale, and a `status.json` that says *running* with the lock free means *died*. `stop` checks the pid's start time against `status.json` before signalling.

## Install

Staging uses `DESTDIR` through `cmake --install`, `meson install` or a configured command. The staged file set is planned against `<prefix>/.kiln/owners.json`, which records per (project, build tree) every installed path with its hash or link target:

1. files the tree installed before and no longer does are removed;
2. files of another tree of the same project are refused unless `--swap` (which removes that tree's whole set);
3. files of another project are refused unless `--force`;
4. unowned files are adopted if identical, otherwise refused unless `--adopt`;
5. a file about to be removed or overwritten must still match its recorded hash;
6. git-tracked files are never removed.

The commit writes a journal, moves each replaced file into a backup directory, moves the staged files in, writes `owners.json` atomically, and only then marks the journal committed. Recovery rolls back anything not committed, so a crash at any step leaves the prefix exactly as it was or exactly as it will be; a test kills the process at every step to check it. A prefix you cannot write to gets a staged install and a `--commit` step that runs no build commands, fit for `sudo`.

## Environment, tests and cache

The environment declaration (`[env]`) is merged from the user, source and build config files and applied to every command. A hash of the declaration is recorded per output in `.kiln/env.log`; an output built under a different hash is dirty. It is deliberately not in `.ninja_log`. `kiln test` runs CTest, Meson or a custom command in that environment and judges the JUnit report itself against `.kiln/xfail.txt`.

The cache feature exports ccache's settings and can put `ccache` in front of compile commands. The command that runs changes; the command that is logged does not, so Ninja's view of the tree is untouched.

## How it is tested

`tests/compat` takes a snapshot of a generated project (CMake, Meson, a hand-written manifest, multi-config), runs one edit script under real Ninja and one under kiln from the same snapshot, and compares what each would rebuild and what each actually ran, for every Ninja version, with Ninja dry-running kiln's tree at every step. The same machinery covers the tools, drop-in use through CMake and Meson, layouts (build directories outside the source tree, worktrees, paths with spaces, read-only sources), memory scheduling under a squeezed or limited memory, detached builds, install crashes, the environment, tests and the cache. Real trees (fmt, GLib, protobuf, DuckDB, RDKit, LLVM) are checked read-only with dry runs and by building them with kiln and comparing test results with Ninja-built trees. See [docs/development.md](docs/development.md).
