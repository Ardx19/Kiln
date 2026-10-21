# Working on kiln

kiln is a Go module (`github.com/Ardx19/kiln`, Go 1.25), Linux only. `make help` lists every task.

## Everyday

```bash
make lint test
```

`lint` runs gofmt, `go vet` and staticcheck. `test` runs unit tests with the race detector. Neither needs Ninja, CMake or Meson.

## Compatibility harness

`tests/compat` takes a snapshot of a generated project and runs the same edit script on it twice, with two Ninja-style runners. It compares what each would run (`-n -v`) and what each actually ran (changed `.ninja_log` entries). Until kiln's engine exists, both runners are real Ninja, so any difference is a bug in the harness.

```bash
make tools          # pinned Ninja 1.11.1/1.12.1/1.13.2, CMake 3.22.6/4.4.4, Meson 1.12.1 into .tools/
make compat-short   # newest Ninja only, about 1 minute
make compat         # every Ninja (including the system one), about 5 minutes
```

**Fixtures.** `go run ./tools/genfixture -gen cmake|meson|ninja -out DIR` writes the same synthetic project for each generator. The script tests run on five fixture kinds: CMake, CMake with forced response files, CMake's Ninja Multi-Config (Debug and Release in one tree), Meson, and a hand-written manifest. Reports land in `.work/compat/<fixture>-<ninja>.report.json`.

**Two things the harness works around on purpose:**

- **Coarse file timestamps.** Edits use `UTIME_NOW`, like `touch(1)`, and wait 30 ms after each build, because the kernel stamps files from a clock that ticks every 10 ms on this machine.
- **Clock steps.** If the wall clock steps during a script, only the steps before it are compared and the script is retried. See [problems/clock-steps.md](problems/clock-steps.md).

Known Ninja behaviours that look like failures are listed in [ninja-quirks.md](ninja-quirks.md).

### Manifest parsing against Ninja

`TestManifestCases` and `TestRealManifests` (in the same `compat` package) compare kiln's parser with the newest Ninja. They check the outputs and rules from `-t targets all`, the evaluated commands from `-t commands -s`, and the error messages. Both Ninja tools exit before Ninja opens `.ninja_log`, so they're safe on any tree. To include your own build trees:

```bash
KILN_TREES=/path/to/build:/path/to/other-build go test -tags compat -run Manifest ./tests/compat
```

`TestRealState` checks kiln's `.ninja_log` and `.ninja_deps` readers on the same trees (`-run RealState`):

- **Hashes.** Every log entry of an edge that `ninja -n` calls clean has kiln's command hash.
- **Deps.** kiln's reading of the deps log reproduces `ninja -n -t deps` exactly.

Ninja only runs under `-n`, and only one that writes the tree's log version. Any other Ninja would delete the log on load (see [ninja-quirks.md](ninja-quirks.md)). The test fails if the state files change.

`TestKilnDryRun` runs every edit script with Ninja doing the builds. At each step it compares `kiln -n -v` with `ninja -n -v`, before and after the build. `TestRealDryRun` does the same comparison once on every real tree, under the same safety rules as `TestRealState`. `TestDepfileCases` feeds tricky depfiles to each Ninja version (through `deps = gcc`) and compares the parsed inputs with kiln's.

`TestKilnBuild` runs every script twice from the same snapshot: Ninja does the builds in run A, kiln in run B. They must agree at every step (commands run, exit codes, log version, leftover work). During run B, Ninja also dry-runs kiln's tree before and after each build and must see the same work, which is the tool-switch invariant. `TestKilnInterruptResume` interrupts a kiln build with SIGINT and resumes it.

`TestEditDuringBuild` edits an input while the one-second command reading it runs. Ninja 1.10–1.11 then see nothing to rebuild and 1.12+ rebuild it, and kiln must do the same as the tree's Ninja. `TestLayouts` builds the CMake fixture with kiln in real-world layouts: a build dir outside the source tree, two build dirs per source, a git worktree, a relative `-C`, paths with spaces, a symlinked build dir, a read-only source tree, and an in-source manifest with `builddir` and `-f`. Afterwards neither kiln nor Ninja may find work left.

kiln builds like Ninja:

```bash
go build -o bin/kiln ./cmd/kiln
bin/kiln -C /path/to/build
```

It takes `-j`, `-k`, `-n`, `-v`, `--quiet`, `-f`, `-d explain|keepdepfile|keeprsp` and targets, and honours `NINJA_STATUS` and `CLICOLOR_FORCE`. Ready edges start in critical-path order, weighted by each edge's last duration in `.ninja_log`. It writes `.ninja_log` and `.ninja_deps` in the tree's existing format. A tree with no log gets the format of the Ninja it's configured with (`CMAKE_MAKE_PROGRAM`, `$NINJA`, or `ninja` on `PATH`).

**Compiler cache.** ccache, managed:

```toml
# <build>/.kiln/config.toml
[cache]
dir      = "/work/ccache"
base_dir = "/work"        # the common parent of your worktrees
max_size = "20G"
hash_dir = false          # let different build directories share entries
inject   = true           # run ccache in front of compilers nothing launches it for
```

```bash
kiln -C build cache stats    # size against the limit, hit rate, cleanups, base_dir hint
kiln -C build cache trim     # evict down to the limit
```

`tests/compat/cache_test.go` builds a second checkout of a fixture in a sibling directory: plain ccache hits 0 of 23 compiles, kiln-managed settings hit all 23; injection gets hits on a project configured with no launcher and Ninja still finds nothing to do; an undersized cache shows its cleanups in `kiln cache stats`. Unit tests in `internal/cache` cover the injection rules.

**Environment and tests.** Declare the environment once and every build, test and `kiln exec` command sees it:

```toml
# <build>/.kiln/config.toml (or <source>/.kiln.toml, or ~/.config/kiln/config.toml)
[env]
unset = ["PYTHONPATH", "CPATH", "PKG_CONFIG_PATH"]
[env.set]
PYTHONNOUSERSITE = "1"
[env.prepend]
LD_LIBRARY_PATH = ["/opt/deps/lib"]
```

```bash
kiln -C build exec -- cmake -S . -B build      # configure under the same environment
kiln -C build test                             # build, run the tests, apply .kiln/xfail.txt
kiln -C build test --strict                    # an expected failure that passes is an error
```

Tests in `tests/compat/env_test.go`: strays in `PKG_CONFIG_PATH`/`CPATH` and a package in a fake user site are invisible under kiln and visible to plain `ninja`; changing, removing or adding the declaration rebuilds while an undeclared variable doesn't; `kiln test` with expected failures and `--strict` under CMake and Meson; and a test that only passes with the declared `LD_LIBRARY_PATH`. Unit tests are in `internal/envdecl` and `internal/testrun`.

**Install.** `kiln install` installs the build into its prefix and records ownership (see [ninja-quirks.md](ninja-quirks.md)):

```bash
kiln -C build install --dry-run      # what would change
kiln -C build install                # build, stage, plan, commit
kiln -C build install --swap         # replace another tree's install of this project
kiln -C build install --stage        # then: sudo kiln install --commit DIR
kiln -C build install --recover      # finish or undo an install a crash interrupted
```

`internal/install`'s tests cover the rules and crash a commit at every crash point (`TestCrashAtEveryPoint`). `tests/compat/install_test.go` covers fixture variants A and B under CMake and Meson (refused, then `--swap` leaves exactly B's set), a renamed library, two projects in one prefix, SIGKILL at each commit step with `KILN_TEST_CRASH_AT=step:n` then `--recover`, stage then commit, a read-only prefix, and `[install] command` trees.

**Detached builds.** Start a build that outlives your terminal, then check on it from anywhere:

```bash
kiln -C build build --detach -j6 all
kiln -C build status            # running (pid, progress, memory), finished (exit code) or died
kiln -C build log -f            # follow output until the build ends
kiln -C build log --failures    # the full output of each failed command
kiln -C build wait              # block; exits with the build's exit code
kiln -C build stop              # as Ctrl-C would
kiln -C build status --json     # for scripts
```

`tests/compat/detach_test.go` covers it: the full lifecycle (a second build fails fast naming the first, `-n` still works, `log -f` ends with the build), exit codes and a bad manifest, SIGHUP to the daemon's session, `kill -9` leading to *died* with the next build starting normally, and `stop`.

**Memory scheduling.** `kiln -C build` keeps running jobs within a memory budget (see [problems/oom.md](problems/oom.md)). Useful flags: `--mem-budget 4G`, `--min-jobs N`, `--mem=off`, `-d sched` (print each decision). Machine defaults go in `~/.config/kiln/config.toml` (or `$KILN_CONFIG`):

```toml
[memory]
enabled  = true
budget   = "8GiB"
min_jobs = 1
```

Tests (`tests/compat/mem_test.go`, using `tools/memhog`, a program that touches N MiB):
- **`TestMemBudget`:** eight 200 MiB jobs under a 600 MiB budget never run more than 3 at once, the peak is learned into `.kiln/mem.log`, and `--mem=off` shows what Ninja would do.
- **`TestMemBrake`:** `KILN_TEST_MEMORY=FILE` replaces the memory readings (`available=BYTES`, `oom_kill=N`, ...), so the test can squeeze memory mid-build. A job is stopped, rerun, and the build finishes with Ninja finding nothing left to do.
- **`TestMemOOMRetry`:** a job SIGKILLed on its first run is run once more; a failing job, or one killed twice, fails the build.
- **`make mem-container`** runs `TestMemContainer` in `docker run --memory=2g`: eight 400 MiB jobs, so kiln finishes with no flags while `ninja -j8` is OOM-killed. CI runs it on cgroup v2; local Docker on WSL is cgroup v1.

**Drop-in mode.** `kiln shim DIR` makes `DIR/ninja` a symlink to kiln. Under that name kiln behaves exactly like Ninja 1.13.2, so CMake and Meson can drive it:

```bash
kiln shim ~/.local/share/kiln/bin
cmake -S src -B build -G Ninja -DCMAKE_MAKE_PROGRAM=$HOME/.local/share/kiln/bin/ninja
NINJA=$HOME/.local/share/kiln/bin/ninja meson setup build src
```

Two tests cover drop-in mode:
- **`TestToolsMatchNinja`** runs `clean`, `cleandead`, `compdb`, `recompact` and `restat` with each Ninja and with kiln from the same snapshot. Output, remaining files and both logs must match, for every Ninja version and fixture kind.
- **`TestDropIn`** drives the shim through CMake (both Ninja generators) and Meson: configure, build, regeneration, clean, install, test, and switching `CMAKE_MAKE_PROGRAM` back and forth.

`tests/compat/testdata/generator-calls/` records every call CMake and Meson make to the make program.

`go run ./tools/genbig -out DIR` writes the 500k-edge manifest that the M1 speed target is measured on.

Fixture trees live under `.work/` (override with `KILN_WORK`). Runs inside the CI image use `.work/ci/`, because trees made there contain `/src/...` paths.

Parse speed on a real tree:

```bash
KILN_BENCH_DIR=/path/to/build go test ./internal/manifest -run XXX -bench Load
```

## Corpus

```bash
corpus/corpus.sh list
corpus/corpus.sh all ninja fmt fmt-multi    # also: make corpus PROJECTS='…'
```

Projects are pinned by tag and commit. They're checked out and built under `.work/corpus/<project>/{src,build}`. `cxx-modules` needs Clang ≥ 16 or GCC ≥ 14, so run it in the CI image. `rdkit` needs Boost and Python development files; pass them with `RDKIT_CMAKE_ARGS`.

## CI image

```bash
make ci-image                                       # docker build -f ci/Dockerfile -t kiln-ci .
make ci-run CI_TARGET="corpus PROJECTS=cxx-modules"
make ci-run                                         # lint test compat, inside the image
```

The image carries the same pinned tools as `scripts/tools.sh` (`KILN_TOOLS=/opt/kiln-tools`), plus GCC 14 and Clang 18. `.github/workflows/ci.yml` runs lint, tests, the compat suite and the small corpus on every push, and GLib and protobuf nightly.

## Spikes

| Spike | Command | Write-up |
|---|---|---|
| Peak memory per edge | `go run ./spikes/memrss -C <build dir> -n 8 -runs 2` | [problems/oom.md](problems/oom.md) |
| Surviving session teardown | `spikes/detach/heartbeat.sh hangup-test`, or `start` / `check` | [problems/detached.md](problems/detached.md) |
| Wall-clock steps | `go run ./spikes/clock -d 90s` | [problems/clock-steps.md](problems/clock-steps.md) |
