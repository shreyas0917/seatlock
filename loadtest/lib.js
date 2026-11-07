// Shared helpers for the SeatLock k6 scripts (rush, capacity, spike, soak,
// smoke, and the chaos runs).
//
// Every request is tagged with `endpoint` (seats | hold | confirm | cancel)
// and classified by the SCENARIO's policy:
//
//   expected   a correct answer. 409 SEAT_HELD / SEAT_ALREADY_BOOKED is the
//              system saying "someone else got this seat": CORRECT, not an error.
//   shed       503 SERVICE_BUSY: the API refusing work it cannot do in time
//              (correct overload behaviour). Only capacity.js accepts it.
//   unexpected anything else (any 500, a 503 where none should happen, a
//              network error...). The pass criteria require 0.
//
// http.setResponseCallback(http.expectedStatuses(...)) makes k6's own
// http_req_failed agree: only statuses outside the expected list count as
// failed.
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Trend } from 'k6/metrics';

export const BASE = __ENV.BASE_URL || 'http://api:8080';
const RUN_ID = __ENV.RUN_ID || `${Date.now()}`;

// ---- custom metrics -------------------------------------------------------
export const holdsCreated = new Counter('holds_created');
export const seatConflicts = new Counter('seat_conflicts'); // 409 SEAT_HELD / SEAT_ALREADY_BOOKED
export const bookingsOk = new Counter('bookings_ok');
export const confirm402 = new Counter('confirm_402');
export const confirm410 = new Counter('confirm_410');
export const confirm504 = new Counter('confirm_504');
export const lockUnavailable503 = new Counter('lock_unavailable_503');
export const shed503 = new Counter('shed_503'); // 503 SERVICE_BUSY
export const unexpectedStatus = new Counter('unexpected_status');
export const status500 = new Counter('status_500'); // a 500 is ALWAYS a bug
export const confirmRetries = new Counter('confirm_retries');
export const responses = new Counter('responses'); // tagged endpoint + status
export const holdToBooked = new Trend('hold_to_booked', true); // hold request start -> booking answer

// The status breakdown per endpoint. k6 only exports tagged sub-metrics that
// have a threshold, so each combination gets an always-true one.
const ENDPOINTS = ['seats', 'hold', 'confirm', 'cancel'];
const STATUSES = ['0', '200', '201', '400', '402', '404', '409', '410', '422', '500', '502', '503', '504'];

// baseOptions returns the options every script shares, merged with its own.
export function baseOptions(extra) {
  const thresholds = {};
  for (const e of ENDPOINTS) {
    thresholds[`http_req_duration{endpoint:${e}}`] = ['max>=0'];
    for (const s of STATUSES) thresholds[`responses{endpoint:${e},status:${s}}`] = ['count>=0'];
  }
  return Object.assign(
    {
      summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)', 'count'],
      // k6 must not become the bottleneck: response bodies are DISCARDED by
      // default (a 200-seat map is ~15 KB that k6 would otherwise keep and
      // process) and read only where the script needs them (hold id, error
      // codes): those requests set responseType 'text'.
      discardResponseBodies: true,
    },
    extra,
    { thresholds: Object.assign(thresholds, (extra && extra.thresholds) || {}) },
  );
}

// ---- identities -----------------------------------------------------------
// A unique user per VU AND iteration: every attempt is a different person.
export function userId() {
  return `u-${RUN_ID}-${exec.scenario.name}-${exec.vu.idInTest}-${exec.scenario.iterationInTest}`;
}
// A unique Idempotency-Key per confirm (a retry reuses it on purpose).
export function newIdempotencyKey(user) {
  return `k-${user}`;
}

// ---- policies -------------------------------------------------------------
// STRICT: rush, spike, smoke. Every 503 is unexpected.
export const STRICT = { acceptShed: false, acceptLockUnavailable: false, accept504: false };
// CAPACITY: 503 SERVICE_BUSY is shed (reported separately), not unexpected.
export const CAPACITY = { acceptShed: true, acceptLockUnavailable: false, accept504: false };

const EXPECTED = {
  seats: [200],
  hold: [201, 409],
  confirm: [201],
  cancel: [200],
};

function errorCode(res) {
  try {
    return res.json('error.code') || '';
  } catch (_) {
    return '';
  }
}

let loggedUnexpected = 0; // per VU

// classify records the status metrics and returns 'expected' | 'shed' | 'unexpected'.
function classify(endpoint, res, policy) {
  const status = res.status;
  let code = status >= 400 ? errorCode(res) : '';
  // Seat-map bodies are discarded (see baseOptions), so their error code is
  // unknown. On reads SeatLock's ONLY 503 is SERVICE_BUSY (reads never touch
  // Redis, and nginx answers 502, not 503, when an upstream is down).
  if (endpoint === 'seats' && status === 503 && code === '') code = 'SERVICE_BUSY';
  responses.add(1, { endpoint, status: String(status) });

  if (status === 500) status500.add(1, { endpoint });
  if (status === 503 && code === 'LOCK_UNAVAILABLE') lockUnavailable503.add(1, { endpoint });
  if (status === 503 && code === 'SERVICE_BUSY') shed503.add(1, { endpoint });
  if (endpoint === 'confirm') {
    if (status === 402) confirm402.add(1);
    if (status === 410) confirm410.add(1);
    if (status === 504) confirm504.add(1);
  }

  let verdict = 'unexpected';
  if (EXPECTED[endpoint].includes(status)) {
    // A 409 on a hold must be a seat conflict, nothing else.
    if (endpoint === 'hold' && status === 409) {
      verdict = code === 'SEAT_HELD' || code === 'SEAT_ALREADY_BOOKED' ? 'expected' : 'unexpected';
      if (verdict === 'expected') seatConflicts.add(1);
    } else {
      verdict = 'expected';
    }
  } else if (status === 503 && code === 'SERVICE_BUSY' && policy.acceptShed) {
    verdict = 'shed';
  } else if (status === 503 && code === 'LOCK_UNAVAILABLE' && policy.acceptLockUnavailable) {
    verdict = 'expected';
  } else if (endpoint === 'confirm' && status === 504 && policy.accept504) {
    verdict = 'expected';
  }
  if (verdict === 'unexpected') {
    unexpectedStatus.add(1, { endpoint, status: String(status) });
    // The first few per VU go to the k6 log WITH their error code, so a
    // report can say exactly what users saw (not just "a 409").
    if (loggedUnexpected < 3) {
      loggedUnexpected++;
      console.warn(`unexpected: ${endpoint} -> ${status} ${code || '(no code)'} ${res.error || ''}`);
    }
  }
  return verdict;
}

// Statuses k6 itself treats as "not failed" (http_req_failed). Kept wide
// enough for every policy; the precise verdict is unexpected_status.
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// ---- API calls -------------------------------------------------------------
export function seatMap(showId, policy) {
  // The body is not needed (discarded); an error's code is not needed either:
  // a non-200 seat map is unexpected whatever its code.
  const res = http.get(`${BASE}/v1/shows/${showId}/seats`, {
    tags: { endpoint: 'seats', name: 'GET /v1/shows/{showID}/seats' },
  });
  classify('seats', res, policy);
  return res;
}

// holdSeat returns { status, holdId, started } (holdId only on 201).
export function holdSeat(showId, seatId, user, policy) {
  const started = Date.now();
  const res = http.post(`${BASE}/v1/holds`, JSON.stringify({ show_id: showId, seat_id: seatId }), {
    headers: Object.assign({ 'X-User-ID': user }, JSON_HEADERS),
    tags: { endpoint: 'hold', name: 'POST /v1/holds' },
    responseType: 'text',
  });
  classify('hold', res, policy);
  let holdId = 0;
  if (res.status === 201) {
    holdsCreated.add(1);
    holdId = res.json('hold_id');
  }
  return { status: res.status, holdId, started };
}

// confirm books a hold with Idempotency-Key `key`. With retries > 0 a
// confirm that got NO answer (network error, or 502/503/504 from a load
// balancer whose upstream died) is sent again with the SAME key: the API's
// idempotency makes the retry safe (never a second charge).
export function confirm(holdId, user, key, policy, started, retries) {
  const send = () =>
    http.post(`${BASE}/v1/bookings`, JSON.stringify({ hold_id: holdId }), {
      headers: Object.assign({ 'X-User-ID': user, 'Idempotency-Key': key }, JSON_HEADERS),
      tags: { endpoint: 'confirm', name: 'POST /v1/bookings' },
      responseType: 'text',
    });
  let res = send();
  for (let i = 0; i < (retries || 0) && isRetryable(res); i++) {
    confirmRetries.add(1);
    res = send();
  }
  classify('confirm', res, policy);
  if (res.status === 201) {
    bookingsOk.add(1);
    if (started) holdToBooked.add(Date.now() - started);
  }
  return res.status;
}

// No answer from SeatLock itself: a network error, or a proxy error page.
function isRetryable(res) {
  return res.status === 0 || res.status === 502 || res.status === 504 || (res.status === 503 && errorCode(res) === '');
}

export function cancel(holdId, user, policy) {
  const res = http.del(`${BASE}/v1/holds/${holdId}`, null, {
    headers: { 'X-User-ID': user },
    tags: { endpoint: 'cancel', name: 'DELETE /v1/holds/{holdID}' },
    responseType: 'text',
  });
  classify('cancel', res, policy);
  return res.status;
}

// ---- setup helpers (tagged "setup", excluded from every reported number) ----
export function findShows(prefix) {
  const res = http.get(`${BASE}/v1/shows`, { tags: { endpoint: 'setup' }, responseType: 'text' });
  if (res.status !== 200) throw new Error(`GET /v1/shows -> ${res.status}`);
  return res.json('shows').filter((s) => s.title.startsWith(prefix));
}

export function seatIds(showId) {
  const res = http.get(`${BASE}/v1/shows/${showId}/seats`, { tags: { endpoint: 'setup' }, responseType: 'text' });
  if (res.status !== 200) throw new Error(`GET seats of show ${showId} -> ${res.status}`);
  return res.json('seats').map((s) => s.id);
}

// steadyShows returns [{ id, first, count }] for the 200 'Load: Steady N'
// shows. Setup data is copied into EVERY VU, so it is kept tiny: each show's
// seat ids are a contiguous range (load.sql inserts them in one ordered
// statement), checked here, so a seat is first + random(count) instead of
// 40,000 ids parsed by hundreds of VUs.
export function steadyShows() {
  const shows = findShows('Load: Steady ');
  if (shows.length === 0) throw new Error('no "Load: Steady" shows: run `make seed-load`');
  return shows.map((s) => {
    const ids = seatIds(s.id).sort((a, b) => a - b);
    if (ids.length === 0 || ids[ids.length - 1] - ids[0] + 1 !== ids.length) {
      throw new Error(`seat ids of show ${s.id} are not contiguous: re-create the load data`);
    }
    return { id: s.id, first: ids[0], count: ids.length };
  });
}

function randomSeat(show) {
  return show.first + Math.floor(Math.random() * show.count);
}

export function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}

// One "user action" of the realistic mix (capacity, spike, soak):
//   70% read a seat map; 30% hold a random seat, then on 201 confirm
//   (confirmShare of the holds, default half) or cancel (the rest, so the
//   seat comes back). Booked seats never come back, so a long run (soak)
//   books a smaller share to not run out of the 40,000 seats.
export function mixedAction(shows, policy, confirmRetriesN, confirmShare) {
  const show = pick(shows);
  if (Math.random() < 0.7) {
    seatMap(show.id, policy);
    return;
  }
  const user = userId();
  const h = holdSeat(show.id, randomSeat(show), user, policy);
  if (h.status !== 201) return;
  if (Math.random() < (confirmShare === undefined ? 0.5 : confirmShare)) {
    confirm(h.holdId, user, newIdempotencyKey(user), policy, h.started, confirmRetriesN);
  } else {
    cancel(h.holdId, user, policy);
  }
}
