#!/usr/bin/env python3
"""Offline analysis for the L4 load-balancer experiments (Python is used here only,
never in the measured data path).

Input : a results directory containing one sub-directory per run, each with
        gen.csv, lb.csv, pool.csv and meta.json (written by scripts/run_sweep.py).
Output: summary_runs.csv (one row per run), summary.csv (median / p95 across
        trials per configuration) and PNG plots, all written to <results>/report/.

Metrics (definitions follow the proposal, section 6):
  jain_median   Jain's Fairness Index of per-backend request counts, in sliding
                windows (WINDOW seconds, STEP seconds), counting every backend that
                was in the pool for the whole window. 1.0 = perfectly even.
  cv_median     coefficient of variation of the same counts (secondary check).
  p50/p95/p99   client-observed latency of successful TCP+HTTP requests, in ms.
  resets_per_churn_event
                LB-side failures (reset, dial_fail, no_reply) divided by the
                number of backend removals. 0 when there was no churn.
  remap_rate    share of requests whose client IP went to a different backend than
                that client's previous request (loss of affinity).
"""
import json
import sys
from pathlib import Path

import numpy as np
import pandas as pd

WINDOW = 5     # seconds
STEP = 1       # seconds
FAIL = {"reset", "dial_fail", "no_reply", "no_backend"}


def jain(x):
    x = np.asarray(x, dtype=float)
    s = x.sum()
    if len(x) == 0 or s == 0:
        return np.nan
    return s * s / (len(x) * (x * x).sum())


def membership(pool):
    """Return list of (ts_ms, set_of_backends_in_pool) after each pool event."""
    state, out = set(), []
    for _, r in pool.sort_values("ts_ms").iterrows():
        if r.event == "add":
            state = state | {r.backend}
        elif r.event == "remove":
            state = state - {r.backend}
        out.append((int(r.ts_ms), frozenset(state)))
    return out


def window_fairness(lb, pool):
    """Jain index and CV per sliding window."""
    ok = lb[lb.outcome == "ok"]
    if ok.empty:
        return pd.DataFrame(columns=["t", "jain", "cv"])
    t0 = int(ok.ts_ms.min())
    t1 = int(ok.ts_ms.max())
    events = membership(pool)
    rows = []
    for ws in range(t0, t1 - WINDOW * 1000, STEP * 1000):
        we = ws + WINDOW * 1000
        # backends in the pool for the whole window
        stable = None
        cur = frozenset()
        for ts, members in events:
            if ts <= ws:
                cur = members
        stable = set(cur)
        for ts, members in events:
            if ws < ts < we:
                stable &= set(members)
        if len(stable) < 2:
            continue
        w = ok[(ok.ts_ms >= ws) & (ok.ts_ms < we) & ok.backend.isin(stable)]
        counts = w.backend.value_counts().reindex(sorted(stable), fill_value=0).values
        if counts.sum() == 0:
            continue
        rows.append({"t": (ws - t0) / 1000, "jain": jain(counts), "cv": counts.std() / counts.mean()})
    return pd.DataFrame(rows)


def analyse_run(d: Path):
    meta = json.loads((d / "meta.json").read_text())
    gen = pd.read_csv(d / "gen.csv")
    lb = pd.read_csv(d / "lb.csv")
    pool = pd.read_csv(d / "pool.csv")

    fair = window_fairness(lb, pool)
    lat = gen[(gen.outcome == "ok") & gen.proto.isin(["tcp", "http"])].latency_ms
    removals = int(((pool.event == "remove") & (pool.reason == "churn")).sum())
    fails = int(lb.outcome.isin(FAIL).sum())
    tcp_http = gen[gen.proto.isin(["tcp", "http"])]

    row = dict(meta)
    row.update(
        requests=len(gen),
        client_fail_tcp_http=int((tcp_http.outcome != "ok").sum()),
        lb_failures=fails,
        churn_events=removals,
        resets_per_churn_event=(fails / removals) if removals else 0.0,
        remap_rate=float(lb.remap.mean()) if len(lb) else np.nan,
        jain_median=float(fair.jain.median()) if len(fair) else np.nan,
        cv_median=float(fair.cv.median()) if len(fair) else np.nan,
        p50=float(lat.quantile(0.50)) if len(lat) else np.nan,
        p95=float(lat.quantile(0.95)) if len(lat) else np.nan,
        p99=float(lat.quantile(0.99)) if len(lat) else np.nan,
    )
    return row


def p95(s):
    return s.quantile(0.95)


def main(results: Path):
    runs = sorted(p.parent for p in results.glob("*/meta.json"))
    if not runs:
        sys.exit(f"no runs found under {results}")
    df = pd.DataFrame([analyse_run(d) for d in runs])
    out = results / "report"
    out.mkdir(exist_ok=True)
    df.to_csv(out / "summary_runs.csv", index=False)

    keys = ["policy", "alpha", "pool", "churn"]
    metrics = ["jain_median", "cv_median", "p50", "p95", "p99", "resets_per_churn_event", "remap_rate"]
    agg = df.groupby(keys)[metrics].agg(["median", p95])
    agg.columns = [f"{m}_{s if isinstance(s, str) else 'p95'}" for m, s in agg.columns]
    agg["trials"] = df.groupby(keys).size()
    agg.reset_index().to_csv(out / "summary.csv", index=False)
    print(agg.reset_index().round(3).to_string(index=False))

    plot(df, out)
    print(f"\nwrote {out}/summary_runs.csv, summary.csv and plots")


def plot(df, out):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    order = [p for p in ["rr", "hash", "least", "consistent"] if p in set(df.policy)]

    def grouped_bars(metric, ylabel, title, fname, split, subset=None):
        d = df if subset is None else df[subset(df)]
        if d.empty:
            return
        levels = sorted(d[split].unique())
        fig, ax = plt.subplots(figsize=(7, 4))
        width = 0.8 / max(len(levels), 1)
        for i, lv in enumerate(levels):
            g = d[d[split] == lv].groupby("policy")[metric]
            med = g.median().reindex(order)
            hi = g.quantile(0.95).reindex(order)
            x = np.arange(len(order)) + i * width
            ax.bar(x, med.values, width, label=f"{split}={lv}", yerr=np.vstack([np.zeros(len(order)), (hi - med).values]),
                   capsize=3)
        ax.set_xticks(np.arange(len(order)) + width * (len(levels) - 1) / 2)
        ax.set_xticklabels(order)
        ax.set_ylabel(ylabel)
        ax.set_title(title)
        ax.legend()
        fig.tight_layout()
        fig.savefig(out / fname, dpi=150)
        plt.close(fig)

    grouped_bars("jain_median", "Jain fairness index (median, bar = p95)", "Load balance vs Zipf skew (no churn)",
                 "fairness_vs_skew.png", "alpha", lambda d: d.churn == "none")
    grouped_bars("remap_rate", "share of requests remapped", "Affinity loss by policy",
                 "remap_by_policy.png", "churn")
    grouped_bars("resets_per_churn_event", "failures per churn event", "Churn resilience",
                 "resets_per_churn_event.png", "churn", lambda d: d.churn != "none")
    grouped_bars("p99", "p99 latency, ms", "Tail latency (TCP+HTTP)", "p99_latency.png", "alpha")


if __name__ == "__main__":
    main(Path(sys.argv[1] if len(sys.argv) > 1 else "results"))
