// render.cjs — renders the HTML sources in this folder to PNGs (headless
// Chromium via Playwright, no network needed):
//   architecture.html   -> /out/architecture-detailed.png (2400x1500)
//   social-preview.html -> /out/social-preview.png (1280x640)
const { chromium } = require('playwright-core');

const SRC = process.env.SRC || '/src';
const OUT = process.env.OUT || '/out';
const pages = [
  { html: 'architecture.html', png: 'architecture-detailed.png', width: 2400, height: 1500 },
  { html: 'social-preview.html', png: 'social-preview.png', width: 1280, height: 640 },
];

(async () => {
  const browser = await chromium.launch();
  for (const p of pages) {
    const page = await browser.newPage({ viewport: { width: p.width, height: p.height }, locale: 'en-US' });
    await page.goto(`file://${SRC}/${p.html}`, { waitUntil: 'load' });
    await page.locator('#canvas').screenshot({ path: `${OUT}/${p.png}` });
    console.log(`${p.png}: ${p.width}x${p.height}`);
    await page.close();
  }
  await browser.close();
})();
