// spike.js — sudden 10x traffic and back. The same realistic mix as
// capacity.js, with ramping-arrival-rate (open model):
//
//   0-30 s      baseline   SPIKE_BASE actions/s
//   30-35 s     ramp up to 10 x SPIKE_BASE
//   35-65 s     spike      10 x SPIKE_BASE for 30 s
//   65-70 s     ramp down
//   70-150 s    recovery   SPIKE_BASE again (the "phase" tag "recovery"
//                          starts at 90 s, after 20 s of settling)
//
// STRICT: any 503 (LOCK_UNAVAILABLE or SERVICE_BUSY) is unexpected.
// Pass (run.sh): unexpected_status == 0, recovery p95(hold) back near the
// baseline p95, verify.sql 0 rows, invariants 0.
import exec from 'k6/execution';
import { baseOptions, STRICT, steadyShows, mixedAction } from './lib.js';

const BASE = Number(__ENV.SPIKE_BASE || 60);
const PEAK = BASE * Number(__ENV.SPIKE_FACTOR || 10);

const phaseThresholds = {};
for (const p of ['baseline', 'spike', 'recovery']) {
  for (const e of ['seats', 'hold', 'confirm', 'cancel']) phaseThresholds[`http_req_duration{endpoint:${e},phase:${p}}`] = ['max>=0'];
  phaseThresholds[`unexpected_status{phase:${p}}`] = ['count>=0'];
}

export const options = baseOptions({
  scenarios: {
    spike: {
      executor: 'ramping-arrival-rate',
      startRate: BASE,
      timeUnit: '1s',
      preAllocatedVUs: Math.max(100, Math.ceil(PEAK / 2)),
      maxVUs: Math.max(300, PEAK * 3),
      stages: [
        { target: BASE, duration: '30s' },
        { target: PEAK, duration: '5s' },
        { target: PEAK, duration: '30s' },
        { target: BASE, duration: '5s' },
        { target: BASE, duration: '80s' },
      ],
    },
  },
  thresholds: Object.assign({ unexpected_status: ['count==0'], status_500: ['count==0'] }, phaseThresholds),
});

export function setup() {
  return { shows: steadyShows(), start: Date.now() };
}

function phaseAt(seconds) {
  if (seconds < 30) return 'baseline';
  if (seconds < 70) return 'spike';
  if (seconds < 90) return 'settling';
  return 'recovery';
}

export default function (data) {
  exec.vu.metrics.tags.phase = phaseAt((Date.now() - data.start) / 1000);
  mixedAction(data.shows, STRICT, 0);
}
