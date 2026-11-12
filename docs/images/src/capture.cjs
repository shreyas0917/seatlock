// capture.cjs — takes the README screenshots from the RUNNING stack with a
// headless Chromium (Playwright), so they are reproducible and show real data.
//
// Run by `make screenshots` (docs/images/src/screenshots.sh), which first runs
// the demos and records their time windows. Never edit or crop the output.
//
// Env: GRAFANA (http://grafana:3000), PROM (http://prometheus:9090), OUT (/out),
//      and the demo windows as unix seconds: WORKER_FROM, WORKER_TO, REQ_FROM, REQ_TO.
//
// How a shot is taken, like a person would look at the dashboard:
//   open it for the demo's time window (plus 1 minute either side), turn
//   auto-refresh OFF (a past window doesn't need it, and 26 panels refreshing
//   every 5 s never finish loading), collapse the rows ABOVE the one to show
//   (instead of scrolling, which misplaces Grafana's kiosk footer), wait
//   until no panel is loading, then check for "No data", errors and spinners.
// A shot with problems must be re-done (re-run its demo), not used.
const { chromium } = require('playwright-core');

const GRAFANA = process.env.GRAFANA || 'http://grafana:3000';
const PROM = process.env.PROM || 'http://prometheus:9090';
const OUT = process.env.OUT || '/out';
const ms = (s) => Number(s) * 1000;
const PAD = 60; // seconds of context on each side of the activity

const DASH = `${GRAFANA}/d/seatlock-reliability/seatlock-reliability?kiosk&theme=dark`;
const range = (from, to) => `&from=${ms(Number(from) - PAD)}&to=${ms(Number(to) + PAD)}`;

const shots = [
  {
    file: 'dashboard-overview.png',
    what: 'all demos, top of the dashboard: Correctness row, traffic and latency',
    url: DASH + range(process.env.WORKER_FROM, process.env.REQ_TO),
    collapse: [],
  },
  {
    // Taken from a chaos/redis-down.sh run (Redis killed 30 s under load), so
    // the fail-closed spike is visible: FAIL_FROM / FAIL_TO = that run's window.
    file: 'dashboard-fail-closed.png',
    what: 'chaos/redis-down.sh under load (Redis killed 30 s): Booking outcomes + Dependencies',
    url: DASH + range(process.env.FAIL_FROM || process.env.REQ_FROM, process.env.FAIL_TO || process.env.REQ_TO),
    collapse: [1, 2],
  },
  {
    file: 'dashboard-worker.png',
    what: 'worker demos (reconcile, outbox retry, abandon): Worker row',
    url: DASH + range(process.env.WORKER_FROM, process.env.WORKER_TO),
    collapse: [1, 2, 3, 4],
  },
];

// problems lists what a reader would see as broken in the viewport: "No data",
// a panel showing "Error", Grafana's load failure page, and queries still in
// flight (the time picker offers "Cancel").
async function problems(page) {
  return page.evaluate(() => {
    const inView = (el) => {
      const r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < window.innerHeight;
    };
    const out = [];
    for (const el of document.querySelectorAll('body *')) {
      if (el.children.length) continue;
      const t = (el.textContent || '').trim();
      if ((t === 'No data' || t === 'Error' || /failed to load its application files|Bad Gateway/.test(t)) && inView(el)) out.push(t);
    }
    if ([...document.querySelectorAll('button')].some((b) => b.textContent.trim() === 'Cancel' && inView(b))) out.push('queries still running');
    return out;
  });
}

// busy: queries still running (the time picker offers "Cancel" until every
// panel's query has returned).
async function busy(page) {
  return page.evaluate(() => [...document.querySelectorAll('button')]
    .some((b) => b.textContent.trim() === 'Cancel' && b.getBoundingClientRect().width > 0));
}

async function settle(page) {
  for (let i = 0; i < 120 && (await busy(page)); i++) await page.waitForTimeout(1000);
  await page.waitForTimeout(2000);
}

async function refreshOff(page) {
  const picker = page.locator('button[aria-label*="refresh" i][aria-label*="interval" i], [data-testid*="RefreshPicker interval"]').first();
  if (!(await picker.count())) return 'no refresh picker found';
  await picker.click();
  await page.getByRole('menuitemradio', { name: 'Off' }).or(page.getByText('Off', { exact: true })).first().click();
  return 'auto-refresh off';
}

async function collapseRows(page, numbers) {
  for (const n of numbers) {
    await page.getByRole('button', { name: new RegExp(`^Collapse row ${n} · `) }).first().click();
    await page.waitForTimeout(500);
  }
}

(async () => {
  const browser = await chromium.launch();
  // An explicit locale: without one, headless Chromium in Docker makes Grafana
  // fail at startup ("Incorrect locale information provided").
  const ctx = await browser.newContext({
    viewport: { width: 1920, height: 1080 }, colorScheme: 'dark', locale: 'en-US', timezoneId: process.env.TZ_ID || 'Asia/Kolkata',
  });
  let bad = 0;
  // ONLY=<file> re-takes a single shot (e.g. after re-running its demo).
  for (const s of shots.filter((x) => !process.env.ONLY || x.file === process.env.ONLY)) {
    const page = await ctx.newPage();
    await page.goto(s.url, { waitUntil: 'domcontentloaded', timeout: 120000 });
    await page.getByRole('button', { name: /^Collapse row 1 · / }).first().waitFor({ timeout: 120000 });
    const r = await refreshOff(page);
    await collapseRows(page, s.collapse);
    await settle(page);
    const p = await problems(page);
    await page.screenshot({ path: `${OUT}/${s.file}` });
    console.log(`${s.file}: ${s.what}\n  ${r}; problems: ${p.length ? p.join('; ') : 'none'}`);
    bad += p.length;
    await page.close();
  }

  if (process.env.ONLY && process.env.ONLY !== 'prometheus-alerts.png') {
    await browser.close();
    process.exit(bad ? 1 : 0);
  }

  // Prometheus: every alert rule, in the same dark theme.
  const page = await ctx.newPage();
  // 'load', not 'networkidle': the Prometheus UI keeps polling.
  await page.goto(`${PROM}/alerts`, { waitUntil: 'load' });
  await page.evaluate(() => localStorage.setItem('mantine-color-scheme-value', 'dark'));
  await page.reload({ waitUntil: 'load' });
  await page.waitForTimeout(4000);
  const rules = await page.getByText(/^(DoubleBookingDetected|InvariantChecksNotRunning|OutboxDeadLetters|PaymentsStuckPending|WorkerJobStalled|HighErrorRate|RedisLockUnavailable|SlowHolds|DBPoolSaturated)$/).count();
  await page.screenshot({ path: `${OUT}/prometheus-alerts.png`, fullPage: true });
  console.log(`prometheus-alerts.png: the alert rules page\n  rules visible: ${rules} of 9`);
  if (rules < 9) bad++;

  await browser.close();
  process.exit(bad ? 1 : 0);
})();
