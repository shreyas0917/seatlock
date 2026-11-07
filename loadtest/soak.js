// soak.js — a long run at a steady, moderate rate (about 50% of the knee):
// the realistic mix at SOAK_RATE actions/s for SOAK_DURATION (20 min by
// default; a short local version of a real multi-hour soak).
//
// A soak test looks for slow leaks that short tests can't see: heap or
// goroutines that keep growing, DB/Redis connections that are never given
// back, a backlog (outbox, PENDING payments) that grows. loadtest/run.sh
// compares Prometheus at the start and the end of the run.
//
// It books 10% of its holds (not 50%) so 20 minutes don't use up the seats.
// STRICT: at half the knee, no 503 should happen at all.
import { baseOptions, STRICT, steadyShows, mixedAction } from './lib.js';

const RATE = Number(__ENV.SOAK_RATE || 200);

export const options = baseOptions({
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.SOAK_DURATION || '20m',
      preAllocatedVUs: Math.max(50, Math.ceil(RATE / 2)),
      maxVUs: Math.max(200, RATE * 3),
    },
  },
  thresholds: {
    unexpected_status: ['count==0'],
    status_500: ['count==0'],
  },
});

export function setup() {
  return { shows: steadyShows() };
}

export default function (data) {
  mixedAction(data.shows, STRICT, 0, 0.1);
}
