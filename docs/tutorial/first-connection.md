# Your First Connection

**Time: 5 minutes**

By the end of this tutorial, you'll have a working Playwright grid and will have controlled a browser through it.

---

## What You'll Do

1. Start the grid with Docker Compose
2. Connect from your code
3. Navigate to a website and take a screenshot

---

## Step 1: Get the Code

Clone the repository:

```bash
git clone https://github.com/mbroton/playwright-distributed.git
cd playwright-distributed
```

---

## Step 2: Start the Grid

Launch the grid with a single command:

```bash
docker compose up -d
```

This starts three containers:

| Container | Purpose |
|-----------|---------|
| **redis** | Stores worker registry and coordinates the system |
| **proxy** | Accepts your connections and routes them to workers |
| **worker** | Runs the actual browser (Chromium by default) |

Verify everything is running:

```bash
docker compose ps
```

You should see all three containers with status "Up":

```
NAME                           STATUS
playwright-distributed-proxy   Up
playwright-distributed-worker  Up
playwright-distributed-redis   Up
```

---

## Step 3: Connect from Your Code

Now the fun part. Create a script that connects to your grid.

### Python

Create a file called `test_grid.py`:

```python
import asyncio
from playwright.async_api import async_playwright

async def main():
    async with async_playwright() as p:
        # Connect to the grid instead of launching a local browser
        browser = await p.chromium.connect("ws://localhost:8080")

        # From here, it's just normal Playwright
        page = await browser.new_page()
        await page.goto("https://example.com")

        # Take a screenshot
        await page.screenshot(path="screenshot.png")
        print(f"Page title: {await page.title()}")

        await browser.close()

asyncio.run(main())
```

Run it:

```bash
python test_grid.py
```

### Node.js

Create a file called `test_grid.js`:

```javascript
import { chromium } from "playwright";

async function main() {
  // Connect to the grid instead of launching a local browser
  const browser = await chromium.connect("ws://localhost:8080");

  // From here, it's just normal Playwright
  const page = await browser.newPage();
  await page.goto("https://example.com");

  // Take a screenshot
  await page.screenshot({ path: "screenshot.png" });
  console.log(`Page title: ${await page.title()}`);

  await browser.close();
}

main();
```

Run it:

```bash
node test_grid.js
```

---

## Step 4: Check the Result

You should see:
- A `screenshot.png` file in your current directory
- Output showing "Page title: Example Domain"

Open the screenshot—you just controlled a browser running in a Docker container!

---

## What Just Happened?

Let's trace the connection:

```
Your Script                    Grid
    │
    │  ws://localhost:8080
    ├─────────────────────────► Proxy
    │                             │
    │                             │ "Find me a worker"
    │                             ├─────────────────► Redis
    │                             │ ◄────────────────┤
    │                             │ "Use worker at ws://worker:3131"
    │                             │
    │                             │ WebSocket
    │  ◄──────────────────────────┼─────────────────► Worker
    │                             │                     │
    │     Messages flow           │                     │ Browser
    │     bidirectionally         │                     │ Instance
    │                             │                     │
```

1. **Your script** connected to `ws://localhost:8080` (the proxy)
2. **The proxy** asked Redis for an available worker
3. **Redis** returned the worker's internal address
4. **The proxy** connected to the worker and started relaying messages
5. **The worker** executed your Playwright commands in a real browser

The beautiful part: your code doesn't know or care about any of this. It just uses standard Playwright APIs.

---

## Understanding Browser Contexts

When you called `browser.new_page()`, you got an isolated browser context. This means:

- **Fresh state**: No cookies, localStorage, or cache from previous sessions
- **Isolated**: Other connections to the same worker can't see your data
- **Efficient**: The browser process is shared, but contexts are separate

This is why the grid is fast—browsers are already running. You're just getting a fresh context in an existing browser.

---

## Cleaning Up

When you're done experimenting:

```bash
docker compose down
```

This stops and removes all containers.

---

## Troubleshooting

### "Connection refused" error

The proxy isn't running. Check with `docker compose ps` and make sure all containers are "Up".

### "No available servers" error

The worker hasn't registered yet. Wait a few seconds and try again—workers need a moment to start up and register with Redis.

### Script hangs forever

Check that port 8080 isn't blocked by a firewall or used by another application.

---

## Next Steps

You've got the basics working. Next, let's add more browser types:

**[Multiple Browsers](multi-browser.md)** — Add Firefox and WebKit workers →

---

## Quick Reference

| Command | Purpose |
|---------|---------|
| `docker compose up -d` | Start the grid |
| `docker compose ps` | Check container status |
| `docker compose logs -f proxy` | Watch proxy logs |
| `docker compose logs -f worker` | Watch worker logs |
| `docker compose down` | Stop the grid |
