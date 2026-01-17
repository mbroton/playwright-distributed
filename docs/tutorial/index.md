# Tutorial

Learn playwright-distributed by building with it. Each tutorial builds on the previous one, taking you from your first connection to a production-ready deployment.

---

## Learning Path

### 1. [Your First Connection](first-connection.md)
**Time: 5 minutes**

Start here. You'll:
- Start a local grid with Docker Compose
- Connect from Python or Node.js
- Take a screenshot of a website

### 2. [Multiple Browsers](multi-browser.md)
**Time: 10 minutes**

Add more browser types:
- Launch Firefox and WebKit workers
- Connect to specific browser types
- Understand browser selection

### 3. [Scaling Workers](scaling-workers.md)
**Time: 15 minutes**

Handle more load:
- Add multiple workers of the same type
- Understand how load is distributed
- Monitor worker activity

### 4. [Production Checklist](production-checklist.md)
**Time: 20 minutes**

Before going live:
- Configuration review
- Networking setup
- Common mistakes to avoid

---

## Prerequisites

Before starting, make sure you have:

- **Docker** and **Docker Compose** installed
- **Python 3.8+** or **Node.js 16+**
- **Playwright** library installed:
  ```bash
  # Python
  pip install playwright

  # Node.js
  npm install playwright
  ```

---

## What You'll Build

By the end of this tutorial, you'll have:

```
┌─────────────────────────────────────────────┐
│              Your Application               │
└─────────────────┬───────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────┐
│                  Proxy                       │
│            ws://localhost:8080              │
└─────────────────┬───────────────────────────┘
                  │
    ┌─────────────┼─────────────┐
    │             │             │
    ▼             ▼             ▼
┌────────┐  ┌────────┐  ┌────────┐
│Chrome  │  │Firefox │  │WebKit  │
│Worker  │  │Worker  │  │Worker  │
│  x2    │  │  x1    │  │  x1    │
└────────┘  └────────┘  └────────┘
```

A multi-browser grid that can handle concurrent connections across Chrome, Firefox, and WebKit.

---

## Ready?

Start with **[Your First Connection](first-connection.md)** →
