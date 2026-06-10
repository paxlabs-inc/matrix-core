# syntax=docker/dockerfile:1.7
#
# matrix-browser — the SHARED headless browser for the Matrix agent fleet.
#
# Runs Microsoft's @playwright/mcp over its Streamable-HTTP transport. Every
# per-user daemon reaches this single app over Fly's private 6PN network
# (http://matrix-browser.internal:8931/mcp) through the daemon-side stdio proxy
# tools/browser/browser.mjs. Each daemon opens its own MCP session; --isolated
# gives every session a throwaway browser context (no cross-tenant cookies).
#
# PINNING: PWMCP_VERSION must match the version whose tools/list is mirrored in
# tools/browser/playwright-tools.json (and enumerated in agents/default.json).
# Bump all three together, then re-run `node tools/browser/browser.mjs --selftest`.
#
# SECURITY NOTE: the agent tool surface includes browser_evaluate and
# browser_run_code_unsafe (arbitrary JS / RCE-equivalent in this process).
# This app must stay PRIVATE (no public Fly IP — see fly.toml) and is a shared
# trust boundary. Harden later (drop the unsafe caps, per-user browser pods, or
# a bearer-token reverse proxy) before exposing beyond the org network.

FROM node:22-bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    NODE_ENV=production \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# Pin to the version mirrored in tools/browser/playwright-tools.json.
ARG PWMCP_VERSION=0.0.75

# tini for clean signal handling; the rest are Chromium's runtime libs which
# `playwright install --with-deps` pulls in.
#
# IMPORTANT: install the browser with @playwright/mcp's OWN bundled playwright
# CLI — NOT `npx playwright install`, which resolves the latest STANDALONE
# playwright package. That one can (and did, 2026-06-10) want a different
# chromium revision than the mcp's pinned playwright, leaving the server with
# "Chrome isn't installed" because /ms-playwright holds the wrong revision.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates tini; \
    npm install -g @playwright/mcp@${PWMCP_VERSION}; \
    node /usr/local/lib/node_modules/@playwright/mcp/node_modules/playwright/cli.js install --with-deps chromium; \
    npm cache clean --force; \
    rm -rf /var/lib/apt/lists/*

EXPOSE 8931

# Health probe: the Streamable-HTTP endpoint rejects a bare GET (needs the MCP
# handshake), but a reachable listener returns an HTTP status — good enough to
# prove the process is up. Fly's own checks live in fly.toml.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD node -e "fetch('http://127.0.0.1:8931/mcp',{method:'POST',headers:{'content-type':'application/json','accept':'application/json, text/event-stream'},body:'{}'}).then(r=>process.exit(r.status>0?0:1)).catch(()=>process.exit(1))"

# --headless: no display. --isolated: fresh context per session (tenant isolation).
# --host :: : dual-stack bind (IPv6 + v4-mapped). Fly's 6PN .internal DNS is
# IPv6-ONLY, so an IPv4 0.0.0.0 bind is unreachable from the daemon fleet
# (ECONNREFUSED on every browser_* call); Node's net.Server with host "::"
# accepts both families, keeping the 127.0.0.1 healthcheck working. --no-sandbox:
# required for Chromium running as root in a container. --port 8931: Streamable
# HTTP served at /mcp. --allowed-hosts '*': disable Playwright-MCP's Host-header
# (DNS-rebinding) guard, which otherwise 403s anything whose Host isn't the bind
# address — including daemons hitting matrix-browser.internal:8931 and the local
# healthcheck. Safe here: the app has NO public IP and is reachable only inside
# the org's private 6PN network (see fly.toml + README).
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "npx", "@playwright/mcp"]
CMD ["--headless", "--isolated", "--browser", "chromium", "--no-sandbox", "--host", "::", "--port", "8931", "--allowed-hosts", "*"]
