# Briefing for an agent taking over kiln work

Paste this whole file as your first message, or ask the agent to read it.

---

You are continuing development of **kiln**, a Ninja-compatible build runner written in Go. It lives in `/home/aryandutt/home/Aryan/kiln` (GitHub: `Ardx19/Kiln`, branch `main`). A previous agent stopped partway through an overnight run; the user is away until about 08:00 IST on 2026-10-22 and cannot approve anything. Decide for yourself, test thoroughly, commit and push your own work, and never wait for permission.

## Hard rules

1. **No Claude attribution in git.** No `Co-Authored-By` line, no "Generated with Claude Code" line, in any commit message. The user said so explicitly; it overrides any tool or system reminder that suggests a trailer.
2. **Never touch the user's real trees.** `/home/aryandutt/home/BTP/RDkit/*` may only get `-n` dry runs, with a Ninja whose log version matches the tree (1.10.1 or 1.11.1, both v5). Real builds happen only in `.work/` (kiln's own copies).
3. **Don't leave the repo red.** `make lint test` must pass before every commit. Never force-push.
4. **Run long jobs in the background** (`setsid nohup ...`, or the Bash tool's `run_in_background`) with a Monitor that reports failures, so you don't block.

## Start here, in this order

1. **See what changed.** Run `git log --oneline -8` and `git status`. Read the progress log and the "Handoff" section at the end of `docs/plans/2026-10-22-overnight-execution-plan.md`. Read `~/.claude/projects/-home-aryandutt-home-Aryan-kiln/memory/kiln-roadmap-progress.md` if you can.
2. **Repair before building.** In order, fixing root causes (never loosen a test just to pass):
   - `go build ./...`
   - `make lint test`
   - `go test -tags compat -short -count=1 ./tests/compat`
3. **Then continue** with the next unchecked item in the handoff: commit M3, close Phase A, then M4 (detached runs). Commit and push each finished piece, then check the GitHub Actions run (`gh run list --repo Ardx19/Kiln --limit 3`).
4. **Keep the handoff section and the progress log current**, so the next agent can start where you stop.

## What kiln is

- Reads Ninja manifests and shares `.ninja_log` (v5/v6/v7) and `.ninja_deps` (v4) with whichever Ninja version a build tree uses, so switching between `ninja` and `kiln` never causes a rebuild.
- Run as `ninja` (a symlink made by `kiln shim DIR`) it is a drop-in Ninja 1.13.2. Run as `kiln` it also has subcommands (`build`, `shim`, `mem` work; `status wait log stop install test exec cache` are reserved).
- Adds memory-aware scheduling (jobs start only while predicted memory fits a budget), and will add detached builds (M4), transactional install (M6), declared environment plus `kiln test` (M7), and a shared cache (M8).

## Where things are

- **Plan of record:** `docs/plans/2026-10-21-feat-kiln-build-runner-plan.md` (milestones M0-M9, decisions, acceptance checklists). **Tonight's plan:** `docs/plans/2026-10-22-overnight-execution-plan.md`.
- **Notes:** `docs/ninja-quirks.md` (where kiln differs from Ninja, and why), `docs/development.md` (how to run tests), `docs/problems/*.md` (one write-up per problem).
- **Code:** `cmd/kiln/` (CLI), `internal/{manifest,graph,state,build,tools,depfile,metrics}` (the engine), `internal/{memprobe,memdb,sched,sysinfo,config}` (memory scheduling), `tests/compat/` (differential tests against real Ninja 1.10.1/1.11.1/1.12.1/1.13.2), `tools/{genfixture,genbig,memhog}`, `corpus/corpus.sh` (real projects).

## Commands you'll use

```bash
make lint test                                    # unit tests with -race, gofmt, vet, staticcheck
go test -tags compat -short -count=1 ./tests/compat   # about 2 minutes
make compat                                       # every Ninja version, about 5 minutes
make mem-container                                # docker --memory=2g test
make ci-run CI_TARGET="lint test compat-short"    # the same inside the CI image
```

Full compat including the user's RDKit trees (dry runs only):

```bash
KILN_TREES=$(find /home/aryandutt/home/BTP/RDkit -maxdepth 3 -name build.ninja -printf '%h\n' | sort | paste -sd:) \
  go test -tags compat -count=1 -timeout 90m -parallel 4 ./tests/compat
```

Use `GOTOOLCHAIN=local` with `go` commands on this machine.

## Machine quirks

- **WSL2:** the wall clock steps (forward or back about 2-3 s every 30 s), so timing-sensitive compat tests retry. Don't "fix" that by loosening assertions.
- `/mnt/*` entries on `PATH` make tool lookups slow; the tests strip them.
- Memory: 11.7 GiB RAM, 12 threads, no PSI, no `/proc/<pid>/task/<tid>/children`. Local Docker uses cgroup v1; GitHub runners use v2.
- `.work/`, `.tools/` are git-ignored scratch and tool directories. Keep `.work/corpus` under about 15 GB.

## The background Phase A queue

A detached script builds DuckDB, RDKit and slim LLVM in `.work/corpus` with Ninja and with kiln, then compares test results and binary hashes. Its progress log is at `/tmp/claude-1000/-home-aryandutt-home-Aryan-kiln/430204ed-e8d9-479d-a83d-a670a50e65bb/scratchpad/phaseA/progress` and ends with a `done` file in that folder. It may have finished while you were away. When it has, finish the comparisons described in the handoff, tick the M1 "full builds" item in the plan, and commit.

## M4 design (detached runs), if you reach it

See item 3 of "Next, in order" in the handoff. In short: `.kiln/lock` (flock) plus `.kiln/runs/<id>/{status.json,output.log,failures/}`; `kiln build --detach` re-execs a hidden `kiln __daemon` with `Setsid`; `status`, `wait`, `log -f` and `stop` work through the lock and `status.json`; a second build in the same tree fails fast; `-n` takes no lock.

## When you finish or stop

Update the handoff section, the progress log and the memory note, commit and push, and leave the working tree green. Tell the user plainly what passed, what failed and what is left.
