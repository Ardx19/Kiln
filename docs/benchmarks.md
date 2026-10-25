# Benchmarks

This page says how to reproduce kiln's benchmark tables and records what has been measured, in the order it was measured. The three large projects are RDKit (compile-heavy), LLVM with Clang (the largest) and DuckDB (the medium project: 699 commands, a few very heavy unity builds).

**Where things stand** (kiln after `c14f5f2`; the sections below say which kiln each run used):
- **Safety.** On machines short of memory (an 11.7 GiB laptop, a 4-CPU / 6 GiB container), Ninja's default `-j` failed or was stopped by the memory guard every time, and kiln with no flags finished every run with no OOM kills.
- **Speed once kiln has learned a project,** against the best hand-tuned `-j`:
  - 7% faster on DuckDB's heaviest compiles (container)
  - 4% faster on RDKit's whole build (container, one run)
  - 9-10% faster on RDKit on the laptop
  - 3% slower on LLVM's heaviest compiles (laptop)
  - On a 251 GiB server, with no memory pressure: 13% faster on DuckDB's whole build, level on LLVM's and RDKit's.
- **First builds,** with no memory history, are kiln's weak spot: 18% slower on DuckDB's heaviest compiles and 7% slower on LLVM's.
- **Not run:** LLVM in the container, and DuckDB on the laptop. Every other cold full build has been, including LLVM's on the server.

The sections up to "Cold full builds (2026-10-23)" used the scheduler as of `e0c8f40`. "After the scheduler fixes" has the later reruns: they replace the 12-15% RDKit container loss and the DuckDB and LLVM losses reported earlier.

## Measured so far

WSL2 on a Ryzen 5 5500U (6 cores, 12 threads), 11.7 GiB RAM, ext4. Best of 5-11 runs.

| What | Ninja 1.13.2 | kiln | How |
|---|---|---|---|
| No-op build, 500k-edge synthetic manifest | 3.49 s | 3.14 s | `go run ./tools/genbig -out DIR`, build it once with Ninja, then time `ninja -C DIR` and `kiln -C DIR` |
| No-op build, protobuf (1,945 edges) | 44.5 ms | 57.1 ms | the corpus tree |
| No-op build, GLib (1,611 edges) | 32.2 ms | 40.4 ms | the corpus tree |
| Near-no-op dry run, RDKit `rdkit-props-batch-v2/build` (12 pending) | 167 ms | 189 ms | `ninja -n` vs `kiln -n`, same log version |
| Eight 400 MiB compile jobs in a 2 GiB container | `-j8` fails | finishes, no flags | `make mem-container` |
| Second checkout of a project: compiler cache hits | 0 of 23 (plain ccache) | 23 of 23 | `tests/compat/cache_test.go` |

kiln stats files in parallel before the scan, which is why it is faster than Ninja on very large manifests and slower by a fixed ~15 ms on small ones (process start, reading the configuration).

## RDKit's 12 heaviest compiles (2026-10-22)

The 12 slowest translation units of an RDKit `Release_2026_09_1` build (Boost.Python wrappers, ~1.3 GiB each at their peak), compiled from clean, on two machine profiles. Ninja at its default `-j` is what `cmake --build` or a bare `ninja` would do. kiln is run with no flags.

**Laptop: 12 threads, 11.7 GiB, WSL2.** 3 runs per row, in rounds with the order rotated each round; the table gives the median and, in brackets, the range. A memory guard stops any run that leaves under 600 MiB free, because a real OOM can freeze the machine.

| Runner | Result | Wall time, median (range) | Peak memory, median | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j14`) | **stopped by the guard** (it would have run out of memory); one run | 28 s | 10.3 GiB | n/a |
| ninja `-j5`, hand-tuned | 3 of 3 ok | 157 s (152–177) | 7.1 GiB | 0 |
| ninja `-j6`, hand-tuned | 3 of 3 ok | 148 s (144–150) | 8.4 GiB | 0 |
| ninja `-j7`, hand-tuned (best) | 3 of 3 ok | 143 s (136–198) | 8.3 GiB | 0 |
| **kiln, first run** (no memory history) | 3 of 3 ok | 133 s (131–140) | 8.2 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | **128 s** (128–132) | 9.1 GiB | 0 |

Command: `bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned`, run in four chunks between 15:05 and 15:50 IST. The kiln rows are from the scheduler as of `e0c8f40` (below), rerun with `-only kiln-first,kiln-learned -series kiln-e0c8f40` from 21:10 to 21:24; with the earlier scheduler they were 144 s (139–206) and 135 s (132–208), interleaved with the Ninja rows.

What this shows on the laptop: kiln with no flags is about 7% faster than the best hand-tuned Ninja on its first run and 10% once it has learned the jobs (128 s against 143 s), and its range is tight. Two cautions: the kiln rerun was not interleaved with the Ninja rows (it ran hours later, after the scheduler change), and the earlier, interleaved kiln runs were 6% faster than `-j7` once learned, a margin within the noise. The single run measured first, against `-j6`, had suggested 16%.

**The slow spell in round 2.** Four runs in a row, from 15:21 to 15:36 (round 2's `-j7`, kiln's first and learned runs under the earlier scheduler, then `-j5`), were 12-58% slower than the same runners in rounds 1 and 3: 198, 206, 208 and 177 s. Nothing else was running in WSL (the load was the compiles; the Claude Code app took well under one core), so the cause is probably on the Windows side: another program, or heat throttling the CPU, which WSL can't see. It is why those ranges reach so high, and why medians rather than means are reported.

**Small runner: a container with 4 CPUs and 6 GiB (no swap)**, GCC 11.4, 3 runs per row in the same rotated rounds. Here nothing guards the run: running out of memory means a real OOM kill.

| Runner | Result | Wall time, median (range) | Peak memory, median | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j6`) | **failed** (a compile was OOM-killed); one run | 1,108 s | 6.0 GiB | 1 |
| ninja `-j3`, hand-tuned | 3 of 3 ok | 178 s (174–190) | 3.7 GiB | 0 |
| ninja `-j4`, hand-tuned (best) | 3 of 3 ok | 170 s (163–170) | 5.0 GiB | 0 |
| ninja `-j5`, hand-tuned | 3 of 3 ok | 185 s (182–192) | 5.7 GiB | 0 |
| **kiln, first run** (no memory history) | 3 of 3 ok | 167 s (166–178) | 3.7 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | **164 s** (156–169) | 4.5 GiB | 0 |

Command: `bench/heavy.sh rdkit container -tuned 3,4,5 -only ninja-tuned,kiln-first,kiln-learned`, run in four chunks between 15:51 and 16:41 IST (the rows' start times are the container's UTC). kiln rows as of `e0c8f40`, rerun from 21:25 to 21:43; with the earlier scheduler they were 181 s (173–188) and 161 s (157–179).

What this shows, on both profiles: with no settings at all, kiln finishes where Ninja's default fails (an OOM kill in the container; on the laptop the guard had to stop it). In the container it matches the best hand-tuned Ninja (167 and 164 s against 170 s, within the noise); on the laptop it is 7-10% faster. With the earlier scheduler, compared within each round, the learned run beat the round's best Ninja twice, tied it twice and lost twice, so the honest claim is that kiln with no flags matches the best `-j` found by sweeping, and may be a little faster once it has learned. The single runs measured first (11% and 16% faster) overstated it.

How it got there, because the first attempt was worse: my first laptop run had kiln start with a 768 MiB guess for a compile. About a dozen heavy compiles started together, each grew to 1.3 GiB, and kiln's brake (which then stopped one job a second) could not keep up, so the benchmark's guard had to stop the run. That is a real defect, not a benchmark artefact. The fixes are in the code the numbers above come from: the first guess for a compile is now 1.5 GiB (3 GiB for a link), and the brake stops as many young jobs as it takes to win back the low watermark. The container "first run" improved from 201 s to 180 s with the same change.

Limits, stated plainly:
- **The kiln rows were rerun after the scheduler change** (`e0c8f40`), hours after the Ninja rows, not interleaved with them; the laptop's speed drifted by up to 58% within this session (the slow spell above).
- **Three runs per row**, on one machine (a Ryzen 5 5500U laptop) and one project. Default Ninja's rows are single runs, made earlier, since repeating a known failure costs 18 minutes in the container. With three runs, a difference of a few percent is within the noise; the ranges show how much. "Best hand-tuned" is the best of the values swept: `-j5` to `-j7` on the laptop, `-j3` to `-j5` in the container.
- **Subsets here; the cold full builds are in the next section.** Each project's heaviest compiles, chosen from its build log.
- **Ordering and page cache.** The page cache can't be dropped without root, so runs after the first in a chunk find the sources and headers cached. The rotation gives every runner each position once over three rounds.
- **Peak memory** is the system's (laptop) or the container's working set (container), sampled twice a second, and includes file cache the compiles read. The learned laptop runs used more memory than the hand-tuned ones (9.1 against 8.3 GiB) because they keep more jobs running; the guard never fired.
- ccache was disabled. Raw numbers, one JSON line per run with its start time: `bench/results/rdkit-12-heaviest.jsonl`. Reproduce with `bench/heavy.sh rdkit laptop` or `bench/heavy.sh rdkit container` (below).

## LLVM's heaviest compiles (2026-10-22)

Seven of the slowest translation units of an LLVM build with Clang (Release, GCC 11.4): `SemaExpr`, `X86ISelLowering`, `CodeGenModule`, `SemaDecl`, `SemaTemplate` and `SemaOpenMP`, plus `ASTMatchers/Dynamic/Registry.cpp`, the slowest unit in the tree's build log. Compiled alone (`ninja -j1`, one at a time), each takes 34-70 s and needs about 1.8-2.5 GiB (`bench/results/llvm-sizing.jsonl`; that file's peaks include the ~1.1 GiB the system was using before), so all seven at once want about 15 GiB. The link-heavy comparison first planned for LLVM does not fit this tree: its links take 25 s or less.

**Laptop: 12 threads, 11.7 GiB, WSL2**, the same 600 MiB guard, 3 runs per row:

| Runner | Result | Wall time, median (range) | Peak memory, median | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j14`) | **stopped by the guard**, 3 of 3 | stopped after 50-66 s | 11.1 GiB | n/a |
| ninja `-j3`, hand-tuned | 3 of 3 ok | 164 s (160–173) | 6.6 GiB | 0 |
| ninja `-j4`, hand-tuned (best) | 3 of 3 ok | **142 s** (142–153) | 8.3 GiB | 0 |
| ninja `-j5`, hand-tuned | 3 of 3 ok | 154 s (146–163) | 10.0 GiB | 0 |
| **kiln, first run** (no memory history) | 3 of 3 ok | 164 s (162–177) | 10.9 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | 155 s (151–158) | 8.7 GiB | 0 |

Command: `bench/heavy.sh llvm laptop -tuned 3,4,5`, with `-only ninja-default,ninja-tuned` for rounds 2 and 3; 16:50–17:32 IST. kiln rows as of `e0c8f40`: `-only kiln-first,kiln-learned -series kiln-e0c8f40`, 20:52–21:09.

What this shows: Ninja's default can't run these at all on this laptop, and kiln with no flags finishes every time, with the guard never firing. It is not the fastest: 155 s once learned against 142 s for the best hand-tuned `-j4` (9% slower), level with `-j5`. Its first run keeps about 800 MiB free at its peak (10.9 GiB used) by stopping compiles that outgrow their 1.5 GiB guesses and running them again, which costs time.

**With the earlier scheduler, kiln did not finish here.** Its first run was stopped by the guard after 77 s (one run). With no history, kiln guesses 1.5 GiB for a compile, so its budget (90% of the 10.5 GiB free at the start, about 9.4 GiB) lets it start six of these ~2.2 GiB units at once. Its brake has two watermarks: below a tenth of the budget free (about 940 MiB here) it starts nothing new, and only below a twentieth (about 470 MiB) does it stop the youngest jobs. The guard stops a run at 600 MiB, between the two, so as the six compiles grew past their guesses, the guard ended the run before kiln's brake was due to act. Whether kiln would then have recovered without an OOM kill was not measured: on this machine a real OOM can freeze WSL, which is why the guard exists. The run left no memory history (no compile had finished), so a "learned" run would only have repeated the first run, and the learned rows were not run. That was a kiln defect: a whole machine swaps and stalls well before its OOM killer acts, so running it down to 470 MiB is unsafe whatever the guard says. Since `e0c8f40`, without a cgroup limit, kiln starts nothing new below 1 GiB free and stops the youngest jobs below 768 MiB (a quarter and an eighth of the budget on a small machine); inside a container's limit the watermarks stay a tenth and a twentieth. The rows above are from that code, and the RDKit kiln rows were rerun with it too.

One run was redone. The bench tool cleaned a target by `ninja -t clean` when its output file was missing, which is the case after a killed run, and that also removes everything built to make it. After the guard stopped round 1's default Ninja, the next run (`-j3`) had to rebuild LLVMSupport and the TableGen tools and hit the time cap. The tool is fixed (`5826096`), and that row was dropped and rerun. The RDKit targets are built from sources only, so their runs were not affected.

## DuckDB's 12 heaviest compiles (2026-10-22)

The 12 slowest unity-build translation units of a DuckDB Release build (`ub_*.cpp`, from the tree's build log), compiled from clean in the **small-runner container (4 CPUs, 6 GiB, no swap)**, GCC 11.4, 3 runs per row. kiln's own history puts these at 1.0-2.8 GiB each (`test_common` the largest).

| Runner | Result | Wall time, median (range) | Peak memory, median | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j6`) | **failed**, 3 of 3 | failed after 125-127 s | 6.0 GiB | 2 per run |
| ninja `-j3`, hand-tuned (best) | 3 of 3 ok | **338 s** (335–344) | 4.6 GiB | 0 |
| ninja `-j4`, hand-tuned | **failed**, 3 of 3 | failed after 92-102 s | 6.0 GiB | 1 per run |
| ninja `-j5`, hand-tuned | **failed**, 3 of 3 | failed after 107-124 s | 6.0 GiB | 1 per run |
| **kiln, first run** (no memory history) | 3 of 3 ok | 360 s (358–422) | 5.0 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | 363 s (351–368) | 4.7 GiB | 0 |

Command: `bench/heavy.sh duckdb container -tuned 3,4,5 -cap 10m`, 17:36–19:04 IST (the rows' start times are UTC). The first two runs had a 6-minute cap; it was raised to 10 minutes once `-j3` was seen to take 338 s. No run reached either cap. kiln rows as of `e0c8f40`, rerun from 21:44 to 22:24; with the earlier scheduler they were 522 s (455–563) and 366 s (341–367).

What this shows: here only one of the four Ninja settings tried finishes at all, and kiln with no flags never ran out of memory. But it is a little slower than that one setting: 360 s first and 363 s learned against 338 s, about 7%. Its first run used to be much slower (522 s). `-d sched` showed why: CMake gives every DuckDB target its own compile rule, so a unit with no history was predicted from all compiles seen so far, at their maximum; once the 2.8 GiB `test_common` finished, every other unit was predicted at 3 GiB and they ran one at a time. Since `e0c8f40` that kind-wide guess is the median, while edges of the same rule keep the cautious 90th percentile. The claim this supports is safety, not speed: kiln is the only no-flags runner that finishes, within 10% of the best hand-tuned `-j`.

The image needed ccache: DuckDB's build runs every compile through `/usr/bin/ccache` (with `CCACHE_DISABLE=1` it calls the compiler straight through). Without it the first attempt failed at once; those runs were not results and were dropped, and the bench tool now refuses to record a run that fails within 5 s.

## Cold full builds (2026-10-23)

The whole default target (`all`: libraries, Python modules and test binaries, ccache off) from clean, **one run per setting**. The hand-tuned `-j` is the best value found in the heaviest-compiles runs above, not swept again. Ninja's default runs under a cap; if it fails or the guard stops it, that is its result.

| Build | Ninja default | Ninja, hand-tuned | kiln, first run | kiln, learned run |
|---|---|---|---|---|
| RDKit, laptop (12 threads, 11.7 GiB) | stopped by the guard at 27 min (11.1 GiB) | `-j7`: 2,073 s (34.6 min) | 1,924 s (**7% faster**) | 1,887 s (**9% faster**) |
| RDKit, container (4 CPUs, 6 GiB) | stopped at 48 min: 6 GiB limit, one OOM kill | `-j4`: 2,625 s (43.8 min) | 2,944 s (12% slower) | 3,028 s (15% slower) |
| DuckDB, container (4 CPUs, 6 GiB) | stopped at 2 min: 6 GiB limit | `-j3`: 1,718 s (28.6 min) | 1,753 s (2% slower) | 1,695 s (1% faster) |

No run, kiln's or hand-tuned, had an OOM kill; the guard and the default runs are the only failures. Peak memory: RDKit laptop 7.6 / 8.4 / 9.1 GiB (hand-tuned / kiln first / learned), RDKit container 4.7 / 4.6 / 4.6 GiB, DuckDB container 4.5 / 5.0 / 4.7 GiB.

Commands: `bench/heavy.sh rdkit-full laptop -tuned 7 -rounds 1 -budget 0 -series full -cap 90m`, `bench/heavy.sh rdkit-full container -tuned 4 ... -cap 2h`, `bench/heavy.sh duckdb-full container -tuned 3 ... -cap 3h`. Run 18:55 to 02:44 IST on 23-24 October; raw rows in `bench/results/rdkit-full.jsonl` and `bench/results/duckdb-full.jsonl`.

What this shows: the safety result holds for whole builds, not only for their heaviest compiles. Where Ninja needs a hand-picked `-j` to finish at all (both containers), kiln finishes without one. On speed it depends: kiln beats the best hand-tuned Ninja on the laptop, ties it on DuckDB, and is slower on RDKit in the container. The 12-15% there is more than the 5-10% run-to-run spread seen in the repeated heaviest-compiles runs, but with one run per setting it is not established, and I did not investigate it. A learned run was not faster than a first run in the container (3,028 against 2,944 s), so there the history did not help; on the laptop it helped by 2%.

Limits specific to these runs:
- **One run per setting, so no ranges.** The comparisons above are single measurements.
- **The `-j` values were not swept on the full builds.** A `-j` that is best for the heaviest compiles may not be best for a whole build; I did not test that.
- **Three setup problems were found by the runs and fixed**, each by a run failing at once or at the end, and those attempts are not in the tables: DuckDB's build runs a Python from `~/.local` (mounted into the container now), RDKit's ChemDraw links expat and freetype (added to the image), and the first full DuckDB `-j3` build was lost to the first of these after 27 minutes.
- Not done at the time: cold full builds of LLVM (done later on the server, below), LLVM in the container, DuckDB on the laptop.

## After the scheduler fixes (2026-10-24 night)

Two changes, `0763588` and `c14f5f2`:
- **Default `-j` under a CPU quota.** It is now the quota, with no + 2.
- **Reservations over time.** A job with history counts as reaching its peak by a third of its last run and being done by 1.25 times it, and its own estimate gets no 10% margin.

Rerun in the same profiles, interleaved with the best hand-tuned Ninja; medians, with the range in brackets.

| Benchmark | Best Ninja | kiln first run | kiln learned | Before the fixes (first / learned) |
|---|---|---|---|---|
| DuckDB's 12 heaviest compiles, 4-CPU / 6 GiB container, 3 rounds | `-j3` 322 s (320–333) | 379 s (373–533) | **300 s** (300–321) | 360 / 363 s vs `-j3` 338 s |
| RDKit full build, same container, 1 run | `-j4` 2,608 s | **2,562 s** | **2,513 s** | 2,944 / 3,028 s vs 2,625 s |
| LLVM's 7 heaviest compiles, laptop, 3 rounds | `-j4` 150 s (148–151) | 161 s (161–180) | 155.5 s (149–156) | 164 / 155 s vs 142 s |

- **Learned runs:** kiln is now about 7% faster than the best hand-tuned Ninja on DuckDB's compiles and 4% faster on RDKit's whole build. It is 3% slower on LLVM's compiles, where it was 9% slower before.
- **First runs:** these have no history, so only the `-j` change applies. That is enough for RDKit in the container. They are still behind on DuckDB's compiles (+18%) and LLVM's (+7%), because the 1.5 GiB first guess is either too high for DuckDB's units or too low for LLVM's 2 GiB ones.
- **Discarded runs:** the first DuckDB series after the fixes (`kiln-c14f5f2`) ran into a slowdown on the Windows side that capped even Ninja's runs. It was discarded, and the clean series is `kiln-c14f5f2-b`.

## Server profile: a shared 32-thread machine

Cold full builds of DuckDB, LLVM and RDKit on a borrowed server: a Xeon Silver 4314 (16 cores, 32 threads), 251 GiB of RAM, with the trees on an ext4 HDD, shared with other people's work. With this much memory nothing runs out, so these runs compare speed only, on a big machine.

Results (2026-10-24, 3 rounds of each setting, every run clean, no OOM kills; medians, with the range in brackets):

| Cold full build | Ninja default (`-j26`) | Ninja `-j20` | Ninja `-j24` | kiln, first run | kiln, learned |
|---|---|---|---|---|---|
| DuckDB (699 commands) | 322 s | 335 s | 320 s | **279 s** (279–280) | **280 s** (279–280) |
| LLVM + clang (4,392) | 1,160 s | 1,228 s | 1,154 s | 1,154 s | 1,153 s |
| RDKit (1,911) | 489 s | 511 s | 486 s | 486 s | 486 s |
| Peak memory, DuckDB / LLVM / RDKit | 22 / 26 / 21 GiB | 18 / 20 / 17 GiB | 20 / 24 / 20 GiB | 33 / 39 / 25 GiB | 33 / 39 / 25 GiB |

- kiln with no flags matches the best hand-tuned `-j` on LLVM and RDKit, and is 13% faster on DuckDB.
- Runs are very steady: each setting's three runs lie within about 1% of one another.
- kiln's peaks are higher. With plenty of memory it starts DuckDB's and LLVM's big compiles together, where Ninja's order spreads them out. This machine allowed that, but a smaller one would not.
- The DuckDB gain is consistent, but I have not investigated its cause.
- Peaks are the summed RSS of each run's own processes (`-peak tree`), an upper bound.
- Afterwards `ninja -n` finds only always-run edges in each tree (DuckDB's 2, RDKit's `inchi_support`), so kiln left Ninja's state intact.
- kiln was `a036408`. Raw rows are in `bench/results/server-*.jsonl`.

The runs are meant to go unnoticed by the machine's other users:

- **CPUs.** Every process is pinned to 24 CPUs, 12 cores with both of their threads (`taskset -c 0-11,16-27`). The other 4 cores are never used. Everything runs at `nice 19`. Under this mask Ninja's and kiln's default `-j` is 26.
- **A supervisor.** `tools/benchq` runs the queue in `bench/server/queue.txt` one step at a time, as the leader of its own session, and refuses to start without the mask and the priority. Its watchdog stops the running step, which records nothing and runs again later, in these cases:
  - less than 64 GiB of memory is available
  - the 8 CPUs left to others stay over 85% busy for 15 minutes
  - less than 200 GiB of disk is free

  It also holds the next step while I/O pressure is high, fails a step whose own processes pass 96 GiB, raises an alert when a build stops making progress, and kills anything a step leaves behind.
- **Everything in one directory,** with a conda env for RDKit's Boost and Python, the pinned Ninja 1.13.2 and CMake 4.4.4 from `scripts/tools.sh`, and caches redirected there.

To run it, on the server: `bench/server/start.sh` starts the supervisor detached, `bench/server/stop.sh` stops it, and `benchq -root ROOT status` shows where it is. On the laptop, `bench/server/watch.sh` waits for the next event that needs attention. `bench/heavy.sh PROJECT server` takes `KILN_CORPUS`, `KILN_BIN` (a pinned kiln) and `KILN_RESULTS`. Its peak memory is the run's own process tree (`-peak tree`), not the whole machine's.

## Reproducing the memory benchmark

`tools/bench` runs one set of targets from clean with these runners: `ninja-default` (Ninja at its default `-j`), `ninja-tuned` (`-jN` for each hand-tuned value in `-tuned`), `kiln-first` (an empty memory history) and `kiln-learned` (the history its `kiln-first` left). It records wall time, the system's peak memory use, and OOM kills. A watchdog kills a run that leaves less than `-guard` MiB, so an unthrottled Ninja can't take the machine down (the run is reported as aborted), and `-cap` kills a run that takes too long. Both kill the run's whole process tree.

A series repeats every runner `-rounds` times, rotating the order each round so that no runner always goes first. Each row is appended to `-out` when its run ends, and runs already there are skipped, so a series can be stopped and continued; an interrupted run (Ctrl-C) is killed and not recorded. `-budget 15m` runs only the next chunk, the pending runs that earlier runs say will fit in 15 minutes. `-plan` prints the chunks, and `-summary` the median and range over the rounds. Each round's `kiln-first` clears kiln's memory history, and its `kiln-learned` starts from a snapshot of what that run left (kept in `.kiln/bench/`), so splitting a series into chunks doesn't change what kiln learned from.

`bench/heavy.sh PROJECT PROFILE` runs this on a project's heaviest compiles (`bench/NAME-targets.txt`) in kiln's corpus trees:

```bash
bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned -plan   # the chunks
bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned         # the next chunk; repeat
bench/heavy.sh rdkit laptop -summary
bench/heavy.sh rdkit container -tuned 3,4,5 -only ninja-tuned,kiln-first,kiln-learned
```

The `laptop` profile runs on the host with a 600 MiB guard. `container` runs in the `kiln-bench` image (`docker build -f bench/Dockerfile -t kiln-bench bench`) with 4 CPUs, 6 GiB and no swap, where a real OOM kill is the result. The defaults are 3 rounds, 15-minute chunks, a 6-minute cap and the series name `rounds`; flags after the profile override them. Rows go to `bench/results/NAME.jsonl`.

For a link-heavy project, build LLVM with `-DLLVM_PARALLEL_LINK_JOBS=N` as the hand-tuned setting (`corpus/corpus.sh` does this through `LLVM_CMAKE_ARGS`); the point of the comparison is that kiln needs no such setting. For DuckDB, add a row for its documented workaround, `GEN= make`.

Each row of a table should state the machine profile and the exact command; the JSON lines in `bench/results/` carry the same fields.
