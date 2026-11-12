# SeatLock: 2-minute demo video script

A screen recording you can do in one take after a dry run. Everything shown is live; nothing is edited in.

## Screen layout (1920×1080)

```
┌──────────────────────────────┬──────────────────────────────────────────┐
│ A. Terminal: k6 / commands    │ B. Browser: Grafana                      │
│    (font 20+ pt, dark)        │    "SeatLock — Reliability", kiosk mode  │
├──────────────────────────────┤                                          │
│ C. Terminal: psql             │                                          │
└──────────────────────────────┴──────────────────────────────────────────┘
```
Left half: two terminals stacked (A on top, C below). Right half: the browser.

## Prepare (before you press record)

```bash
make up-bench                 # stack with the realistic 50-150 ms gateway
make seed                     # show 1 (used by the single hold in step 3)
make seed-load                # rush show (100 seats) + other load data
make reset-data && curl -s -X POST localhost:8095/debug/reset    # free every seat, clear the gateway
```

- **Grafana:** open http://localhost:3001/d/seatlock-reliability?kiosk, time range **"Last 5 minutes"**, refresh **5s**, browser zoom **90%** so rows 1–3 fit.
- **Terminal A:** `cd` into the repo; have these ready in the shell history:
  1. `RUNS=1 SKIP_WARMUP=1 ./loadtest/run.sh rush`
  2. `docker compose kill redis`
  3. `curl -s -X POST localhost:8080/v1/holds -H 'X-User-ID: demo' -d '{"show_id":1,"seat_id":1}'; echo`
  4. `docker compose start redis`
- **Terminal C:** in the repo, with this typed but not run: `docker compose exec -T postgres psql -U seatlock -d seatlock < demo/verify.sql`.
- **Do one full dry run**, then reset again (the `make reset-data` line above).
- Turn on Do Not Disturb, hide the taskbar and bookmarks bar, close other tabs. Record at 1080p, 30 fps.

## Timeline (about 2:00)

| Time | Show | Say (simple English) |
|---|---|---|
| **0:00–0:10** The problem | Grafana, Correctness row (all green) | "When tickets for a big show go on sale, thousands of people click 'book' at the same second. The one thing a booking system must never do is sell the same seat twice, or charge someone twice." |
| **0:10–0:30** The architecture | Scroll Grafana to the top; then point at the panels as you name them | "This is SeatLock, a booking backend in Go. Redis is only a fast lock that turns away people who lost the race. PostgreSQL is the authority: a unique constraint makes a second booking for a seat impossible. Payments are idempotent, and a background worker finishes anything a crash leaves behind." |
| **0:30–1:00** Rush, live | Terminal A: run command 1. Then Grafana: requests per second jump, the 409 wall, hold latency | "Now a flash sale: five thousand booking attempts in ten seconds, on one show with one hundred seats." *(wait for the summary)* "Exactly one hundred bookings and one hundred charges. Everyone else got a 409: 'this seat is taken'. That's the correct answer, not an error. Hold latency stayed in milliseconds." |
| **1:00–1:25** Kill Redis | Terminal A: command 2, then command 3 (shows 503 `LOCK_UNAVAILABLE`), then command 4. Grafana: the `lock_unavailable` spike in "Holds by outcome", the Redis errors | "Now I kill Redis in the middle of it." *(run 2, 3)* "New holds are refused with a 503: the system fails closed. It would rather say 'try again' than risk selling a seat without the lock. People who already hold a seat can still pay, because payment only uses PostgreSQL." *(run 4)* "Redis is back, and holds work again on their own." |
| **1:25–1:40** Prove it | Terminal C: run the `verify.sql` command; every check prints `(0 rows)`. Grafana Correctness row still green | "Did anything go wrong? These eleven checks look for double bookings, money taken without a seat, refunds that never happened. Every one returns zero rows, and the same checks run every fifteen seconds as the live gauges you see here." |
| **1:40–2:00** Close | Browser: the GitHub repo page | "Load tests, five chaos tests and a CI gate that fails the build on any double booking. All numbers are from one laptop, and the method is in the repo. It's all at github.com/shreyas0917/seatlock. Thanks for watching." |

## Tips
- **Do a dry run first**; the rush takes ~15 s with checks, so you may trim the wait in editing or talk over it.
- **1080p, large font** (terminal 20 pt or more), dark terminal theme, Grafana in its default theme.
- **No notifications**: Do Not Disturb on, chat apps closed.
- Speak slowly; it's fine to pause while a command runs.
- If anything fails during the take, `make reset-data`, then start again from 0:00.
