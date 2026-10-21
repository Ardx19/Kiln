# The wall clock steps (WSL)

## Problem

Build tools decide what to rebuild by comparing file mtimes, and the kernel stamps files from the wall clock. When the wall clock jumps backwards, a file written *later* can get an *older* timestamp. That causes two kinds of failure:

- **Missed rebuilds.** You save a header just after a backwards step. Its mtime is older than objects built before the step, so nothing rebuilds and the build silently uses stale code.
- **Endless rebuilds and spurious errors.** Outputs built just after a step look older than their inputs and rebuild on every run. Meson refuses to configure ("Clock skew detected. File … has a time stamp 0.15 s in the future").

## Evidence (this machine, 2026-10-21)

WSL 2.3.26, kernel 5.15.167.4, systemd enabled, `systemd-timesyncd` active (`NTP=yes`).

- `go run ./spikes/clock -d 90s` reported **a backwards step of about 1.8 s every 30 s** (−1.82 s, −1.74 s, −1.83 s).
- In the M0 compatibility harness, running the same Ninja twice on the same snapshot with the same edits gave different rebuild sets. Ninja's own `-d explain` showed an archive recorded as *older* than an object built before it. Every disagreement went away once the harness discarded steps that overlapped a clock step.
- One Meson configure in the harness failed with the clock-skew error above.
- **Later the same day, the direction flipped**, with nothing reconfigured. Sampling the wall clock against the monotonic clock for 70 s showed **forward steps of about 3.0 s every 32 s** (+2.97 s, +3.01 s), with no drift in between. A 40-second kiln build saw one of them.

Only backward steps break builds. A forward step leaves every file written after it newer than every file written before it, so no comparison changes. It can't put a file in the future either.

## Likely cause

Two time sources are fighting. WSL syncs the VM's clock from Windows, and `systemd-timesyncd` inside the distro also disciplines it with NTP. The regular 30-second sawtooth fits that. This is a hypothesis, not yet confirmed.

**To test it:** stop timesyncd and measure again.

```bash
sudo systemctl disable --now systemd-timesyncd
```

```bash
go run ./spikes/clock -d 120s
```

If the steps stop, the fix belongs in kiln's documentation as a WSL setup note. To undo it, run `sudo systemctl enable --now systemd-timesyncd`.

## What kiln does about it

- **Harness (done).** `tests/compat` watches the wall clock against the monotonic clock. It compares only steps that ran without a clock step, and retries the rest.
- **kiln (M1e, done).** kiln watches the clock the same way during a real build. The first time it steps back by 100 ms or more, kiln warns once, naming the size of the step and pointing here. Forward steps are ignored. Re-checking the affected edges' inputs is still an option for later.
- **M9 (stretch).** Content hashing makes rebuild decisions independent of the clock, which matters more on WSL than the plan first assumed.
