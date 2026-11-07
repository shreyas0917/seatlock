// rush.js — THE headline test: a flash sale.
//
// 5,000 booking attempts arrive at 500/s for 10 s on ONE show with 100
// seats ('Load: Rush'). Each attempt is a different user: pick a seat, hold
// it, and on 201 confirm it (pay) with its own Idempotency-Key.
//
// OPEN MODEL (constant-arrival-rate): k6 STARTS a new attempt every 2 ms
// whether or not earlier ones have finished, like real users who don't wait
// for each other. A closed model (N VUs looping) would hide slowness: when
// the server slows down, each VU sends its next request later, so the load
// drops exactly when it hurts and the measured latency looks better than
// what users see. That bias is called COORDINATED OMISSION.
// If k6 runs out of VUs it drops iterations (dropped_iterations): run.sh
// fails the run then, so "5,000 attempts" is always true.
//
// Correct answers: hold 201 / 409 (SEAT_HELD, SEAT_ALREADY_BOOKED), confirm
// 201. Any 503, 5xx or other status is UNEXPECTED (nothing is broken here).
// Pass (checked by loadtest/run.sh after the run): exactly 100 bookings,
// unexpected_status == 0, verify.sql 0 rows, gateway charges == bookings +
// refunds, Prometheus invariant gauges all 0.
import { baseOptions, STRICT, findShows, seatIds, userId, holdSeat, confirm, newIdempotencyKey, pick } from './lib.js';

const RATE = Number(__ENV.RUSH_RATE || 500); // attempts per second
const DURATION = __ENV.RUSH_DURATION || '10s'; // 500/s x 10 s = 5,000 attempts

export const options = baseOptions({
  scenarios: {
    rush: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Number(__ENV.RUSH_VUS || 600),
      maxVUs: Number(__ENV.RUSH_MAX_VUS || 1500),
    },
  },
  thresholds: {
    unexpected_status: ['count==0'],
    dropped_iterations: ['count==0'],
  },
});

export function setup() {
  const shows = findShows('Load: Rush');
  if (shows.length !== 1) throw new Error('show "Load: Rush" not found: run `make seed-load`');
  const seats = seatIds(shows[0].id);
  if (seats.length !== 100) throw new Error(`expected 100 seats, got ${seats.length}`);
  // Popular seats: the first 20 (front rows). 70% of attempts want one of
  // them; 30% pick any seat, so every seat is wanted by someone.
  return { showId: shows[0].id, seats, popular: seats.slice(0, 20) };
}

export default function (data) {
  const seat = Math.random() < 0.7 ? pick(data.popular) : pick(data.seats);
  const user = userId();
  const h = holdSeat(data.showId, seat, user, STRICT);
  if (h.status === 201) {
    confirm(h.holdId, user, newIdempotencyKey(user), STRICT, h.started, 0);
  }
}
