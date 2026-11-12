# Screenshots

This folder holds the images used in the main README. They come from **real runs** of the stack (the dashboard is the project's "UI"); nothing is edited or cropped.

| File | What it shows | Made by |
|---|---|---|
| `dashboard-overview.png` | All demos: the Correctness row (all green), traffic and latency | `make screenshots` |
| `dashboard-fail-closed.png` | `chaos/redis-down.sh` under load: the `lock_unavailable` spike and Redis acquire errors | `make screenshots` |
| `dashboard-worker.png` | The worker demos: job runs, seconds since last success, outbox, money vs refunds | `make screenshots` |
| `prometheus-alerts.png` | All 9 alert rules | `make screenshots` |
| `architecture-detailed.png` | The architecture as built | `make images` (from [src/architecture.html](src/architecture.html)) |
| `social-preview.png` | GitHub social preview (1280×640) | `make images` (from [src/social-preview.html](src/social-preview.html)) |

**`make screenshots`** ([src/screenshots.sh](src/screenshots.sh)) runs the worker demos, the request-path demos and `chaos/redis-down.sh` under load, then captures with a headless Chromium (Playwright in Docker, 1920×1080, Grafana kiosk mode, dark theme). It fails if a captured panel shows "No data", an error, or is still loading. **`make images`** only re-renders the two HTML sources.

## Taking them by hand

### How to produce the data

```bash
make up && make seed && make reset-data
./demo/observability.sh          # race, payment timeout, Redis stop/start (about 1 minute)
```

For the worker panels, optionally also run the worker demos (they need the short demo timings):

```bash
make up-demo && make reset-data
./demo/worker-reconcile.sh && ./demo/outbox-retry.sh && ./demo/abandon.sh
make up                          # back to normal timings afterwards
```

### Screenshots to take

Use a browser window of **1920×1080**, the dashboard time range **"Last 15 minutes"**, and wait for one 5s refresh.

| File | URL | What to show |
|---|---|---|
| `dashboard-overview.png` | http://localhost:3001/d/seatlock-reliability | The whole dashboard from the top: the Correctness row all green (0 double bookings, every invariant 0), traffic and latency below |
| `dashboard-correctness.png` | same, Correctness row | Close-up of the invariant stat panels: "verify.sql as live gauges" |
| `dashboard-fail-closed.png` | same, scroll to "Booking outcomes" and "Dependencies" | Right after `observability.sh`: the `lock_unavailable` spike in "Holds by outcome" and `acquire error` in "Redis lock operations": fail-closed, visible |
| `dashboard-worker.png` | same, scroll to "Worker" | After the worker demos: reconcile outcomes, outbox backlog draining, seconds since last success per job |
| `prometheus-alerts.png` | http://localhost:9095/alerts | The alert rules list (all green / inactive), showing `DoubleBookingDetected` and the others |
| `prometheus-alert-firing.png` *(optional)* | http://localhost:9095/alerts | Stop Redis (`docker compose stop redis`), send a few holds, wait 1 minute: `RedisLockUnavailable` firing. Then `docker compose start redis` |

## Phase 6: load test and chaos screenshots

The list of benchmark and chaos screenshots (which run, which time window, which panels) is in [docs/benchmarks/RESULTS.md](../benchmarks/RESULTS.md#grafana-screenshots-to-take). Save them here with the file names given there.

Tips:
- Grafana opens without a login (anonymous viewer). Press `Esc` to leave panel view, and use the browser's full-screen (F11) for a clean shot.
- To hide the Grafana side menu, add `?kiosk` to the dashboard URL: http://localhost:3001/d/seatlock-reliability?kiosk
