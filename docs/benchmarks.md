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

**Small runner: a container with 4 CPUs and 6 GiB (no swap)**, GCC 11.4:

| Runner | Result | Wall time | Peak memory | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j6`) | **failed** | 1,108 s | 6.0 GiB | 1 |
| ninja `-j3`, hand-tuned | ok | 198 s (rerun: 200 s) | 3.8 GiB | 0 |
| ninja `-j4`, hand-tuned | ok | 175 s | 5.1 GiB | 0 |
| **kiln, first run** (no memory history) | ok | 180 s | 3.7 GiB | 0 |
| **kiln, learned run** | ok | **156 s** | 4.5 GiB | 0 |

**Laptop: 12 threads, 11.7 GiB, WSL2** (a memory guard stops any run that leaves under 600 MiB free, because a real OOM can freeze the machine):

| Runner | Result | Wall time | Peak memory | OOM kills |
|---|---|---|---|---|
| ninja, default (`-j14`) | **stopped by the guard** (it would have run out of memory) | 28 s | 10.3 GiB | n/a |
| ninja `-j6`, hand-tuned | ok | 160 s | 8.4 GiB | 0 |
| **kiln, first run** | ok | 147 s | 7.7 GiB | 0 |
| **kiln, learned run** | ok | **135 s** | 9.1 GiB | 0 |

What this shows: with no settings at all, kiln finishes where default Ninja fails, and once it has learned the jobs it beats the best hand-tuned setting I tried on both profiles (by 11% in the container and 16% on the laptop). Its first run, which has to guess, is already about as fast as the hand-tuned runs.

How it got there, because the first attempt was worse: my first laptop run had kiln start with a 768 MiB guess for a compile. About a dozen heavy compiles started together, each grew to 1.3 GiB, and kiln's brake (which then stopped one job a second) could not keep up, so the benchmark's guard had to stop the run. That is a real defect, not a benchmark artefact. The fixes are in the code the numbers above come from: the first guess for a compile is now 1.5 GiB (3 GiB for a link), and the brake stops as many young jobs as it takes to win back the low watermark. The container "first run" improved from 201 s to 180 s with the same change.

Limits, stated plainly:
- **One run per row**, on one machine (a Ryzen 5 5500U laptop) and one project, so differences under ~10% are noise. The container's hand-tuned `-j3` repeated within 1%. "Best hand-tuned" means the best of the values I tried (`-j3` and `-j4` in the container, `-j6` on the laptop); I did not sweep further.
- **A subset, not a cold full build.** The full RDKit build, LLVM and DuckDB are still to be run, as is a second project in the container profile.
- **Ordering.** In the container the hand-tuned reruns ran *after* the kiln runs, so page-cache warmth does not favour kiln. On the laptop the hand-tuned `-j6` ran before kiln's runs.
- **Peak memory** is the system's (laptop) or the container's working set (container), sampled twice a second, and includes file cache the compiles read. The learned laptop run used more memory than the hand-tuned one (9.1 vs 8.4 GiB) because it keeps more jobs running; the guard never fired.
- ccache was disabled. Raw numbers: `bench/results/rdkit-heavy.jsonl`. Reproduce with `bench/rdkit-heavy.sh 6g 3` (container) or `tools/bench` directly (laptop; use a guard).

## Reproducing the memory benchmark

`tools/bench` runs one set of targets from clean with four runners: `ninja-default` (Ninja at its default `-j`), `ninja-tuned` (`-jN`, the hand-tuned setting), `kiln-first` (an empty memory history) and `kiln-learned` (the history that run left). It records wall time, the system's peak memory use, and OOM kills, and prints a Markdown table. A watchdog kills a run that leaves less than `-guard` MiB, so an unthrottled Ninja can't take the machine down; such a run is reported as aborted.

```bash
go build -o .work/bench ./tools/bench && go build -o .work/kiln ./cmd/kiln

# Laptop profile (use the heavy targets, not a full cold build, to iterate)
CCACHE_DISABLE=1 .work/bench -dir .work/corpus/rdkit/build -ninja .tools/ninja/1.13.2/ninja \
  -kiln .work/kiln -tuned 6 -project rdkit -profile laptop -out bench/results.jsonl \
  -guard 1500 $(ninja -C .work/corpus/rdkit/build -t targets all | grep -E 'MolOps|rdmolfiles' | cut -d: -f1)

# Small-runner profile: the same inside a container with a memory limit
docker run --rm --cpus=4 --memory=8g --memory-swap=8g -v "$PWD:$PWD" -w "$PWD" kiln-ci \
  .work/bench -dir ... -profile container-4cpu-8g ...
```

For a link-heavy project, build LLVM with `-DLLVM_PARALLEL_LINK_JOBS=N` as the hand-tuned setting (`corpus/corpus.sh` does this through `LLVM_CMAKE_ARGS`); the point of the comparison is that kiln needs no such setting. For DuckDB, add a row for its documented workaround, `GEN= make`.

Each row of a table should state the machine profile and the exact command; the JSON lines in `bench/results.jsonl` carry the same fields.
