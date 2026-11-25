# L4 load balancer: how to run the experiments

This extends the Stream B harness (traffic generator + region switch) with the rest of the proposal:
the load balancer with four policies (Stream A), the backend pool and churn injector (Stream B),
and the metrics and sweep tooling (Stream C).

```
trafficgen -> switch (regional delay/loss, adds PROXY line) -> lb (policy) -> backends (5-10)
                                                                ^                ^
                                                                +--- churn ------+   (stop/start a backend, then tell the LB)
```

## Components

| Path | What it does |
|---|---|
| `cmd/lb`, `internal/lb` | The load balancer: TCP, HTTP and UDP forwarding; reads the PROXY line; picks a backend; logs every decision to `lb.csv` and every pool change to `pool.csv`. Admin API on `:9100`. |
| `internal/policy` | The four policies behind one interface: `rr`, `hash` (hash(src IP) mod N), `least` (fewest active connections), `consistent` (hash ring, 160 virtual nodes per backend). |
| `internal/pool` | The set of backends in rotation. Backend `i` uses ports `10000+10i` (TCP), `+1` (UDP), `+2` (HTTP). |
| `cmd/backend` | Up to 10 echo backends in one process. Each answers after `SERVICE_TIME` (default 30 ms) and reports its load on `:9200/load`. Stopping one resets its open connections. |
| `cmd/churn` | The churn injector: every `CHURN_INTERVAL` it alternately removes a random backend and adds it back. |
| `cmd/quickgen` | Docker-free traffic generator (no switch, so no regional delay). |
| `scripts/run_sweep.py` | Runs the policy x skew x pool size x churn matrix, with trials. |
| `analysis/analyze.py` | Computes the metrics and plots. |

## Run one experiment (full stack, with the regional switch)

```powershell
$env:POLICY="consistent"; $env:POOL_SIZE="5"; $env:ZIPF_ALPHA="1.2"
$env:CHURN_INTERVAL="5s"; $env:DURATION="60s"
docker compose -f docker-compose.lb.yml up --build --abort-on-container-exit --exit-code-from trafficgen
docker compose -f docker-compose.lb.yml down -v
```

Results land in `./results/` (`gen.csv`, `lb.csv`, `pool.csv`, `backend.csv`, `churn.csv`).

## Run the whole matrix

```powershell
python scripts/run_sweep.py --mode native --quick        # 1-minute check that everything works
python scripts/run_sweep.py --mode docker --duration 60 --trials 3
python analysis/analyze.py results
```

The proposal's full matrix is 4 policies x 3 skews x 2 pool sizes x 2 churn rates x 5 trials = 240 runs of
5 minutes, about 20 hours. The script prints the estimate first. Practical options: shorter runs
(`--duration 60`), fewer trials, or a smaller matrix (`--alphas 0.8 1.2 --pools 5 --churns none high`).
Report whatever you actually ran. The no-churn runs are the "none" rows.

`--mode native` needs only Go and Python (it works on Windows without Docker) but has no regional delay,
so use it for fairness, affinity and churn results. Use `--mode docker` for the latency numbers.

## Metrics (see the docstring in `analysis/analyze.py`)

- **Jain's Fairness Index** of per-backend request counts in sliding 5-second windows, with the coefficient of variation as a check.
- **p50 / p95 / p99** client latency of successful TCP and HTTP requests.
- **Failures per churn event**: requests the LB could not complete (backend reset, connection refused, no UDP reply) divided by the number of backend removals.
- **Remap rate**: the share of requests that went to a different backend than the same client IP's previous request. This is the loss of affinity.

Results are summarised as median and p95 across trials.

## Design decisions to mention in the report

- **The LB's flow key is the client source IP** (all that an L4 balancer can see). The traffic generator already turns hot keys into hot IPs, so key skew becomes IP skew.
- **Least-loaded uses the LB's own count of open connections** to each backend, which is exact, instead of a polled signal that would be stale. The backends still report their own count on `/load`.
- **Churn is a real failure.** The backend is stopped first, which resets its open connections. The LB learns about it `DETECT_DELAY` (500 ms) later, like a failed health check. Requests sent to the dead backend in that gap are the visible failures. The LB does not retry.
- **Backend service time (30 ms)** keeps connections open long enough for churn to catch some of them.
- **No eBPF/XDP path.** The proposal lists it as a stretch goal and names the userspace-only implementation as the fallback; this is the fallback.

## Known limits

- `quickgen` and the native mode have no regional delay or loss.
- Only 20 client IPs per region (80 in total), so hash-based policies cannot balance more finely than that.
- The 4-policy smoke test (one 20 s run each) only checks the pipeline. Its numbers are not results; run proper trials.
