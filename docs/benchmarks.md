# Benchmarks

This page says how to reproduce kiln's benchmark tables and records what has been measured. **The full headline runs (cold builds of RDKit, LLVM and DuckDB, on a laptop and in a small container) have not been run**: they take hours each and want an otherwise idle machine. A first result for RDKit's heaviest compiles in a small container is below, and everything needed to run the rest is here.

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
| **kiln, first run** (no memory history) | 3 of 3 ok | 144 s (139–206) | 8.0 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | **135 s** (132–208) | 9.2 GiB | 0 |

Command: `bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned`, run in four chunks between 15:05 and 15:50 IST.

What this shows on the laptop: kiln with no flags matches the best hand-tuned Ninja on its first run and is about 6% faster once it has learned the jobs (135 s against 143 s). That is a smaller margin than the 16% a single earlier run suggested (it compared against `-j6` at 160 s; sweeping `-j5` to `-j7` found a better Ninja). Within a round, which is the fairest comparison, the learned run beat the round's best Ninja by 8% in round 1 (132 s against 143 s) and tied it in round 3 (135 s against 136 s); in round 2 its runs fell in the slow spell below.

**The slow spell in round 2.** Four runs in a row, from 15:21 to 15:36 (round 2's `-j7`, kiln's first and learned runs, then `-j5`), were 12-58% slower than the same runners in rounds 1 and 3: 198, 206, 208 and 177 s. Nothing else was running in WSL (the load was the compiles; the Claude Code app took well under one core), so the cause is probably on the Windows side: another program, or heat throttling the CPU, which WSL can't see. It is why those ranges reach so high, and why medians rather than means are reported.

**Small runner: a container with 4 CPUs and 6 GiB (no swap)**, GCC 11.4, 3 runs per row in the same rotated rounds. Here nothing guards the run: running out of memory means a real OOM kill.

| Runner | Result | Wall time, median (range) | Peak memory, median | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j6`) | **failed** (a compile was OOM-killed); one run | 1,108 s | 6.0 GiB | 1 |
| ninja `-j3`, hand-tuned | 3 of 3 ok | 178 s (174–190) | 3.7 GiB | 0 |
| ninja `-j4`, hand-tuned (best) | 3 of 3 ok | 170 s (163–170) | 5.0 GiB | 0 |
| ninja `-j5`, hand-tuned | 3 of 3 ok | 185 s (182–192) | 5.7 GiB | 0 |
| **kiln, first run** (no memory history) | 3 of 3 ok | 181 s (173–188) | 4.6 GiB | 0 |
| **kiln, learned run** | 3 of 3 ok | **161 s** (157–179) | 4.5 GiB | 0 |

Command: `bench/heavy.sh rdkit container -tuned 3,4,5 -only ninja-tuned,kiln-first,kiln-learned`, run in four chunks between 15:51 and 16:41 IST (the rows' start times are the container's UTC).

What this shows, on both profiles: with no settings at all, kiln finishes where Ninja's default fails (an OOM kill in the container; on the laptop the guard had to stop it). Its first run, which has to guess, is about as fast as the hand-tuned settings (within 1% of the best on the laptop, 6% behind it in the container). Once it has learned the jobs, its median is 5-6% below the best hand-tuned Ninja on both (135 against 143 s, 161 against 170 s). That margin is within the run-to-run noise: compared within each round, the learned run beat the round's best Ninja twice, tied it twice and lost twice (in the laptop's round 2, where it fell in the slow spell, 208 s against `-j6`'s 144 s before it; and in the container's round 1, 179 s against 170 s). So the claim these numbers support is that kiln with no flags matches the best `-j` found by sweeping, and is a little faster once it has learned, not that it is clearly faster. The single runs measured first (11% and 16% faster) overstated it.

How it got there, because the first attempt was worse: my first laptop run had kiln start with a 768 MiB guess for a compile. About a dozen heavy compiles started together, each grew to 1.3 GiB, and kiln's brake (which then stopped one job a second) could not keep up, so the benchmark's guard had to stop the run. That is a real defect, not a benchmark artefact. The fixes are in the code the numbers above come from: the first guess for a compile is now 1.5 GiB (3 GiB for a link), and the brake stops as many young jobs as it takes to win back the low watermark. The container "first run" improved from 201 s to 180 s with the same change.

Limits, stated plainly:
- **Three runs per row**, on one machine (a Ryzen 5 5500U laptop) and one project. Default Ninja's rows are single runs, made earlier, since repeating a known failure costs 18 minutes in the container. With three runs, a difference of a few percent is within the noise; the ranges show how much. "Best hand-tuned" is the best of the values swept: `-j5` to `-j7` on the laptop, `-j3` to `-j5` in the container.
- **A subset, not a cold full build.** The full RDKit build is still to be run, as are LLVM and DuckDB.
- **Ordering and page cache.** The page cache can't be dropped without root, so runs after the first in a chunk find the sources and headers cached. The rotation gives every runner each position once over three rounds.
- **Peak memory** is the system's (laptop) or the container's working set (container), sampled twice a second, and includes file cache the compiles read. The learned laptop runs used more memory than the hand-tuned ones (9.2 against 8.3 GiB) because they keep more jobs running; the guard never fired.
- ccache was disabled. Raw numbers, one JSON line per run with its start time: `bench/results/rdkit-heavy.jsonl`. Reproduce with `bench/heavy.sh rdkit laptop` or `bench/heavy.sh rdkit container` (below).

## Reproducing the memory benchmark

`tools/bench` runs one set of targets from clean with these runners: `ninja-default` (Ninja at its default `-j`), `ninja-tuned` (`-jN` for each hand-tuned value in `-tuned`), `kiln-first` (an empty memory history) and `kiln-learned` (the history its `kiln-first` left). It records wall time, the system's peak memory use, and OOM kills. A watchdog kills a run that leaves less than `-guard` MiB, so an unthrottled Ninja can't take the machine down (the run is reported as aborted), and `-cap` kills a run that takes too long. Both kill the run's whole process tree.

A series repeats every runner `-rounds` times, rotating the order each round so that no runner always goes first. Each row is appended to `-out` when its run ends, and runs already there are skipped, so a series can be stopped and continued; an interrupted run (Ctrl-C) is killed and not recorded. `-budget 15m` runs only the next chunk, the pending runs that earlier runs say will fit in 15 minutes. `-plan` prints the chunks, and `-summary` the median and range over the rounds. Each round's `kiln-first` clears kiln's memory history, and its `kiln-learned` starts from a snapshot of what that run left (kept in `.kiln/bench/`), so splitting a series into chunks doesn't change what kiln learned from.

`bench/heavy.sh PROJECT PROFILE` runs this on a project's heaviest compiles (`bench/PROJECT-heavy-targets.txt`) in kiln's corpus trees:

```bash
bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned -plan   # the chunks
bench/heavy.sh rdkit laptop -tuned 5,6,7 -only ninja-tuned,kiln-first,kiln-learned         # the next chunk; repeat
bench/heavy.sh rdkit laptop -summary
bench/heavy.sh rdkit container -tuned 3,4,5 -only ninja-tuned,kiln-first,kiln-learned
```

The `laptop` profile runs on the host with a 600 MiB guard. `container` runs in the `kiln-bench` image (`docker build -f bench/Dockerfile -t kiln-bench bench`) with 4 CPUs, 6 GiB and no swap, where a real OOM kill is the result. The defaults are 3 rounds, 15-minute chunks, a 6-minute cap and the series name `rounds`; flags after the profile override them. Rows go to `bench/results/PROJECT-heavy.jsonl`.

For a link-heavy project, build LLVM with `-DLLVM_PARALLEL_LINK_JOBS=N` as the hand-tuned setting (`corpus/corpus.sh` does this through `LLVM_CMAKE_ARGS`); the point of the comparison is that kiln needs no such setting. For DuckDB, add a row for its documented workaround, `GEN= make`.

Each row of a table should state the machine profile and the exact command; the JSON lines in `bench/results/` carry the same fields.
