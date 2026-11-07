# Run files

The raw results behind [RESULTS.md](../RESULTS.md), from the final runs on 2026-11-07/08.

- `<name>.json`: k6 `--summary-export` (every metric, including per-endpoint and per-status sub-metrics).
- `<name>.result.json`: the same run reduced by `go run ./loadtest/report` (RPS, p50/p95/p99 per endpoint, status counts, custom counters).

| Files | What |
|---|---|
| `rush-run{1,2,3}` | §1 rush, 3 runs |
| `capacity-<rate>-run{1,2,3}` | §2 ladder, 400–800 actions/s, 3 runs per step |
| `capacity-{300,1000}-run1` | §2 ladder, single runs |
| `capacity-{300,500}-recheck-run{1,2,3}` | §2 re-runs of the two steps that failed between passing steps |
| `capacity-steps-*.md` | the step tables as `capacity.sh` printed them |
| `cpu-clock-during-recheck.txt` | CPU clock (MHz) and `% Processor Performance` once a minute during the re-run (throttling evidence) |
| `spike-run{1,2,3}` | §3 spike, 3 runs (run 2 failed the recovery rule) |
| `soak-10min-run1` | §4 10-minute soak |
| `chaos-<name>` + `chaos-<name>-timeline.txt` | §5 the background load of each chaos test, and its Prometheus timeline |
| `bottleneck600-{before,after}-run{1..9}` | §6 before/after at 600 actions/s, 9 runs each |
| `ab800-{before,after}-pair{1,2,3}` | §6 interleaved A/B at 800 actions/s |
| `environment.txt` | `loadtest/env.sh` output for the final runs |
