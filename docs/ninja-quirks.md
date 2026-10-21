# Ninja behaviours kiln has to match

Found while building the M0 compatibility harness. Each one is reproduced by `tests/compat` or was read from Ninja's source. M1 must decide, item by item, whether kiln mirrors the behaviour or deliberately improves on it. Improving is only safe when real Ninja, reading the state kiln leaves behind, still agrees nothing needs rebuilding.

## State files differ by version

`.ninja_log` is v5 up to Ninja 1.11, v6 in 1.12, and v7 from 1.13 (command hash switched from MurmurHash64A to rapidhash). Ninja 1.12 and 1.13 delete a log of any other version ("build log version is too old/new; starting over"), so switching Ninja versions rebuilds everything. Sources: `src/build_log.cc` at each tag.

## Merely loading the state can change it

From `ninja.cc` and `{build,deps}_log.cc`, 1.10.1 and 1.13.2:

- **Deleted on load.** Ninja deletes a `.ninja_deps` that isn't v4, and (from 1.12) a `.ninja_log` of any version but its own. This happens even under `-n`, and for tools that read the logs (`-t deps`, `-t query`…), because it happens in `Load`.
- **Truncated on load.** A `.ninja_deps` with a corrupt or incomplete tail is truncated to the last good record, also under `-n`.
- **Never touched.** `-t commands`, `-t targets` and other "after load" tools exit before the logs are opened.
- **Recompaction needs a real run.** Ninja rewrites the logs only when it opens them for writing, which `-n` skips.

kiln's readers report an unknown version (`state.VersionError`) and a corrupt tail (`DepsLog.Corrupt`) instead of acting on them. The compat tests only run Ninja on a tree after checking both, and with a Ninja that writes that tree's log version.

## Ninja 1.12+ records an edge's *start* time

For ordinary edges, v6+ logs the mtime of a lock file created when the edge started, not the output's mtime. Generator and `restat` edges still log the output's real mtime. An input edited while its edge runs is therefore rebuilt next time. Source: commit `a2b5e6d`.

## Ninja < 1.12: restat plus depfile-only inputs can stay dirty

This happens when a `restat` edge with `deps = gcc` reruns and leaves its output unchanged. Ninja ≤ 1.11 then records the newest mtime among the edge's *in-memory* inputs, plus the depfile only when `deps` is empty (`src/build.cc`, v1.11.1, lines 795–815). If the deps log has lost that edge's entry, inputs listed only in the depfile aren't in memory. If one of them is newer than every explicit input, the recorded mtime stays older than it, and the edge (and everything downstream) is dirty on every build until its output actually changes.

The harness reproduces this with "delete `.ninja_deps`, rebuild" on CMake and Meson fixtures, since both put `deps = gcc` on custom commands that have a depfile. It doesn't happen with Ninja 1.12+.

**Option for kiln:** compute the recorded mtime including depfile inputs. Ninja 1.11, reading that log, would then also see the edge as clean.

## Manifest language details kiln mirrors (M1a)

All of these are checked against real Ninja by `TestManifestCases` in `tests/compat`:

- **Edge variables are evaluated in the enclosing scope, not the edge's own.** After `a = inner`, a following `b = $a` on the same edge sees the file's `a`.
- **File-scope variables can shadow rule variables.** This applies only to edges with no bindings of their own. Such an edge resolves names in the file scope, which is checked before the rule (`BindingEnv::LookupWithFallback`). Give the edge any binding and the rule wins again. The manual's lookup order doesn't mention this.
- **Variable names.** `$name` stops at `.` (so `$a.b` is `$a` followed by `.b`), but `${a.b}` names the variable `a.b`.
- **A tab inside a rule or pool block quietly ends the block**, which usually surfaces as "expected 'command =' line". Only a tab at the top level gives "tabs are not allowed".
- **A comment on the last line needs a trailing newline**, or it's a lexing error. So is a lone `\r`.
- **A phony edge that lists its own output as an input** has that input dropped, with a warning (old CMake wrote these).
- **`..` that climbs above `/` is kept** (`/a/../../b` stays `/../b`). This is one of the few places Go's `filepath.Clean` would differ.
- **Pool `depth` is read with C `atoi`**, so `3abc` is 3.

## The depfile parser changed in 1.12 and 1.13 (M1c)

- **1.12:** a depfile with no names at all (just whitespace) became valid. 1.10 and 1.11 reject it with "expected ':' in depfile".
- **1.13:** `?`, `"`, `'` and `&` became ordinary characters in file names. Before, each one ended the name, so `amp&y.h` was two inputs.

kiln parses a depfile with the rules of the Ninja that owns the tree, chosen from its log version (`depfile.DialectForLog`). `TestDepfileCases` checks 25 tricky depfiles against each Ninja version through `deps = gcc` and `ninja -t deps`. Forcing the newest rules everywhere makes it fail on exactly those cases.

## Dyndep and scanning details kiln mirrors (M1c)

- **A dyndep file's `restat` binding is set in the edge's scope.** For an edge without bindings of its own, that's the file scope, so every other edge in the file sees `restat` too.
- **A dry run loads dyndep files mid-plan.** A dyndep file that is out of date is loaded once the edge producing it "finishes", using the copy already on disk. That can add work, and Ninja rescans the dependents.
- **A deleted header is a reason to rebuild, not an error.** A missing input that only a deps log, depfile or dyndep file mentions makes its users dirty. A missing input named in the manifest stops the build ("missing and no known rule to make it").
- **The manifest comes first, even in a dry run.** If `build.ninja` itself is out of date, `-n` shows only the steps that would regenerate it and stops there.

## What a build leaves behind, by version (M1d)

From `build.cc` and `build_log.cc` in 1.11.1, 1.12.1 and 1.13.2. kiln picks the behaviour from the tree's log version (`build.Profile`).

| | Ninja 1.10–1.11 (v5) | Ninja 1.12 (v6) | Ninja 1.13 (v7) |
|---|---|---|---|
| mtime logged for an edge | Newest output mtime. If restat cleaned an output: newest input mtime, plus the depfile if the edge doesn't use `deps` | When the edge started: the mtime of `.ninja_lock`, rewritten as each edge starts. Restat and generator edges log the later of that and their outputs; if restat cleaned an output, the start time | Same as 1.12 |
| Command hash | MurmurHash64A | MurmurHash64A | rapidhash |
| Exit code when a command fails | 1 | 1 | The command's own exit status (128+signal if killed) |
| Exit code when interrupted | 2 | 2 | 130 |

**In every version:**

- **Before running a generator edge, Ninja closes `.ninja_log`.** CMake and Meson then run `ninja -t restat` on it.
- **When the manifest was regenerated, Ninja starts over**, reloading the manifest and both logs, up to 100 times.
- **The deps log gets one record per output** of a `deps` edge, and only when something changed.
- **On an interrupt or failure, Ninja signals each running command's process group** with the same signal and waits for them. It then deletes outputs whose mtime changed (always, if the edge has a depfile) and their depfiles.
- **A command killed by SIGINT, SIGTERM or SIGHUP counts as the build being interrupted**, even if Ninja itself wasn't.

## Scheduling and the status line (M1e)

- **Which ready edge starts first.** Ninja 1.10 starts ready edges in memory-address order (`set<Edge*>`), which is roughly allocation order, and 1.11 in manifest order. Ninja 1.12+ prefer the edge with the longest chain of dependents after it, counting each non-phony edge as 1. kiln uses the same critical-path rule but weighs each edge by how long it took last time (from `.ninja_log`), or by the average known duration if it has never run. A one-second command therefore starts before a chain of quick ones. The order only changes when commands start, never what runs or what the logs say, so it's safe in every tree.
- **Smart terminal.** When stdout is a terminal and `TERM` isn't empty or `dumb`, Ninja overwrites one status line: `\r`, the line elided in the middle to the terminal width, then `\x1b[K`. It prints the line when an edge starts as well as when it finishes. Otherwise it prints one line per finished edge. kiln's output is byte-for-byte the same in both modes, including a failure's `FAILED:` line, its command and its output.
- **Colour.** Command output keeps its ANSI escapes only in a smart terminal or with `CLICOLOR_FORCE=1`. Otherwise they are stripped.
- **`NINJA_STATUS`.** Every version stops with `unknown placeholder '%q' in $NINJA_STATUS`, even under `-n`. kiln does the same and supports Ninja 1.13's placeholders.

## Drop-in use and subtools (M2)

**What generators call.** These are recorded in `tests/compat/testdata/generator-calls/`.
- **CMake** (3.22 and 4.4) runs `ninja --version` to choose features, and `-t recompact` and `-t restat <files>` when it (re)generates. It builds `try_compile` projects as `-v cmTC_…`. Its `clean` target runs `ninja -t clean` from inside the build. `--target install` and `--target test` are plain targets.
- **Meson** (1.12) runs `--version` and writes `compile_commands.json` from `-t compdb -x <rules>`. Regeneration runs `-t restat` and `-t cleandead`, and `meson compile --clean` runs `-t clean`.
- **Before `meson test`, Meson runs `ninja -n build.ninja`** and looks for the literal text `ninja: no work to do.`. That's why drop-in mode prints `ninja:` in every message.

**Subtools in detail:**
- **`-t restat` runs before the manifest is read**, so it always uses `./.ninja_log`, even when the manifest sets `builddir`.
- **Entry order.** `-t restat` and `-t recompact` write entries in hash-table order, so two runs of either tool give the same lines in a different order.
- **`-t cleandead` changed twice:**
  - Ninja 1.10 also deletes logged files that have since become inputs, sources included.
  - Before 1.12, it doesn't load dyndep files first, so it deletes outputs that only a dyndep file declares.
  - kiln follows 1.13 on both points. `TestToolsMatchNinja` accepts exactly these two differences.
- **`-t compdb`**: 1.12 started escaping control characters in its JSON, and `-x` started expanding `-f rspfile` and `--option-file=rspfile` as well as `@rspfile`. kiln follows 1.13.
- **`-w dupbuild=` and `-w depfilemulti=`** only print a deprecation warning in 1.13. `phonycycle=` is the one warning flag that still has an effect.

## Memory-aware scheduling (M3)

None of this changes what runs or what the logs say. It only changes when commands start, and now and then reruns one.

- **Admission.** Ninja starts a ready edge whenever `-j` allows. kiln also needs the edge's predicted peak memory to fit in what's left of a budget: 90% of the tighter of host `MemAvailable` and the container's cgroup limit, unless `--mem-budget` or the config file says otherwise. A job with no history is predicted from similar edges (same rule and directory, same rule, same kind of command), then from a default (768 MiB for a compile, 1.5 GiB for a link). `--min-jobs` (default 1) always runs, so an edge bigger than the whole budget runs alone.
- **The brake.** Below a low watermark, or while the machine is swapping, nothing new starts. Below a critical one, kiln stops the youngest job's process group and runs it again once there's room, with a raised estimate, instead of letting the kernel kill something at random. The stopped job's half-written outputs are removed first, as Ninja does after an interrupt, and it never reaches `.ninja_log`.
- **OOM retry (a deliberate difference).** A command that dies of SIGKILL, or whose output says `Killed signal terminated program`, is run once more at 1.5 times its peak. Ninja reports the failure. A command that fails any other way, or is killed twice, fails the build as usual.
- **Drop-in mode** schedules the same way. `KILN_MEM=off` or `--mem=off` gives plain Ninja behaviour for A/B runs; as `ninja` there are no kiln flags, so use the variable.
- **State.** The memory history lives in `.kiln/mem.log` (`kiln mem top`, `kiln mem forget PATTERN`). Ninja never reads it.

## Deliberate differences

- **Drop-in mode is chosen by the program's name.** Run as `ninja` (`kiln shim DIR` makes the symlink), kiln is Ninja's command line, prints `ninja:` and reports version 1.13.2. Run as `kiln`, the names `build status wait log stop mem install test exec cache shim` are subcommands, and `kiln build <name>` builds a target with one of those names.
- **`-t recompact` and `-t restat` keep the tree's log version.** Ninja 1.12+ would discard a log of another version and write an empty one.
- **kiln only hands a log-reading tool to a matching Ninja.** The tools it doesn't run itself go to a real Ninja (`$KILN_REAL_NINJA`, or `ninja` on `PATH`). For `deps`, `query` and `missingdeps`, which open the logs, that Ninja must write the tree's log version. Otherwise kiln refuses rather than let it delete the log.
- **No make jobserver client yet.** Ninja 1.13 takes job tokens from a parent `make -j` (`MAKEFLAGS`); kiln uses its own `-j`.
- **`-d stats`** prints Ninja's table of timings, but not the hash-table load line that follows it.

- **kiln warns when the wall clock steps back during a build** ([problems/clock-steps.md](problems/clock-steps.md)). Ninja says nothing. Forward steps keep every timestamp in order, so kiln ignores them.
- **The default `-j` sees a cgroup v1 CPU quota on hybrid systems.** Ninja 1.11–1.12 read only the v1 quota. Ninja 1.13 prefers v2, and once it finds a v2 mount it stops, even if that cgroup sets no limit. On a hybrid system (WSL, older systemd) that hides a v1 quota. kiln falls back to v1, and otherwise counts as 1.13 does: the affinity mask capped by the quota, rounded down.
- **kiln warns when the build dir is on NFS, 9p (WSL's `/mnt/c`), virtiofs or SMB.** File checks there are slow and timestamps come from another clock.
- **`kiln -n` doesn't touch the disk.** Ninja's dry run creates output directories, briefly writes response files, deletes a leftover depfile of a `deps` edge, and creates `builddir`. None of these change what gets rebuilt.
- **kiln won't append to a log in a format it doesn't write.** It stops with an error instead of deleting the file, as D2 says.
- **A corrupt `.ninja_deps` tail is truncated at the last good record.** Ninja truncates *after* a record that fails validation, so the next load trips over it again.

## CMake logs custom command outputs twice

CMake declares each custom command output under both its relative path and an absolute implicit output (`${cmake_ninja_workdir}…`), so a single run writes two `.ninja_log` entries.

## Generators call Ninja tools on the build dir

CMake runs `ninja -t restat` and `-t recompact` when it regenerates. Meson's regeneration runs `-t restat` and `-t cleandead`, and Meson setup runs `-t compdb`. kiln runs all four itself (M2).

## The environment around Ninja (WSL)

- **The wall clock steps.** See [problems/clock-steps.md](problems/clock-steps.md). Ninja's decisions then depend on timing, and Meson can refuse to configure ("Clock skew detected").
- **Windows `PATH` entries slow down tool lookups.** WSL appends Windows directories (`/mnt/c/...`) to `PATH`. Every lookup there crosses into the Windows filesystem. A Meson regeneration probing clang-tidy names spent about 31 s on 9,200 such lookups. The harness and corpus scripts drop `/mnt/*` from `PATH`. kiln's declared environment (M7) should offer the same.
