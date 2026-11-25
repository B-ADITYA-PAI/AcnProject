#!/usr/bin/env python3
"""Run the policy x skew x pool-size x churn experiment matrix.

Two modes:
  --mode native  Go binaries on localhost (no Docker, no regional delay). Needs
                 `go build` to have produced ./bin/{lb,backend,churn,quickgen}
                 (this script builds them if missing). Good for fairness / churn /
                 affinity results and for quick checks. Works on Windows, Linux, macOS.
  --mode docker  The full stack from docker-compose.lb.yml (traffic generator ->
                 switch with regional delay/loss -> LB -> backends). Use this for
                 latency results.

Each run writes results/<name>/{gen,lb,pool,backend,churn}.csv and meta.json.
Then:  python analysis/analyze.py results

Examples
  python scripts/run_sweep.py --mode native --quick
  python scripts/run_sweep.py --mode docker --duration 60 --trials 3
  python scripts/run_sweep.py --policies rr consistent --alphas 1.2 --pools 5 --churns high
"""
import argparse
import itertools
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EXE = ".exe" if os.name == "nt" else ""
CHURN = {"none": 0, "low": 30, "high": 5}   # seconds between add/remove events


def build():
    (ROOT / "bin").mkdir(exist_ok=True)
    for c in ["lb", "backend", "churn", "quickgen"]:
        target = ROOT / "bin" / (c + EXE)
        if not target.exists():
            subprocess.run(["go", "build", "-o", str(target), f"./cmd/{c}"], cwd=ROOT, check=True)


def run_native(name, out: Path, p, env_extra):
    out.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, LOG_DIR=str(out), BACKEND_HOST="127.0.0.1", BACKEND_MAX="10",
               POOL_SIZE=str(p["pool"]), POLICY=p["policy"], SERVICE_TIME="30ms",
               LB_ADMIN="http://127.0.0.1:9100", BACKENDS_ADMIN="http://127.0.0.1:9200",
               CHURN_INTERVAL=f'{CHURN[p["churn"]]}s' if CHURN[p["churn"]] else "0",
               CHURN_WARMUP="3s", **env_extra)
    procs = []
    try:
        for exe in ["backend", "lb", "churn"]:
            procs.append(subprocess.Popen([str(ROOT / "bin" / (exe + EXE))], env=env,
                                          stdout=(out / f"{exe}.log").open("w"), stderr=subprocess.STDOUT))
            time.sleep(0.6)
        subprocess.run([str(ROOT / "bin" / ("quickgen" + EXE)), "-duration", f'{p["duration"]}s',
                        "-alpha", str(p["alpha"]), "-rate", str(p["rate"]), "-seed", str(p["seed"]),
                        "-out", str(out / "gen.csv")], check=True)
        time.sleep(1.0)
    finally:
        for pr in procs:
            pr.send_signal(signal.SIGTERM if os.name != "nt" else signal.CTRL_BREAK_EVENT) if pr.poll() is None else None
        time.sleep(0.5)
        for pr in procs:
            if pr.poll() is None:
                pr.kill()


def run_docker(name, out: Path, p, env_extra):
    out.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, RESULTS_DIR=str(out.resolve()), POLICY=p["policy"], POOL_SIZE=str(p["pool"]),
               ZIPF_ALPHA=str(p["alpha"]), DURATION=f'{p["duration"]}s', SEED=str(p["seed"]),
               CHURN_INTERVAL=f'{CHURN[p["churn"]]}s' if CHURN[p["churn"]] else "0", **env_extra)
    base = ["docker", "compose", "-f", str(ROOT / "docker-compose.lb.yml")]
    try:
        subprocess.run(base + ["up", "--abort-on-container-exit", "--exit-code-from", "trafficgen"],
                       cwd=ROOT, env=env, check=True,
                       stdout=(out / "compose.log").open("w"), stderr=subprocess.STDOUT)
    finally:
        subprocess.run(base + ["down", "-v"], cwd=ROOT, env=env, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mode", choices=["native", "docker"], default="native")
    ap.add_argument("--policies", nargs="+", default=["rr", "hash", "least", "consistent"])
    ap.add_argument("--alphas", nargs="+", type=float, default=[0.8, 1.0, 1.2])
    ap.add_argument("--pools", nargs="+", type=int, default=[5, 10])
    ap.add_argument("--churns", nargs="+", default=["none", "low", "high"], choices=list(CHURN))
    ap.add_argument("--duration", type=int, default=300, help="seconds per run (proposal: 300)")
    ap.add_argument("--trials", type=int, default=5)
    ap.add_argument("--rate", type=float, default=50, help="requests/s per region (native mode)")
    ap.add_argument("--results", default="results")
    ap.add_argument("--quick", action="store_true", help="tiny sweep to check the setup works")
    a = ap.parse_args()

    if a.quick:
        a.alphas, a.pools, a.churns, a.duration, a.trials = [1.0], [5], ["none", "high"], 20, 1

    cells = list(itertools.product(a.policies, a.alphas, a.pools, a.churns, range(1, a.trials + 1)))
    est = len(cells) * (a.duration + 8) / 60
    print(f"{len(cells)} runs x ~{a.duration + 8}s = about {est:.0f} minutes ({est / 60:.1f} h)")
    if a.mode == "native":
        build()
    else:
        subprocess.run(["docker", "compose", "-f", str(ROOT / "docker-compose.lb.yml"), "build"], cwd=ROOT, check=True)

    results = ROOT / a.results
    for i, (policy, alpha, pool, churn, trial) in enumerate(cells, 1):
        name = f"{policy}_a{alpha}_n{pool}_c{churn}_t{trial}"
        out = results / name
        if (out / "meta.json").exists():
            print(f"[{i}/{len(cells)}] {name}: already done, skipping")
            continue
        if out.exists():
            shutil.rmtree(out)
        print(f"[{i}/{len(cells)}] {name}", flush=True)
        p = dict(policy=policy, alpha=alpha, pool=pool, churn=churn, trial=trial,
                 duration=a.duration, rate=a.rate, seed=1000 + trial, mode=a.mode)
        (run_native if a.mode == "native" else run_docker)(name, out, p, {})
        (out / "meta.json").write_text(json.dumps(p))   # written last: marks the run complete

    print(f"\ndone. Now run: python analysis/analyze.py {a.results}")


if __name__ == "__main__":
    main()
