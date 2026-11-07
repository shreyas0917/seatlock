// smoke.js — the CI load smoke test: a small rush that must stay CORRECT.
//
// 200 booking attempts arrive at 20/s for 10 s on ONE show with 20 seats
// ('Load: Smoke'), open model like rush.js. Every attempt is a different
// user: hold a random seat, on 201 confirm it.
//
// The build FAILS unless (checked by loadtest/run.sh): exactly 20 bookings,
// unexpected_status == 0, gateway charges == bookings + refunds,
// verify.sql 0 rows. A double booking can't hide: it would be a 21st
// booking, a second charge, or a verify.sql row.
import { baseOptions, STRICT, findShows, seatIds, userId, holdSeat, confirm, newIdempotencyKey, pick } from './lib.js';

export const options = baseOptions({
  scenarios: {
    smoke: {
      executor: 'constant-arrival-rate',
      rate: 20,
      timeUnit: '1s',
      duration: '10s',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  thresholds: {
    unexpected_status: ['count==0'],
    dropped_iterations: ['count==0'],
  },
});

export function setup() {
  const shows = findShows('Load: Smoke');
  if (shows.length !== 1) throw new Error('show "Load: Smoke" not found: run `make seed-load`');
  const seats = seatIds(shows[0].id);
  if (seats.length !== 20) throw new Error(`expected 20 seats, got ${seats.length}`);
  return { showId: shows[0].id, seats };
}

export default function (data) {
  const user = userId();
  const h = holdSeat(data.showId, pick(data.seats), user, STRICT);
  if (h.status === 201) {
    confirm(h.holdId, user, newIdempotencyKey(user), STRICT, h.started, 0);
  }
}
