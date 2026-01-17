# Multiple Browsers

**Time: 10 minutes**

Your grid currently runs Chromium. Let's add Firefox and WebKit workers so you can test across all major browser engines.

---

## What You'll Do

1. Start a grid with all three browser types
2. Connect to specific browsers
3. Run the same test across all browsers

---

## Step 1: Start the Multi-Browser Grid

The repository includes a development compose file with all browser types:

```bash
docker compose -f docker-compose.local.yaml up -d
```

This starts:

| Container | Browser | Port |
|-----------|---------|------|
| worker-chromium | Chromium | Internal |
| worker-firefox | Firefox | Internal |
| worker-webkit | WebKit | Internal |
| proxy | Routes to all | 8080 |
| redis | Registry | Internal |

Check they're all running:

```bash
docker compose -f docker-compose.local.yaml ps
```

---

## Step 2: Connect to Specific Browsers

The proxy uses a query parameter to route to the right browser type.

### Default (Chromium)

```python
# No query parameter = Chromium
browser = await p.chromium.connect("ws://localhost:8080")
```

### Firefox

```python
# ?browser=firefox routes to Firefox workers
browser = await p.firefox.connect("ws://localhost:8080?browser=firefox")
```

### WebKit

```python
# ?browser=webkit routes to WebKit workers
browser = await p.webkit.connect("ws://localhost:8080?browser=webkit")
```

> **Important**: Match the Playwright client to the browser type. Use `p.firefox.connect()` for Firefox, not `p.chromium.connect()`.

---

## Step 3: Cross-Browser Testing

Here's a script that runs the same test across all browsers:

### Python

```python
import asyncio
from playwright.async_api import async_playwright

BROWSERS = [
    ("chromium", "ws://localhost:8080"),
    ("firefox", "ws://localhost:8080?browser=firefox"),
    ("webkit", "ws://localhost:8080?browser=webkit"),
]

async def test_browser(playwright, name, url):
    """Run a simple test on one browser."""
    print(f"Testing {name}...")

    # Get the right client for this browser type
    client = getattr(playwright, name)
    browser = await client.connect(url)

    try:
        page = await browser.new_page()
        await page.goto("https://example.com")
        title = await page.title()
        print(f"  {name}: {title}")

        # Take a browser-specific screenshot
        await page.screenshot(path=f"screenshot-{name}.png")

    finally:
        await browser.close()

async def main():
    async with async_playwright() as p:
        # Test all browsers
        for name, url in BROWSERS:
            await test_browser(p, name, url)

    print("\nAll browsers tested!")

asyncio.run(main())
```

### Node.js

```javascript
import { chromium, firefox, webkit } from "playwright";

const BROWSERS = [
  { name: "chromium", client: chromium, url: "ws://localhost:8080" },
  { name: "firefox", client: firefox, url: "ws://localhost:8080?browser=firefox" },
  { name: "webkit", client: webkit, url: "ws://localhost:8080?browser=webkit" },
];

async function testBrowser({ name, client, url }) {
  console.log(`Testing ${name}...`);

  const browser = await client.connect(url);

  try {
    const page = await browser.newPage();
    await page.goto("https://example.com");
    const title = await page.title();
    console.log(`  ${name}: ${title}`);

    await page.screenshot({ path: `screenshot-${name}.png` });
  } finally {
    await browser.close();
  }
}

async function main() {
  for (const browser of BROWSERS) {
    await testBrowser(browser);
  }
  console.log("\nAll browsers tested!");
}

main();
```

Run it and you'll get three screenshots, one from each browser engine.

---

## How Browser Selection Works

When you connect with `?browser=firefox`:

1. **Proxy** extracts the `browser` query parameter
2. **Proxy** asks Redis for workers with `browserType=firefox`
3. **Redis** returns only Firefox workers
4. **Proxy** selects one and connects you

If no workers match, you get a 503 error. Make sure you have workers for the browser type you're requesting.

---

## Running Tests in Parallel

Want to test all browsers simultaneously? Here's how:

### Python

```python
import asyncio
from playwright.async_api import async_playwright

async def test_browser(playwright, name, url):
    client = getattr(playwright, name)
    browser = await client.connect(url)

    try:
        page = await browser.new_page()
        await page.goto("https://example.com")
        title = await page.title()
        return f"{name}: {title}"
    finally:
        await browser.close()

async def main():
    async with async_playwright() as p:
        # Run all browsers in parallel
        results = await asyncio.gather(
            test_browser(p, "chromium", "ws://localhost:8080"),
            test_browser(p, "firefox", "ws://localhost:8080?browser=firefox"),
            test_browser(p, "webkit", "ws://localhost:8080?browser=webkit"),
        )

        for result in results:
            print(result)

asyncio.run(main())
```

---

## Common Mistakes

### Wrong client for browser type

```python
# WRONG: Using chromium client for Firefox
browser = await p.chromium.connect("ws://localhost:8080?browser=firefox")

# RIGHT: Match client to browser type
browser = await p.firefox.connect("ws://localhost:8080?browser=firefox")
```

### Missing browser parameter for non-Chromium

```python
# WRONG: This connects to Chromium, not Firefox
browser = await p.firefox.connect("ws://localhost:8080")

# RIGHT: Specify the browser type
browser = await p.firefox.connect("ws://localhost:8080?browser=firefox")
```

### Typo in browser parameter

```python
# WRONG: "Firefox" with capital F
browser = await p.firefox.connect("ws://localhost:8080?browser=Firefox")

# RIGHT: All lowercase
browser = await p.firefox.connect("ws://localhost:8080?browser=firefox")
```

---

## Cleaning Up

```bash
docker compose -f docker-compose.local.yaml down
```

---

## Next Steps

You now have a multi-browser grid. Next, let's scale it to handle more load:

**[Scaling Workers](scaling-workers.md)** — Add more workers for higher throughput →

---

## Quick Reference

| Browser | Query Parameter | Playwright Client |
|---------|-----------------|-------------------|
| Chromium | (none) or `?browser=chromium` | `chromium.connect()` |
| Firefox | `?browser=firefox` | `firefox.connect()` |
| WebKit | `?browser=webkit` | `webkit.connect()` |
