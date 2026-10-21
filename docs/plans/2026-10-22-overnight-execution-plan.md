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

**State at 02:20 IST:**
- Committed and pushed: `8a95d29` (M2 drop-in; M3 core).
- **Uncommitted, tested, ready to commit as "M3: memory-aware scheduling"** once the full compat suite passes:
  - `tests/compat/mem_test.go` (`TestMemBudget`, `TestMemBrake`, `TestMemOOMRetry`, `TestMemContainer`)
  - the `KILN_TEST_MEMORY` hook in `internal/sysinfo/memory.go`
  - Makefile `mem-container` and the CI job `mem-container`
  - docs: `docs/ninja-quirks.md`, `docs/development.md`, `docs/problems/oom.md`, the plan's M3 section
  - `docs/plans/2026-10-22-overnight-execution-plan.md` (this file)
- Verified so far: `make lint test` green, short compat green, the four memory tests pass (the container test ran in `docker --memory=2g`).
- **In flight:** the full compat suite (`KILN_TREES` = the 11 RDKit trees, dry runs). Output: `/tmp/claude-1000/-home-aryandutt-home-Aryan-kiln/430204ed-e8d9-479d-a83d-a670a50e65bb/scratchpad/compat-m3.out`; it ends with a line `exit=N`. If that file is gone, rerun: `KILN_TREES=$(find ~/home/BTP/RDkit -maxdepth 3 -name build.ninja -printf '%h\n' | sort | paste -sd:) go test -tags compat -count=1 -timeout 90m -parallel 4 ./tests/compat`.
- **Phase A queue** (detached, `scratchpad/phaseA.sh`): DuckDB done (kiln's tree passes 7,648 test cases); RDKit's kiln build was running; LLVM next. Progress: `scratchpad/phaseA/progress`. When it ends, compare `rdkit-test-*.xml`, `llvm-ninja.sums` vs `llvm-kiln.sums`, and check DuckDB's two always-dirty edges (`.cache/clangd`, `repository`) against Ninja's own tree; then tick M1's "full builds" item and commit.

**Next, in order:**
1. Commit M3 (above). Check the GitHub Actions run after pushing.
2. Close Phase A (above).
3. **M4 detached runs.** Design: `internal/run` with `.kiln/lock` (flock, fail fast naming the running build), `.kiln/runs/<id>/{status.json,output.log,failures/}`, `runs/latest`, keep 20; status.json is written atomically about once a second from the build loop (an Observer hook on the Builder, with a 1 s ticker so it never goes stale), fields: state, pid and process start ticks, targets, done/total, running jobs with RSS, budget/reserved, failures, exit code. `kiln build --detach` re-execs a hidden `kiln __daemon` with Setsid, stdin from /dev/null, output to `output.log`, with a handshake (parent waits for state running or finished; a manifest error is relayed with its exit code). Commands: `status [--json]` (running = lock held; finished = exit code; died = status says running but lock free), `wait [--timeout]` (exits with the build's code), `log [-f] [--failures]`, `stop` (SIGTERM after checking the pid's start ticks). `-n` takes no lock. `KILN_NO_LOCK=1` skips it. Tests in `tests/compat/detach_test.go`: SIGHUP to the session, `kill -9` leads to *died* and the next build runs, `log -f` returns at the end, `wait` exit codes, `stop` cleans up, a schema test for `status --json`.
4. Not for tonight: M5 (benchmarks, v0.1.0 tag), M6-M8.

## Progress log (continued)

- **02:35** M3 committed and pushed (`d814648`), including a fix for the CI failure on `8a95d29` (`TestDropIn/meson` ran `meson test` without Ninja on `PATH`; verified in the CI image).
- **02:50** M4 built (uncommitted at this line; see git log): `internal/run`, the builder's Observer, `build --detach`, `status`/`wait`/`log`/`stop`, `tests/compat/detach_test.go` (5 tests pass).
- **04:35** M6 built: `internal/generator`, `internal/install` (rules, journaled commit, recovery), `kiln install`; unit tests (including a crash at every commit point) and `tests/compat/install_test.go` pass. Phase A: DuckDB and RDKit trees agree with Ninja's (RDKit: same 229 pass / 49 fail in both); LLVM's kiln rebuild was running.
- **05:10** M7 built: declared environment (`internal/envdecl`, `.kiln/env.log`), `kiln exec`, `kiln test` with JUnit and expected failures; `tests/compat/env_test.go` passes; GLib `kiln test` with an xfail list was running.
- **05:45** M8 built: `internal/cache`, `[cache]` config, injection, hit-rate report, `kiln cache stats|trim`; `tests/compat/cache_test.go` passes (plain ccache 0/23 hits on a second checkout, managed 23/23).
