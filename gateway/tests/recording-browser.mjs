import assert from 'node:assert/strict';
import { chromium } from 'playwright';

const base = process.argv[2];
const browser = await chromium.launch({ args: ['--autoplay-policy=no-user-gesture-required'] });
try {
  for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
    const context = await browser.newContext({ viewport });
    const page = await context.newPage();
    const errors = [];
    const requests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', msg => { if (msg.type() === 'error') console.log('browser:', msg.text()); });
    page.on('request', request => {
      if (request.url().includes('/api/recordings/segment')) requests.push(new URL(request.url()));
    });
    // A single transient segment failure must recover without restarting VOD.
    let interrupted = false;
    await page.route('**/api/recordings/segment?**', async route => {
      const query = new URL(route.request().url()).searchParams;
      if (!interrupted && query.get('kind') === 'media') {
        interrupted = true;
        await route.abort('failed');
      } else {
        await route.continue();
      }
    });
    await page.goto(`${base}/fixture`);
    await page.evaluate(() => {
      window.failures = [];
      window.player = new window.RecordingPlayback(document.querySelector('video'), {
        onError: reason => window.failures.push(reason),
      });
      window.player.open('/api/recordings/play?start=2026-09-05T00:00:00Z&duration=48');
    });
    const checkColor = async (channel, time) => {
      await page.waitForFunction(({ channel, time }) => {
        const video = document.querySelector('video');
        if (video.readyState < 2 || video.seeking || video.currentTime < time || video.currentTime > time + 4) return false;
        const canvas = document.createElement('canvas');
        canvas.width = 320; canvas.height = 180;
        const ctx = canvas.getContext('2d');
        ctx.drawImage(video, 0, 0, 320, 180);
        const rgb = ctx.getImageData(40, 40, 1, 1).data;
        return rgb[channel] > 80 && rgb[channel] > rgb[(channel+1)%3]*2 && rgb[channel] > rgb[(channel+2)%3]*2;
      }, { channel, time }, { timeout: 30000 });
    };
    await checkColor(0, 0.1);
    assert.equal(await page.evaluate(() => Math.round(document.querySelector('video').duration)), 48);
    assert(requests.every(u => Number(u.searchParams.get('duration')) <= 6));
    assert(!requests.some(u => new Date(u.searchParams.get('start')).getUTCSeconds() >= 30), 'downloaded the far end before seeking');
    // Jump beyond the buffered window, then backwards; verify decoded colors.
    await page.evaluate(() => { document.querySelector('video').currentTime = 40; });
    await checkColor(2, 40);
    await page.evaluate(() => { document.querySelector('video').currentTime = 10; });
    await checkColor(0, 10);
    // Play across the 12-second discontinuity and verify the next footage.
    await checkColor(1, 12.2);
    await page.evaluate(() => {
      window.player.open('/api/recordings/play?start=2026-09-05T00:00:00Z&duration=48');
      window.player.open('/api/recordings/play?start=2026-09-05T00:00:24Z&duration=24');
    });
    await checkColor(2, 0.1);
    assert.deepEqual(await page.evaluate(() => window.failures), []);
    assert.deepEqual(errors, []);
    await page.evaluate(() => window.player.close());
    assert.equal(await page.evaluate(() => document.querySelector('video').getAttribute('src')), null);
    console.log(`PASS ${viewport.width}px: first frame, bounded buffering, transient retry, forward/backward seek, segment boundary and rapid clip switch`);
    await context.close();
  }
} finally {
  await browser.close();
}
