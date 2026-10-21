# Changelog

Dates are 2026. The project is developed in milestones (M0-M9); see [docs/plans/](docs/plans/).

## Unreleased (heading to v0.1.0)

Everything below is implemented and tested; no version has been tagged yet.

- **Engine (M1).** A Go port of Ninja's manifest parser, dependency scan, build plan and command runner, sharing `.ninja_log` (v5/v6/v7) and `.ninja_deps` (v4) with whichever Ninja a tree uses, so switching tools never rebuilds. Critical-path scheduling weighted by logged durations; Ninja-identical status line; a warning when the wall clock steps back during a build; a faster scan than Ninja's on very large manifests.
- **Drop-in mode (M2).** Run as `ninja` (`kiln shim DIR`), kiln is Ninja 1.13.2's command line and the `-t` tools generators call; CMake (both Ninja generators) and Meson drive it.
- **Memory-aware scheduling (M3).** Jobs start only while their predicted peak memory fits a budget derived from the host and the container's cgroup; learned per output in `.kiln/mem.log`; a brake that stops and reruns the youngest job instead of letting the kernel kill one; a retry for OOM-killed jobs.
- **Detached builds (M4).** `kiln build --detach`, `status`, `wait`, `log`, `stop`; one build per tree; survives SIGHUP.
- **Transactional install (M6).** `kiln install` with per-file ownership records, `--swap`, `--dry-run`, staging for root-owned prefixes, and crash-safe commits.
- **Declared environment and `kiln test` (M7).** `[env]` applied to every command and `kiln exec`; changing it rebuilds; JUnit-based tests with an expected-failure list.
- **Shared cache (M8).** Managed ccache settings, optional injection, hit-rate reports and `kiln cache stats|trim`.
- **Licence.** Apache-2.0.

Still to do before v0.1.0: the benchmark runs on RDKit, LLVM and DuckDB (see the plan, M5).
