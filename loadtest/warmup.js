// warmup.js — 15 s of light mixed traffic before every measured run, so the
// first measured requests don't pay for cold caches, new DB/Redis
// connections or Go's first allocations. Not measured: run.sh resets the
// data and the gateway again afterwards.
import { baseOptions, CAPACITY, steadyShows, mixedAction } from './lib.js';

export const options = baseOptions({
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.WARMUP_RATE || 100),
      timeUnit: '1s',
      duration: __ENV.WARMUP_DURATION || '15s',
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
});

export function setup() {
  return { shows: steadyShows() };
}

export default function (data) {
  mixedAction(data.shows, CAPACITY, 0);
}
