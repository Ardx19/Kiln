# Long builds tied to a terminal

## Problem

A cold build of a large tree takes hours. Closing the terminal, losing an SSH connection, or an IDE restarting its shell kills the build. The usual workaround is `setsid nohup ninja … &`, followed by `pgrep` and `tail -f` to find out what happened.

## Approach

`kiln build --detach` re-executes kiln as a daemon in a new session (`setsid`) with stdin from `/dev/null`. The daemon holds the build lock, and `status`, `wait`, `log -f` and `stop` work through the lock and a status file. The design is in M4 of the [plan](../plans/2026-10-21-feat-kiln-build-runner-plan.md).

## Spike (M0)

[`spikes/detach/heartbeat.sh`](../../spikes/detach/heartbeat.sh) starts a `setsid` process that writes a timestamp every 5 s. `check` reports whether it's alive and the largest gap between beats.

| Case | How it was tested | Result on this machine |
|---|---|---|
| The session gets SIGHUP (terminal closed, SSH dropped) | `hangup-test`: starts a stand-in session that launches the heartbeat plus an ordinary `sleep &`, then `pkill -HUP -s <session>` | **Survives.** The ordinary background job is killed and the heartbeat keeps beating |
| Logout with logind `KillUserProcesses=yes` | Read the logind configuration | Not set here (systemd's default is `no`) and `Linger=no`, so logout doesn't kill other sessions' processes. On distributions that enable it, the daemon must start through `systemd-run --user --scope`, or the user must enable lingering (`loginctl enable-linger`) |
| Every WSL window closed (WSL may stop the distro) | **Manual, still to do:** see below | — |

### The manual WSL check

This machine runs WSL 2.3.26 with systemd enabled.

1. Start a 30-minute heartbeat: `spikes/detach/heartbeat.sh start 30`
2. Close **every** WSL window, including VS Code remote windows and the Claude app. Anything still attached to WSL keeps the distro running and spoils the test.
3. Wait at least 5 minutes, then open a WSL terminal.
4. Run `spikes/detach/heartbeat.sh check`.

How to read the result:

- **Largest gap about 5 s:** the distro kept running, and a plain `setsid` is enough.
- **One large gap:** WSL stopped or froze the distro. M4 then needs a documented keep-alive (for example a Windows-side `wsl.exe` process) or `instanceIdleTimeout`-style settings. Check the current WSL documentation for the exact option name before relying on it.
