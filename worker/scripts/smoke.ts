import { chromium } from 'playwright-core';

const wsEndpoint = process.env.WS_ENDPOINT;

if (!wsEndpoint) {
    throw new Error('WS_ENDPOINT is required');
}

const browser = await chromium.connect(wsEndpoint, { timeout: 5000 });

try {
    const context = await browser.newContext();
    const page = await context.newPage();

    await page.setContent('<title>Playwright Distributed smoke test</title>');

    const title = await page.title();
    if (title !== 'Playwright Distributed smoke test') {
        throw new Error(`Unexpected page title: ${title}`);
    }

    await context.close();
    console.log('Smoke test passed.');
} finally {
    await browser.close();
}
