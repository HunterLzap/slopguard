import { test, expect } from '@playwright/test';
import * as fs from 'node:fs';
import * as path from 'node:path';

const baseURL = process.env.SLOPGUARD_BASE_URL || 'http://127.0.0.1:3000';
const routes: string[] = JSON.parse(process.env.SLOPGUARD_ROUTES || '["/"]');
const screenshotDir = process.env.SLOPGUARD_SCREENSHOT_DIR || 'ui-screenshots';

fs.mkdirSync(screenshotDir, { recursive: true });

function routeFileName(route: string): string {
  const cleaned = route.replace(/[^a-zA-Z0-9_-]+/g, '_').replace(/^_+|_+$/g, '');
  return cleaned || 'root';
}

for (const route of routes) {
  test(`UI smoke ${route}`, async ({ page }) => {
    const problems: string[] = [];

    page.on('console', (message) => {
      if (message.type() === 'error') {
        problems.push(`console.error: ${message.text()}`);
      }
    });
    page.on('pageerror', (error) => {
      problems.push(`pageerror: ${error.message}`);
    });
    page.on('response', (response) => {
      if (response.status() >= 500) {
        problems.push(`HTTP ${response.status()}: ${response.url()}`);
      }
    });

    const url = new URL(route, baseURL).toString();
    await page.goto(url, { waitUntil: 'domcontentloaded' });
    await page.waitForLoadState('networkidle', { timeout: 5000 }).catch(() => undefined);
    await page.screenshot({
      path: path.join(screenshotDir, `${routeFileName(route)}.png`),
      fullPage: true,
    });

    for (const problem of problems) {
      console.log(`SLOPGUARD_UI_FINDING\t${route}\t${problem}`);
    }
    expect(problems, `UI problems on ${route}:\n${problems.join('\n')}`).toEqual([]);
  });
}
