// capacity.js — one STEP of the capacity ladder: a realistic mix at a FIXED
// arrival rate (CAP_RATE user actions per second) for CAP_DURATION.
// loadtest/capacity.sh runs it at increasing rates to find the knee.
//
// Mix (lib.js mixedAction), across 200 shows x 200 seats:
//   70% read a seat map; 30% hold a random seat, then on 201 confirm (half)
//   or cancel (half). That is about 64% reads, 27% holds, 5% confirms and
//   5% cancels by request; the report shows the real split.
//
// Open model (constant-arrival-rate), for the same reason as rush.js: the
// arrival rate must not drop when the server slows down (coordinated omission).
//
// Status accounting: 503 SERVICE_BUSY is SHED (the API refusing work it
// cannot finish in time: correct overload behaviour), reported separately.
// Everything else outside the expected answers is UNEXPECTED. A 500 at any
// step is a bug: the status_500 threshold fails the step.
import { baseOptions, CAPACITY, steadyShows, mixedAction } from './lib.js';

const RATE = Number(__ENV.CAP_RATE || 200);

export const options = baseOptions({
  scenarios: {
    capacity: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.CAP_DURATION || '60s',
      // Little's law: VUs needed = rate x time per action. Generous, so k6
      // itself never limits the rate (dropped_iterations shows if it did).
      preAllocatedVUs: Math.max(50, Math.ceil(RATE / 2)),
      maxVUs: Math.max(200, RATE * 3),
    },
  },
  thresholds: {
    status_500: ['count==0'],
  },
});

export function setup() {
  return { shows: steadyShows() };
}

export default function (data) {
  mixedAction(data.shows, CAPACITY, Number(__ENV.CONFIRM_RETRIES || 0));
}
