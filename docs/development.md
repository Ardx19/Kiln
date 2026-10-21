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

**Fixtures.** `go run ./tools/genfixture -gen cmake|meson|ninja -out DIR` writes the same synthetic project for each generator. Reports land in `.work/compat/<fixture>-<ninja>.report.json`.

**Two things the harness works around on purpose:**

- **Coarse file timestamps.** Edits use `UTIME_NOW`, like `touch(1)`, and wait 30 ms after each build, because the kernel stamps files from a clock that ticks every 10 ms on this machine.
- **Clock steps.** If the wall clock steps during a script, only the steps before it are compared and the script is retried. See [problems/clock-steps.md](problems/clock-steps.md).

Known Ninja behaviours that look like failures are listed in [ninja-quirks.md](ninja-quirks.md).

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
