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

## What it means for M3

1. **The premise holds.** The same edge peaked within 0.4% of itself across runs. Per-output history is a sound basis for prediction, and the planned 10% margin is generous.
2. **Wrapper overhead is negligible for compiles.** The tree peak exceeds the largest single process by only 2–5 MiB (shell, ccache, g++ driver). Sampling the whole tree still matters for edges that run processes in parallel (LTO, `-pipe`), but `VmHWM` and `wait4` alone would be close here.
3. **The peak comes at the end.** It arrives at 83–99% of the job's runtime, so kiln has to hold a job's reservation until it exits. Shrinking a reservation early, based on what the job has used so far, would be unsafe.
4. **The OOM story checks out.** At about 1.3 GiB each, 14 concurrent heavy units need roughly 18 GiB against 11.7 GiB of RAM, while 6 need about 8 GiB. That matches the hand-tuned `-j6`.
5. **Logged durations depend on load.** Each compile took 54–96 s alone but 158–238 s in the last real build. That's fine for ranking edges by critical path, but those durations can't be read as costs measured on an idle machine.

**Still open:** these are the slowest translation units, not typical ones. Before setting the default estimate for edges with no history, measure a sample of ordinary translation units and a large link. Linking clang is the obvious case once an LLVM tree exists.
