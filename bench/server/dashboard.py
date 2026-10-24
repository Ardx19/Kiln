#!/usr/bin/env python3
"""Live dashboard of the server benchmark queue (read-only).

Shows progress bars for the whole queue, the current step and the current
run, the CPUs (ours and the ones left to others), memory, and the latest
events, refreshing every few seconds. It only reads files and /proc, so it
is safe to leave open. Run it on the server, through SSH with a terminal:

    ssh -t kiln-bench python3 -I ROOT/kiln/bench/server/dashboard.py

Ctrl-C quits. The run bar's length is a guess, from earlier runs of the
same project (or its warm-up), since bench reports a run only when it ends.
"""
import json
import os
import re
import statistics
import sys
import time

ROOT = os.environ.get("KILN_BENCH_ROOT") or os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
OURS = list(range(0, 12)) + list(range(16, 28))
RESERVED = list(range(12, 16)) + list(range(28, 32))
REFRESH = 3


def read(path, default=""):
    try:
        with open(path) as f:
            return f.read()
    except OSError:
        return default


def bar(frac, width=40):
    frac = max(0.0, min(1.0, frac))
    full = int(frac * width)
    return "[" + "#" * full + "-" * (width - full) + f"] {frac * 100:5.1f}%"


def cpu_times():
    t = {}
    for line in read("/proc/stat").splitlines():
        f = line.split()
        if f and re.fullmatch(r"cpu\d+", f[0]):
            v = list(map(int, f[1:9]))
            busy = v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
            t[int(f[0][3:])] = (busy, busy + v[3] + v[4])
    return t


def meminfo():
    m = {}
    for line in read("/proc/meminfo").splitlines():
        k, _, v = line.partition(":")
        m[k] = int(v.split()[0]) * 1024 if v.split() else 0
    return m


def queue():
    steps = []
    for line in read(f"{ROOT}/run/queue.txt").splitlines():
        line = line.split("#")[0]
        f = [x.strip() for x in line.split("|", 2)]
        if len(f) == 3 and f[0]:
            steps.append(f)
    try:
        state = json.loads(read(f"{ROOT}/run/state.json", "{}"))
    except ValueError:
        state = {}
    return steps, state


def rows(name):
    out = []
    for line in read(f"{ROOT}/results/{name}.jsonl").splitlines():
        try:
            r = json.loads(line)
        except ValueError:
            continue
        if r.get("series") == "server-full":
            out.append(r)
    return out


def ts(s):
    try:
        return time.mktime(time.strptime(s[:19], "%Y-%m-%dT%H:%M:%S"))
    except (ValueError, TypeError):
        return 0


def fmt(secs):
    secs = int(max(0, secs))
    return f"{secs // 3600}h{secs % 3600 // 60:02d}m" if secs >= 3600 else f"{secs // 60}m{secs % 60:02d}s"


def step_detail(step, st, now):
    """Lines about the running step: its runs and the current run's bar."""
    name, _, cmd = step
    m = re.search(r"heavy\.sh (\S+) server -rounds (\d+)", cmd)
    started = ts(st.get("start", ""))
    lines = [f"Step  {name}: running {fmt(now - started)}"]
    if not m:  # a warm-up: guess from earlier warm-ups' logs
        lines.append("      " + bar(0) + "  (warm-up: no estimate)")
        log = read(f"{ROOT}/results/logs/{name}.log")
        last = [l for l in log.splitlines() if l.startswith("[")][-1:]
        if last:
            done, total = re.match(r"\[(\d+)/(\d+)\]", last[0]).groups() if re.match(r"\[(\d+)/(\d+)\]", last[0]) else (0, 1)
            lines[-1] = "      " + bar(int(done) / int(total)) + f"  {done}/{total} commands"
        return lines
    proj, rnd = m.group(1), int(m.group(2))
    rs = rows(proj)
    this = [r for r in rs if r.get("round") == rnd]
    runs = 5
    lines.append("      " + bar(len(this) / runs) + f"  {len(this)}/{runs} runs of round {rnd}")
    for r in this:
        label = r["runner"] + (f" -j{r['jobs']}" if r.get("jobs") else "")
        ok = "ok" if r["exit"] == 0 and not r.get("aborted") and not r.get("capped") else "FAILED"
        lines.append(f"        {label:22s} {r['wall_s']:7.0f} s  {r['peak_mib'] / 1024:5.1f} GiB  {ok}")
    # The current run began when the last one ended, or when the step began.
    begin = started
    for r in rs:
        end = ts(r.get("start", "")) + r.get("wall_s", 0)
        if end > begin and end <= now:
            begin = end
    walls = [r["wall_s"] for r in rs if r["exit"] == 0]
    if not walls:
        w = re.search(r"built in (\d+)s", read(f"{ROOT}/results/logs/{proj.split('-')[0]}-warmup.log") or "")
        walls = [float(w.group(1))] if w else [600.0]
    est = statistics.median(walls)
    if len(this) < runs:
        el = now - begin
        lines.append(f"Run   {len(this) + 1} of {runs}: {fmt(el)} of about {fmt(est)}")
        lines.append("      " + bar(el / est))
    return lines


def main():
    prev = cpu_times()
    while True:
        time.sleep(REFRESH)
        now = time.time()
        cur = cpu_times()
        use = {c: (cur[c][0] - prev[c][0]) / max(1, cur[c][1] - prev[c][1]) for c in cur if c in prev}
        prev = cur
        steps, state = queue()
        done = sum(1 for s in steps if state.get(s[0], {}).get("status") == "done")
        out = [f"kiln-bench queue   {time.strftime('%H:%M:%S')}   (Ctrl-C quits)", ""]
        out.append(f"Queue " + bar(done / max(1, len(steps))) + f"  {done}/{len(steps)} steps")
        for s in steps:
            stt = state.get(s[0], {}).get("status", "pending")
            mark = {"done": "x", "running": ">", "failed": "!", "pending": " "}.get(stt, "?")
            wall = state.get(s[0], {}).get("wall_s")
            extra = f" {fmt(wall)}" if stt == "done" and wall else ""
            out.append(f"  [{mark}] {s[0]:14s}{extra}")
            if stt == "running":
                out[-1:] = [out[-1]] + ["    " + l for l in step_detail(s, state[s[0]], now)]
        hb = {}
        try:
            hb = json.loads(read(f"{ROOT}/run/heartbeat", "{}"))
        except ValueError:
            pass
        age = now - os.path.getmtime(f"{ROOT}/run/heartbeat") if os.path.exists(f"{ROOT}/run/heartbeat") else 1e9
        out += ["", f"Supervisor: {hb.get('state', '?')}" + ("" if age < 60 else f"   HEARTBEAT {fmt(age)} OLD")]
        out.append("")
        ours = sum(use.get(c, 0) for c in OURS) / len(OURS)
        res = sum(use.get(c, 0) for c in RESERVED) / len(RESERVED)
        out.append(f"CPU ours (0-11,16-27)    " + bar(ours, 30))
        out.append(f"CPU others (12-15,28-31) " + bar(res, 30))
        row = ""
        for c in sorted(use):
            row += f"{c:2d}{'*' if c in RESERVED else ' '}{'|' * int(use[c] * 8):8s} "
            if c % 8 == 7:
                out.append("  " + row)
                row = ""
        m = meminfo()
        total, avail = m.get("MemTotal", 1), m.get("MemAvailable", 0)
        gib = 1 << 30
        out.append("")
        out.append(f"RAM used  " + bar((total - avail) / total, 30) + f"  {(total - avail) / gib:5.1f} of {total / gib:.0f} GiB")
        out.append(f"RAM ours  " + bar(hb.get("own_gib", 0) * gib / total, 30) + f"  {hb.get('own_gib', 0):5.1f} GiB (cap 96)")
        out.append(f"Available {avail / gib:.0f} GiB (stop under 64)   load {read('/proc/loadavg').split()[0] if read('/proc/loadavg') else '?'}"
                   f"   disk free {hb.get('disk_free_gib', 0):.0f} GiB   I/O pressure {hb.get('io_some_avg60', 0):.0f}%")
        out += ["", "Recent events:"]
        for l in read(f"{ROOT}/run/events.log").splitlines()[-5:]:
            out.append("  " + l[11:19] + " " + l[26:][:110])
        sys.stdout.write("\033[H\033[2J" + "\n".join(out) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
