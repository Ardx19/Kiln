# Out-of-memory builds

## Problem

Ninja's default parallelism is CPUs + 2. On a 12-thread laptop with 11.7 GiB of RAM that's 14 jobs, and a large C++ tree has translation units that each need over 1 GiB. When enough of them run together, the kernel OOM-kills a compiler and the build fails. The usual workaround is a hand-tuned `-j`, or a project-specific knob such as LLVM's `LLVM_PARALLEL_LINK_JOBS`.

## Approach

kiln learns each edge's peak memory from past runs and only starts a job while the predicted total fits a budget. The design is in M3 of the [plan](../plans/2026-10-21-feat-kiln-build-runner-plan.md). It rests on one premise: **an edge's peak memory is stable from run to run**. The M0 spike tested that premise.

## Spike (M0)

**Method.** [`spikes/memrss`](../../spikes/memrss/main.go) took the 8 slowest object files from the `.ninja_log` of an RDKit tree (`rdkit-props-batch-v2/build`). For each one it:

- got the compile command from `ninja -t commands`
- pointed `-o` and `-MF` at scratch files, so the tree wasn't touched
- ran the command twice, one at a time, with `CCACHE_DISABLE=1`

During each run it sampled the process tree every 50 ms and recorded three peaks: the summed RSS of the tree, the largest per-process `VmHWM`, and `wait4`'s `ru_maxrss`.

**Machine:** Ryzen 5 5500U (6 cores / 12 threads), 11.7 GiB RAM, WSL2 kernel 5.15, GCC 11.4, Release build.

| Translation unit | Wall s (run 1, 2) | Tree peak MiB (run 1, 2) | Spread | Max VmHWM MiB | wait4 MiB | Peak at | Logged s in last real build |
|---|---|---|---|---|---|---|---|
| `GraphMol/molopstest.cpp` | 96, 96 | 1094, 1091 | 0.3% | 1089 | 1089 | 99% | 238 |
| `Wrap/MolOps.cpp` | 89, 88 | 1466, 1466 | 0.0% | 1464 | 1464 | 83% | 203 |
| `ChemReactions/testReaction.cpp` | 81, 78 | 929, 933 | 0.4% | 931 | 931 | 99% | 197 |
| `Wrap/rdmolfiles.cpp` | 80, 81 | 1343, 1342 | 0.0% | 1340 | 1341 | 99% | 195 |
| `SubstructLibrary/Wrap/SubstructLibraryWrap.cpp` | 84, 78 | 1342, 1343 | 0.1% | 1341 | 1342 | 99% | 176 |
| `FileParsers/test1.cpp` | 66, 59 | 869, 872 | 0.4% | 870 | 870 | 96% | 158 |
| `FileParsers/file_parsers_catch.cpp` | 55, 61 | 902, 904 | 0.3% | 902 | 902 | 98% | 158 |
| `Wrap/Mol.cpp` | 54, 57 | 1331, 1332 | 0.0% | 1330 | 1330 | 99% | 158 |

"Peak at" is when the tree peak occurred, as a fraction of wall time. Raw samples are written to `spikes/memrss/out/results.jsonl`, which isn't committed.

## What kiln does now (M3)

kiln samples each job's process group from `/proc` every 200 ms (summing the whole tree, so `-pipe` and LTO count) and keeps wait4's `ru_maxrss` as a floor for spikes between ticks. A job's peak goes into `.kiln/mem.log` as a guarded maximum: a bigger run raises the estimate at once, a smaller one only lowers it by a quarter of the gap, and only if a compiler backend really ran, so a ccache hit can't erase what a real compile needs. Edges with no history are predicted from similar ones.

Each round the scheduler starts ready edges in critical-path order while the predicted memory of running jobs plus the new one fits the budget. Smaller edges may start ahead of a blocked bigger one until it has waited 10 s, then it gets a reservation and the next free slot (so it can't starve). If memory runs short anyway, kiln stops the youngest job and runs it again later; a job the kernel OOM-killed is retried once at 1.5 times its peak.

The sampler costs about 0.2% of a core with 14 jobs. Behaviour is checked by a simulation test of the admission rule (1,000 random workloads: the budget is never exceeded by prediction, nothing starves, `-j` and `--min-jobs` hold) and by `tests/compat/mem_test.go`, including a 2 GiB container where Ninja at `-j8` is OOM-killed and kiln finishes.

## What it means for M3

1. **The premise holds.** The same edge peaked within 0.4% of itself across runs. Per-output history is a sound basis for prediction, and the planned 10% margin is generous.
2. **Wrapper overhead is negligible for compiles.** The tree peak exceeds the largest single process by only 2–5 MiB (shell, ccache, g++ driver). Sampling the whole tree still matters for edges that run processes in parallel (LTO, `-pipe`), but `VmHWM` and `wait4` alone would be close here.
3. **The peak comes at the end.** It arrives at 83–99% of the job's runtime, so kiln has to hold a job's reservation until it exits. Shrinking a reservation early, based on what the job has used so far, would be unsafe. (Since 2026-10-24 kiln does use the timing the other way: a job with history is expected to reach its peak by a third of its usual run, and not to need it after 1.25 times that run, so jobs whose peaks won't overlap may run together. See DESIGN.md.)
4. **The OOM story checks out.** At about 1.3 GiB each, 14 concurrent heavy units need roughly 18 GiB against 11.7 GiB of RAM, while 6 need about 8 GiB. That matches the hand-tuned `-j6`.
5. **Logged durations depend on load.** Each compile took 54–96 s alone but 158–238 s in the last real build. That's fine for ranking edges by critical path, but those durations can't be read as costs measured on an idle machine.

**Still open:** these are the slowest translation units, not typical ones. Before setting the default estimate for edges with no history, measure a sample of ordinary translation units and a large link. Linking clang is the obvious case once an LLVM tree exists.

## A real occurrence (2026-10-22)

While LLVM 23.1.4 (clang, X86, Release) was being rebuilt at `-j8` on the 11.7 GiB laptop, `SemaOpenMP.cpp` was OOM-killed (`c++: fatal error: Killed signal terminated program cc1plus`) and the build stopped. That run used a kiln without memory scheduling, and other memory-hungry tests were running on the machine too, which is the everyday way this happens. Resuming the same tree with the current kiln, at the same `-j8` and with no other changes, finished the remaining 2,136 edges in 58 minutes with no failures: the scheduler held edges back 1,185 times and reserved memory for a big one 756 times, and nothing was killed. All 124 resulting binaries are byte-identical to Ninja's. This is one run, not a benchmark (the machine was shared); the controlled comparison is the M5 benchmark.
