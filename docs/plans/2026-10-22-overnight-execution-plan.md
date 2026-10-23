---
title: "kiln: overnight execution plan (2026-10-22)"
type: feat
status: active
date: 2026-10-22
parent: 2026-10-21-feat-kiln-build-runner-plan.md
---

# kiln: overnight execution plan (2026-10-22, 01:45 → 08:00 IST)

## Context

The user said "execute the plan" for the roadmap to v0.3. It's recorded in `docs/plans/2026-10-21-feat-kiln-build-runner-plan.md` under "Roadmap to v0.3". They're away from 01:45 to 08:00 and want unattended progress:
- decide alone, test thoroughly, commit and push
- never stop to wait for approval
- run long jobs in the background, with alerts that wake me on failure

Commits carry no Claude attribution (memory: no-claude-attribution).

**Limits (checked 01:42):**
- **5-hour window:** 80% used; it resets at about 03:50 IST.
- **Weekly:** 11% used; it resets on 22 Oct at 18:30 IST.
- **Context:** 70% used; auto-compaction starts at 97%.

**Decision.** Use what's left of this window for the safe, high-value steps: get the tree green, commit and push M2, arm the wake-ups. Then use the fresh window from 03:50 to about 08:50 for the rest of M3, closing Phase A, and as much of M4 as fits.

**State now:**
- Nothing since `456875a` (M1e) is committed.
- **Done:** M2 (drop-in), the Phase A setup (licence, corpus additions, genbig), and most of M3. The last session wired memory scheduling into the builder, added `kiln mem`, `internal/config` and the flags. `make lint test` was green then.
- **Phase A queue** (detached, still running):
  - **DuckDB:** built with Ninja and with kiln. kiln's tree passes all 7,648 test cases.
  - **RDKit:** Ninja's build is done; kiln's build is at 1246/1911.
  - **LLVM:** comes next.
- **Open question from the DuckDB no-op check:** both kiln and Ninja still see 2 edges left after kiln's build (`.cache/clangd` and `repository`). They look like DuckDB's always-dirty edges, which needs confirming against Ninja's own tree.

## Window 1: now until the limit (about 20% left)

1. **Wake-ups:**
   - a one-shot cron at **03:53 IST**: "continue the overnight plan"
   - a recurring heartbeat every hour at :17 (it only fires when the session is idle), so work resumes after any stop
   - a Monitor on the Phase A progress file that wakes me if a step exits non-zero
2. **Make the tree green:** `go build ./...`, `make lint test`, and `go test -tags compat -short` for the tools and drop-in tests.
3. **Commit "M2: drop-in integration"** and push to `origin main`. It holds the M2 files, the licence, `NOTICE`, `genbig`, the corpus additions and the docs. The M3 files go in their own commit later. If they can't be separated cleanly, commit everything as "M2 + M3 work in progress", with a green build either way.
4. Update the memory progress note, then stop. The cron resumes the work.

## Window 2: 03:50 → about 08:50 IST (fresh 5-hour budget)

Order is by value; each step ends with a commit and push.

1. **Finish M3:**
   - A `memhog` fixture kind, plus `tests/compat/mem_test.go`. Run it locally with `docker run --memory=2g --memory-swap=2g kiln-ci` (cgroup v1), and add it as a CI job (cgroup v2): kiln with no flags completes, and `ninja -j8` is OOM-killed.
   - **Brake and OOM-retry tests,** through a test hook (`KILN_TEST_MEMORY` overrides the memory reading), so headroom can drop and the OOM counter rise mid-build. The killed job must rerun, the build must succeed, and every Ninja version's `ninja -n` must then find no work.
   - The full compat suite with memory scheduling on, plus `make ci-run`.
   - **Docs:** the M3 progress and checklist, `docs/problems/oom.md`, quirks (OOM retry, requeue), and `development.md`.
   - **Commit "M3: memory-aware scheduling"** and push.
2. **Close Phase A** once the queue finishes:
   - compare RDKit's `ctest` results between the trees, and LLVM's `bin/*` hashes
   - settle the DuckDB always-dirty question
   - tick M1's "full builds" item in the plan, and write the memory-spike follow-up if the trees allow
   - commit and push
3. **M4, detached runs,** if the budget allows:
   - `internal/run`: lock, run dirs, `status.json`, `output.log`
   - `kiln build --detach` with a handshake
   - `status` / `wait` / `log -f` / `stop`, and fail-fast on a second build
   - tests for SIGHUP, `kill -9` leading to *died*, `log -f` ending, and `wait`'s exit codes
   - commit and push
4. Update the memory progress note so the next session knows exactly where things stand.

**Not expected by 08:00:**
- **M5:** the benchmarks need about 2 days of machine time, and tagging v0.1.0 is outward-facing.
- **M6–M8.**
- **The manual WSL check** for M4 (closing every window), which needs you.

## Safeguards

- **Background work** (builds, the compat suite, the container test) runs with `run_in_background` or Monitor, filtered to catch failures (`FAIL`, a non-zero exit, `Killed`, `panic`), so errors wake me.
- **Only kiln's own trees** get real builds (`.work`, `.work/corpus`). The user's RDKit trees get dry runs with a version-matched Ninja.
- **No force-push.** Every push goes to `main` after `make lint test` passes; a red CI is fixed in the next commit.
- **Disk:** the corpus stays under about 15 GB (it's 7 GB now; LLVM is still to build).

## Verification for each commit

- `make lint test`
- `go test -tags compat` for the affected tests: the full suite for the M3 commit
- `make ci-run CI_TARGET="lint test compat-short"` for M3
- the GitHub Actions run, checked after each push

## What you'll find at 08:00

- **Commits on `origin/main`:** M2 drop-in; M3 memory-aware scheduling, if window 2 isn't cut short; Phase A results; and M4, if the budget allowed.
- **The plan doc** with ticked checklists and progress notes for M2, M3 and M1's corpus builds, plus a status line saying exactly where work stopped.
- **The memory note** `kiln-roadmap-progress`, with the next step.
- **A short summary** as the last message in this session: what passed, what failed, what's left.

## Progress log

- **02:15** M2 and the M3 core are committed and pushed (`8a95d29`).
- **02:40** M3 tests added: `TestMemBudget`, `TestMemBrake`, `TestMemOOMRetry` pass.

## Working notes (resuming after a pause or a limit)

**On resuming:** run `git log --oneline -8` and `git status`, then `go build ./...`, `make lint test` and the short compat suite, and fix anything red before going on. Then continue with the next unchecked item below. Commit and push each finished piece (no Claude attribution, no Co-Authored-By line). Long jobs run in the background with monitors. The user's RDKit trees only get `-n` dry runs. Keep this file's progress log and the memory note `kiln-roadmap-progress` current.

**State (after the overnight run, 2026-10-22 morning):** M0-M8 are built, tested, committed and pushed, and CI is green. See the progress log below and `docs/plans/2026-10-21-feat-kiln-build-runner-plan.md` for each milestone's checklist.

**Still open, all needing the user or hours of idle machine:**
1. The full benchmark tables for M5: cold builds of RDKit, LLVM and DuckDB on the laptop and in a container, with the best hand-tuned Ninja per project. `tools/bench`, `bench/rdkit-heavy.sh` and `docs/benchmarks.md` have the tooling and what has been measured so far (a subset of RDKit on two profiles).
2. Tagging `v0.1.0`, which publishes binaries through `.github/workflows/release.yml`. Held back until the tables above exist.
3. The manual WSL check for detached builds (`docs/problems/detached.md`).
4. M9 is a backlog.

## Progress log (continued)

Times are the real commit times (IST).

- **02:02** M3 committed and pushed (`d814648`), including a fix for the CI failure on `8a95d29` (`TestDropIn/meson` ran `meson test` without Ninja on `PATH`; verified in the CI image). The memory tests, including the 2 GiB container test, pass.
- **03:56** M4 committed (`e04e83a`): `internal/run`, the builder's Observer, `build --detach`, `status`/`wait`/`log`/`stop`, `tests/compat/detach_test.go`. CI green.
- **04:04** M6 committed (`c176140`): `internal/generator`, `internal/install` (rules, journaled commit, recovery), `kiln install`; unit tests including a crash at every commit point, plus end-to-end tests under CMake and Meson. CI green.
- **04:21** M7, M8 and the M5 documentation and release work committed (`1216f17`): declared environment, `kiln exec`, `kiln test`, the compiler cache, README, DESIGN, CHANGELOG, `tools/bench`, the release workflow. The README test found and fixed a bug (a detached build with nothing to do exited 1).
- **Phase A:** DuckDB and RDKit agree with Ninja's trees (RDKit: the same 229 pass / 49 fail in both); LLVM's kiln rebuild was still running. Remaining: its comparison, then hardening and the untested items listed in the memory note.
- **05:39** Phase A closed. LLVM (resumed under the memory scheduler after an OOM kill under the old kiln) finished with all 124 binaries identical to Ninja's; M1's "full builds" item is ticked in the plan.
- **06:14** First real headline benchmark (RDKit's 12 heaviest compiles, 4 CPUs / 6 GiB container): default Ninja fails with an OOM kill; best hand-tuned Ninja `-j4` 175 s; kiln with no flags 201 s first run and 159 s learned, no kills. Recorded in `docs/benchmarks.md` with its limits.
- **06:44** RDKit in-tree install swap done on kiln's own checkout (both variants built with kiln: the Boost.Python tree reused from Phase A, the nanobind tree built detached in 26.5 minutes). The variants differ by exactly 66 and 57 paths; after `--swap` none of the other variant's remain, and `import rdkit.Chem` works both ways. It exposed that RDKit installs to absolute destinations, so kiln now finds the real prefix itself (`ac28c65`).
- **06:47** Real-root `--commit` tested in a container (`make root-install-test`, also a CI job). It found a defect: files installed by root kept the staging user's ownership. Fixed, and the commit now validates a staged plan before changing anything (no paths outside the prefix, no symlink escapes as root, staged files unchanged since planning).
- **07:10** Laptop-profile benchmark. The first attempt exposed a real defect (with no history kiln overcommitted: 768 MiB guess per compile, brake stopping one job a second); fixed with larger first guesses and a brake that stops as many young jobs as needed (`e83960e`). Rerun on both profiles with the fixed code: kiln beats the best hand-tuned Ninja on both (details in docs/benchmarks.md).

### 2026-10-22 afternoon: local memory benchmarks in chunks

- **15:04** `tools/bench` runs series: rounds with a rotated order, resume from the results file, `-budget` chunks, `-cap`, per-round history snapshots for kiln's first/learned pair, `-plan` and `-summary`; `bench/heavy.sh PROJECT PROFILE` (`e2672f9`).
- **15:50** RDKit laptop, 3 rounds: best Ninja `-j7` 143 s, kiln 144 s first and 135 s learned (medians). A slow spell in round 2 (15:21-15:36) is recorded. The earlier single run's 16% shrank to about 6%, within the noise (`686d3d7`).
- **16:42** RDKit container, 3 rounds: best Ninja `-j4` 170 s, kiln 181 s first and 161 s learned. Across both profiles, kiln matches the best swept `-j` and is a little faster on median once it has learned (`4cb74ea`).
- **17:01** A bench bug: cleaning a target whose output a killed run left missing used `ninja -t clean`, which also removed LLVM's TableGen tools and libraries. Fixed, tested, and the affected row rerun (`5826096`).
- **17:36** The container image lacked ccache, which DuckDB's build runs every compile through. Added, and bench now refuses to record a run that fails within 5 s (`18d5928`).
- **17:45** LLVM laptop (7 units of ~2 GiB): Ninja's default is stopped by the guard 3 of 3 times; best `-j4` 142 s. kiln's first run was stopped by the 600 MiB guard, which sits between kiln's watermarks (~940 MiB no new starts, ~470 MiB stop the youngest), so this is an open question; the learned rows were not run (`b987e07`).
- **19:05** DuckDB container, 3 rounds: Ninja's default, `-j4` and `-j5` fail every time (OOM kills); `-j3` 338 s. kiln finishes every run: 522 s first, 366 s learned, 8% slower than `-j3` (`fe75c01`).

- **20:51** Scheduler fixes the benchmarks found (`e0c8f40`): on a whole machine the watermarks are at least 1 GiB / 768 MiB (containers keep a tenth / a twentieth of the budget), and an edge with no history and no same-rule neighbours gets the median of its kind, not the maximum (`-d sched` showed DuckDB's units all predicted at 3 GiB and run one at a time).
- **22:26** kiln rows rerun with it, 3 rounds each (`c470911`): LLVM now finishes every time (164/155 s; best Ninja -j4 142 s), RDKit laptop 133/128 s (-j7 143 s), RDKit container 167/164 s (-j4 170 s), DuckDB 360/363 s (only -j3 finishes, 338 s).

**Open after this:** cold full builds; LLVM in the container and DuckDB on the laptop; the kiln reruns were not interleaved with the Ninja rows; tagging v0.1.0 (held).

### 2026-10-23/24 night: cold full builds (one run per setting)

- **18:55-02:44** `bench/heavy.sh rdkit-full|duckdb-full` (`2d48b6c`). Three setup problems surfaced and were fixed, none recorded as results: DuckDB's build runs a uv Python from `~/.local` (mounted in the container, `030e411`), and RDKit's ChemDraw needs expat and freetype (added to the image, `42fe140`); bench refused to record the failed-at-once run.
- **Results:** RDKit laptop `-j7` 2,073 s vs kiln 1,924 / 1,887 s; RDKit container `-j4` 2,625 s vs kiln 2,944 / 3,028 s (slower, not investigated); DuckDB container `-j3` 1,718 s vs kiln 1,753 / 1,695 s. Ninja's default failed or was stopped everywhere.
- **Not done:** LLVM full builds, LLVM in the container, DuckDB on the laptop, the `v0.1.0` tag.

## Remote benchmark server (kiln-bench)

Added 2026-10-22. SSH alias `kiln-bench` (key `~/.ssh/kiln_bench`, user `swarnavas`; 32 cores, 251 GiB RAM, shared machine). **Strict boundary: everything lives under one directory and nothing outside it is touched.**

`ROOT=/home/swarnavas/Work/PhD_Work/Covaln_Dev_work/testing_btp/kiln-bench`

```
$ROOT/
  miniconda/     own Miniconda (batch install with -p; no `conda init`, no shell rc edits)
  env/           conda env (-p): go 1.25.1, cmake 3.22.1, ccache 4.5.1, ninja 1.10.1
  pkgs/          CONDA_PKGS_DIRS (package cache stays here, not in ~/.conda)
  tmp/           TMPDIR, GOPATH, GOCACHE, CCACHE_DIR, XDG_CACHE_HOME
  kiln/          clone of this repo
  duckdb/  rdkit/  llvm/    one folder per project: its own clone, build trees and results
  results/       raw benchmark output
```

Rules:
- Every command runs with `HOME`-relative state redirected into `$ROOT` (an `activate.sh` there sets PATH, CONDA_PKGS_DIRS, TMPDIR, GOPATH, GOCACHE, CCACHE_DIR, XDG_CACHE_HOME). Do not use the existing `~/miniconda3`: it would read `~/.condarc` and write its package cache outside `$ROOT`.
- No sudo, no apt, no edits to dotfiles, no system services. Docker is not used unless approved (images and layers land in /var/lib/docker, outside `$ROOT`); the container profile is otherwise emulated with kiln's own memory limits, or run with cgroup limits only if the user allows it.
- At most about 24 of the 32 cores; the machine is shared. Keep the bench memory guard on.
- `/home` is a separate 11 TB `/dev/sda` at 91%; expect slower I/O than the laptop and label every table with the server's specs and filesystem.
- Cleanup is `rm -rf $ROOT`.
