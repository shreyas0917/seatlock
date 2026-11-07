| Target rate (actions/s) | Achieved RPS | p50 / p95 / p99 hold (ms) | p95 seat map (ms) | p95 confirm (ms) | p95 cancel (ms) | Shed % | Unexpected % | Dropped | 500s | Within the knee rule? |
|---|---|---|---|---|---|---|---|---|---|---|
| 300 | 387 | 11.8 / 67.7 / 132.1 | 26.7 | 246.9 | 74.4 | 0.000 | 0.000 | 0 | 0 | yes |
| 500 | 620 | 204.3 / 954.8 / 1287.2 | 514.0 | 1924.6 | 969.2 | 0.000 | 0.000 | 1005 | 0 | no |

**Knee: 300 actions/s** (highest step with p95(hold) < 250 ms and shed + unexpected < 0.1%; medians of 3 runs)
