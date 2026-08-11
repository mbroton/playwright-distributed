import { chromium, firefox, webkit } from 'playwright-core';

const wsEndpoint = process.env.WS_ENDPOINT;
const browserType = process.env.BROWSER_TYPE ?? 'chromium';

if (!wsEndpoint) {
    throw new Error('WS_ENDPOINT is required');
}

const browserTypes = { chromium, firefox, webkit } as const;
if (!(browserType in browserTypes)) {
    throw new Error(`Unsupported BROWSER_TYPE: ${browserType}`);
}

const browser = await browserTypes[browserType as keyof typeof browserTypes]
    .connect(wsEndpoint, { timeout: 5000 });

try {
    const context = await browser.newContext();
    const page = await context.newPage();

    await page.setContent('<title>Playwright Distributed smoke test</title>');

    const title = await page.title();
    if (title !== 'Playwright Distributed smoke test') {
        throw new Error(`Unexpected page title: ${title}`);
    }

    await context.close();
    console.log(`${browserType} smoke test passed.`);
} finally {
    await browser.close();
}
