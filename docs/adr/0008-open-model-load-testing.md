# ADR-0008: Open-model load testing, and numbers only with their environment

**Status:** accepted (Phase 6)

## Context
Performance numbers on a résumé or README are easy to inflate by accident: a closed-loop test that hides slowness, 409s counted as errors (or errors hidden as 409s), a lucky best run, a number with no machine attached, a load generator that is itself the bottleneck.

## Decision
- **Open model:** k6 `constant-arrival-rate` / `ramping-arrival-rate`. Requests arrive on schedule whether or not earlier ones finished, so a slow server can't reduce its own load (no *coordinated omission*). `dropped_iterations` is reported, and the rush and smoke tests fail on any drop.
- **Classify every response:** *expected* (200/201, and 409 with a seat-conflict code), *shed* (`503 SERVICE_BUSY`, accepted only in the capacity test), *unexpected* (everything else; any 500 is always a bug).
- **Correctness after every run, not just speed:** time to consistency, exact booking counts, gateway charges == bookings + refunds, `verify.sql` 0 rows, fresh invariant gauges at 0.
- **Every run records its environment** (`loadtest/env.sh`: CPU, RAM, Docker limits, versions, pool sizes, gateway delay) and the load generator's own CPU and memory.
- **3 runs, median with min–max**; reset and warm-up before each; unrelated workloads paused; numbers worded exactly as measured ("5,000 booking attempts in a 10-second burst", never "5,000 concurrent users").
- **Compare A and B under the same conditions:** interleave them (`loadtest/ab.sh`).

## Consequences
- Found that **the load generator was the first bottleneck** (k6 at 494% CPU vs the API's 208%) before any number was trusted.
- An interleaved A/B reversed a misleading non-interleaved result (drift on a throttling laptop).
- Some results are reported as **failed or unexplained** (the 500 actions/s step, spike 2 of 3), and the quoted capacity is the conservative one.
- All numbers are explicitly **one laptop, local, not production capacity**.

## Alternatives considered
- **Closed model (N looping users):** simpler, but under-reports latency exactly when it matters.
- **Best-of-N or single runs:** easy to cherry-pick.
- **Reporting throughput only:** a fast wrong system would look good.

Details: [phase-6.md](../learning/phase-6.md), [RESULTS.md](../benchmarks/RESULTS.md).
