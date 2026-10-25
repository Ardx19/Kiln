# Changelog

Dates are 2026. The project is developed in milestones (M0-M9); see [docs/plans/](docs/plans/).

## v0.1.0 (not yet tagged)

The first release. Everything below is implemented and tested.

- **Engine (M1).** A Go port of Ninja's manifest parser, dependency scan, build plan and command runner, sharing `.ninja_log` (v5/v6/v7) and `.ninja_deps` (v4) with whichever Ninja a tree uses, so switching tools never rebuilds. Critical-path scheduling weighted by logged durations; Ninja-identical status line; a warning when the wall clock steps back during a build; a faster scan than Ninja's on very large manifests.
- **Drop-in mode (M2).** Run as `ninja` (`kiln shim DIR`), kiln is Ninja 1.13.2's command line and the `-t` tools generators call; CMake (both Ninja generators) and Meson drive it.
- **Memory-aware scheduling (M3).** Jobs start only while their predicted peak memory fits a budget derived from the host and the container's cgroup; learned per output in `.kiln/mem.log`; a brake that stops and reruns the youngest job instead of letting the kernel kill one; a retry for OOM-killed jobs. A job with history counts as reaching its peak by a third of its last run and being done by 1.25 times it, so jobs whose peaks won't meet run together. Under a cgroup CPU quota (a container's `--cpus`) the default `-j` is the quota, not CPUs + 2.
- **Detached builds (M4).** `kiln build --detach`, `status`, `wait`, `log`, `stop`; one build per tree; survives SIGHUP.
- **Transactional install (M6).** `kiln install` with per-file ownership records, `--swap`, `--dry-run`, staging for root-owned prefixes, and crash-safe commits.
- **Declared environment and `kiln test` (M7).** `[env]` applied to every command and `kiln exec`; changing it rebuilds; JUnit-based tests with an expected-failure list.
- **Shared cache (M8).** Managed ccache settings, optional injection, hit-rate reports and `kiln cache stats|trim`.
- **Benchmarks (M5).** Cold full builds and heaviest compiles of RDKit, LLVM and DuckDB on a laptop, in a 4-CPU / 6 GiB container and on a 32-thread server, against Ninja's default and the best hand-tuned `-j`: [docs/benchmarks.md](docs/benchmarks.md). Tooling: `tools/bench`, `bench/heavy.sh`, and `tools/benchq` with `bench/server/` for unattended runs on a shared machine.
- **Licence.** Apache-2.0.
