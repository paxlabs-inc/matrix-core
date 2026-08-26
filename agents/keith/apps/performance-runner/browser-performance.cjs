const fs = require('node:fs');
const { chromium } = require(process.env.KEITH_PLAYWRIGHT_MODULE);

const origin = process.env.KEITH_WEB_ORIGIN;
const loginSecret = process.env.KEITH_BROWSER_LOGIN_SECRET;
const executablePath = process.env.KEITH_CHROMIUM_EXECUTABLE;
const rootSessionId = process.env.KEITH_BROWSER_ROOT_SESSION;
const resultPath = process.env.KEITH_BROWSER_RESULT;
const iterations = Number.parseInt(process.env.KEITH_BROWSER_ITERATIONS || '32', 10);

if (!origin || !loginSecret || !executablePath || !rootSessionId || !resultPath || !Number.isSafeInteger(iterations) || iterations < 1) {
  throw new Error('browser performance configuration is incomplete');
}

function micros(value) {
  return Math.max(0, Math.round(value * 1000));
}

async function countProjection(page, surface, label) {
  const projection = page.locator(`[data-panel="${surface}"] .projection`);
  await projection.getByText(new RegExp(`^${label}: \\d+$`)).waitFor({ timeout: 30000 });
  const text = await projection.textContent();
  const match = new RegExp(`${label}: (\\d+)`).exec(text || '');
  if (!match) throw new Error(`${surface} projection did not contain ${label}`);
  return Number.parseInt(match[1], 10);
}

let browser;
(async () => {
  browser = await chromium.launch({
    executablePath,
    headless: true,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const context = await browser.newContext({ reducedMotion: 'reduce' });
  const page = await context.newPage();
  const errors = [];
  const snapshotDetails = [];
  const websocketUrls = [];
  page.on('websocket', (socket) => {
    websocketUrls.push(socket.url());
    socket.on('framereceived', ({ payload }) => {
      if (typeof payload !== 'string') return;
      try {
        const message = JSON.parse(payload);
        const pending = [message];
        while (pending.length > 0) {
          const value = pending.pop();
          if (!value || typeof value !== 'object') continue;
          if (Array.isArray(value.children)) {
            snapshotDetails.push(`${value.session?.session_id || 'unknown'}:${value.children.length}`);
          }
          pending.push(...Object.values(value));
        }
      } catch (_) {}
    });
  });
  page.on('pageerror', (error) => errors.push(`page error: ${error.stack || error}`));
  page.on('requestfailed', (request) => {
    errors.push(`request failed: ${request.url()} ${request.failure()?.errorText || ''}`);
  });
  page.on('response', async (response) => {
    if (response.url().includes('/api/') && response.status() >= 400) {
      errors.push(`API response: ${response.status()} ${response.url()}`);
    }
  });

  await page.goto(origin, { waitUntil: 'domcontentloaded' });
  await page.locator('#password').fill(loginSecret);
  await Promise.all([
    page.waitForURL(`${origin}/`),
    page.getByRole('button', { name: 'Sign in' }).click(),
  ]);
  await page.goto(`${origin}/?session=${encodeURIComponent(rootSessionId)}`, { waitUntil: 'domcontentloaded' });
  await page.getByText('Connected', { exact: true }).waitFor({ timeout: 30000 });
  await page.locator('[data-panel="diagnostics"] .projection')
    .getByText(/^Generation \d+; sequence \d+; revision \d+$/)
    .waitFor({ state: 'attached', timeout: 30000 });
  const rootSession = page.locator(`.session[data-session="${rootSessionId}"]`);
  if (await rootSession.count() !== 1) throw new Error('browser could not identify the measured root session');
  const rootProfileId = await rootSession.getAttribute('data-profile');
  const rawRootChildren = await page.evaluate(({ profile, session }) => new Promise((resolve, reject) => {
    const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
    const socket = new WebSocket(`${scheme}://${location.host}/api/events/${profile}/${session}`);
    const timeout = setTimeout(() => {
      socket.close();
      reject(new Error('root WebSocket probe timed out'));
    }, 30000);
    socket.onmessage = ({ data }) => {
      try {
        const pending = [JSON.parse(data)];
        while (pending.length > 0) {
          const value = pending.pop();
          if (!value || typeof value !== 'object') continue;
          if (value.session?.session_id === session && Array.isArray(value.children)) {
            clearTimeout(timeout);
            socket.close();
            resolve(value.children.length);
            return;
          }
          pending.push(...Object.values(value));
        }
      } catch (_) {}
    };
    socket.onerror = () => {
      clearTimeout(timeout);
      reject(new Error('root WebSocket probe failed'));
    };
  }), { profile: rootProfileId, session: rootSessionId });
  if (rawRootChildren < 1) throw new Error(`targeted root WebSocket exposed ${rawRootChildren} children`);
  const childProjection = page.locator('[data-panel="children"] .projection');
  try {
    await childProjection
      .getByText(/^Children: [1-9]\d*$/)
      .waitFor({ state: 'attached', timeout: 30000 });
  } catch (error) {
    throw new Error(`root child projection did not update; expected_session=${rootSessionId}; text=${await childProjection.textContent()}; snapshots=${snapshotDetails.join(',')}; sockets=${websocketUrls.join(',')}; ${error}`);
  }

  const routeSwitchMicros = [];
  const routes = await page.locator('[data-route]').evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute('data-route')).filter(Boolean),
  );
  for (const route of routes) {
    const elapsed = await page.evaluate((selectedRoute) => new Promise((resolve, reject) => {
      const button = document.querySelector(`[data-route="${selectedRoute}"]`);
      const panel = document.querySelector(`[data-panel="${selectedRoute}"]`);
      if (!button || !panel) {
        reject(new Error(`route ${selectedRoute} is missing`));
        return;
      }
      const started = performance.now();
      button.click();
      requestAnimationFrame(() => {
        if (panel.hidden) {
          reject(new Error(`route ${selectedRoute} did not become visible`));
          return;
        }
        resolve(performance.now() - started);
      });
    }), route);
    routeSwitchMicros.push(micros(elapsed));
  }

  await page.locator('[data-route="children"]').click();
  const initialChildren = await countProjection(page, 'children', 'Children');
  if (initialChildren < 1) throw new Error('browser session did not expose recursive children');

  await page.locator('[data-route="goals"]').click();
  const goalEventToRenderMicros = [];
  let expectedGoals = await countProjection(page, 'goals', 'Goals');
  const diagnosticsBefore = await page.locator('[data-panel="diagnostics"] .projection').textContent();
  for (let index = 0; index < iterations; index += 1) {
    const started = performance.now();
    await page.locator('[data-panel="goals"] textarea[name="value"]').fill(`browser-event-goal-${index}`);
    await page.locator('[data-panel="goals"] button[type="button"]').click();
    expectedGoals += 1;
    await page.locator('[data-panel="goals"] .projection').getByText(`Goals: ${expectedGoals}`, { exact: true }).waitFor({ timeout: 30000 });
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => resolve())));
    goalEventToRenderMicros.push(micros(performance.now() - started));
  }
  const diagnosticsAfter = await page.locator('[data-panel="diagnostics"] .projection').textContent();
  if (diagnosticsAfter === diagnosticsBefore) {
    throw new Error('browser diagnostics sequence did not advance during the event stream');
  }
  if (errors.length > 0) throw new Error(errors.join('; '));

  const result = {
    route_switch_micros: routeSwitchMicros,
    goal_event_to_render_micros: goalEventToRenderMicros,
    initial_children: initialChildren,
    final_goals: expectedGoals,
    websocket_sequence_advanced: true,
    final_reconnected: false,
  };
  fs.writeFileSync(resultPath, JSON.stringify(result));
  await new Promise((resolve) => {
    process.stdin.once('data', resolve);
    process.stdin.once('end', resolve);
    process.stdin.resume();
  });
  await page.getByText('Connected', { exact: true }).waitFor({ timeout: 30000 });
  result.final_reconnected = true;
  fs.writeFileSync(resultPath, JSON.stringify(result));
  await browser.close();
  browser = undefined;
})().catch((error) => {
  console.error(error.stack || error);
  process.exitCode = 1;
}).finally(async () => {
  if (browser) await browser.close();
});
