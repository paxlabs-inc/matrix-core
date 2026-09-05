const path = require('node:path')
const { chromium } = require(process.env.KEITH_PLAYWRIGHT_MODULE)
const axeSource = require(
  require.resolve('axe-core', { paths: [path.join(__dirname, '../ui')] }),
).source

const origin = process.env.KEITH_WEB_ORIGIN
const password = process.env.KEITH_WEB_LOGIN_SECRET
const expected = process.env.KEITH_BROWSER_EXPECT || 'KEITH_BROWSER_LIVE_OK'
const prompt =
  process.env.KEITH_BROWSER_PROMPT ||
  `Reply with exactly ${expected}. Do not call tools and do not add punctuation.`
const toolExpected = process.env.KEITH_BROWSER_TOOL_EXPECT || 'KEITH_NEXT_TOOL_OK'
const toolPrompt =
  process.env.KEITH_BROWSER_TOOL_PROMPT ||
  `Use your file tools to create browser-next-proof.txt with exactly ${toolExpected}, read it back, then end your reply with exactly ${toolExpected}.`

let browser
;(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.KEITH_CHROMIUM_EXECUTABLE || undefined,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  })
  const context = await browser.newContext({ reducedMotion: 'reduce' })
  const page = await context.newPage()
  await page.addInitScript({ content: axeSource })
  const failures = []
  const requested = []
  page.on('request', (request) => requested.push(request.url()))
  page.on('requestfailed', (request) => failures.push(`${request.url()} ${request.failure()?.errorText}`))
  page.on('pageerror', (error) => failures.push(error.stack || String(error)))

  await page.goto(origin, { waitUntil: 'domcontentloaded' })
  const login = page.locator('#password')
  const ready = page.locator('.state-pill.connected')
  await Promise.race([
    login.waitFor({ state: 'visible', timeout: 20_000 }),
    ready.waitFor({ state: 'visible', timeout: 20_000 }),
  ])
  if (await login.isVisible()) {
    await login.fill(password)
    await Promise.all([
      page.waitForURL(`${origin}/`),
      page.getByRole('button', { name: 'Continue' }).click(),
    ])
  }

  await ready.waitFor({ timeout: 20_000 })
  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && response.url().includes('/commands'),
      { timeout: 20_000 },
    ),
    page.locator('.new-chat').click(),
  ])
  if (!createResponse.ok()) {
    throw new Error(`new conversation command failed with ${createResponse.status()}`)
  }
  await page.waitForFunction(
    () => {
      const button = document.querySelector('.new-chat')
      return (
        button instanceof HTMLButtonElement &&
        !button.disabled &&
        button.textContent.includes('New conversation')
      )
    },
    undefined,
    { timeout: 20_000 },
  )
  const composer = page.getByRole('textbox', { name: 'Ask Keith' })
  await composer.fill(prompt)
  await page.evaluate(() => {
    window.__keithSawStreamingAssistant = false
    window.__keithSawLiveTool = false
    window.__keithStreamObserver = new MutationObserver(() => {
      if (document.querySelector('.assistant-row:not([data-committed]) .assistant-content')) {
        window.__keithSawStreamingAssistant = true
      }
      if (document.querySelector('.live-tool')) window.__keithSawLiveTool = true
    })
    window.__keithStreamObserver.observe(document.body, { childList: true, subtree: true, attributes: true })
  })
  const sendStarted = Date.now()
  const [firstCommand] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && response.url().includes('/commands'),
    ),
    page.getByRole('button', { name: 'Send' }).click(),
  ])
  if (!firstCommand.ok()) throw new Error(`first prompt returned ${firstCommand.status()}`)
  await page.getByTestId('pending-user-message').filter({ hasText: prompt }).waitFor({ timeout: 1_500 })
  const echoLatency = Date.now() - sendStarted
  if (echoLatency > 1_500) throw new Error(`user echo latency was ${echoLatency}ms`)
  await page.getByRole('region', { name: 'Keith live activity' }).waitFor({ timeout: 5_000 })
  await page.getByText(expected, { exact: true }).waitFor({ timeout: 180_000 })
  await page.getByRole('region', { name: 'Keith live activity' }).waitFor({ state: 'hidden', timeout: 20_000 })
  await page.waitForTimeout(250)
  const sawStreamingAssistant = await page.evaluate(() => Boolean(window.__keithSawStreamingAssistant))
  if (!sawStreamingAssistant) throw new Error('assistant output never appeared as an uncommitted live stream')

  await page.reload({ waitUntil: 'domcontentloaded' })
  await ready.waitFor({ timeout: 20_000 })
  await page.getByText(expected, { exact: true }).waitFor({ timeout: 20_000 })
  await page.evaluate(() => {
    window.__keithSawLiveTool = false
    window.__keithSawCommentaryBeforeTool = false
    window.__keithSawStreamAfterUser = false
    window.__keithToolObserver = new MutationObserver(() => {
      const streaming = document.querySelector('.assistant-row:not([data-committed])')
      const users = document.querySelectorAll('.user-row')
      const user = users.item(users.length - 1)
      if (
        streaming &&
        user &&
        Boolean(user.compareDocumentPosition(streaming) & Node.DOCUMENT_POSITION_FOLLOWING)
      ) {
        window.__keithSawStreamAfterUser = true
      }
      if (
        document.querySelector('.assistant-row:not([data-committed]) .assistant-content') &&
        !document.querySelector('.live-tool')
      ) {
        window.__keithSawCommentaryBeforeTool = true
      }
      if (document.querySelector('.live-tool')) window.__keithSawLiveTool = true
    })
    window.__keithToolObserver.observe(document.body, { childList: true, subtree: true, attributes: true })
  })

  await composer.fill(toolPrompt)
  const [toolCommand] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && response.url().includes('/commands'),
    ),
    page.getByRole('button', { name: 'Send' }).click(),
  ])
  if (!toolCommand.ok()) throw new Error(`tool prompt returned ${toolCommand.status()}`)
  await page.locator('.assistant-row').filter({ hasText: toolExpected }).last().waitFor({ timeout: 180_000 })
  await page.getByRole('region', { name: 'Keith live activity' }).waitFor({ state: 'hidden', timeout: 20_000 })
  const sawLiveTool = await page.evaluate(() => Boolean(window.__keithSawLiveTool))
  if (!sawLiveTool) throw new Error('tool activity never appeared in the live run stream')
  const sawCommentaryBeforeTool = await page.evaluate(
    () => Boolean(window.__keithSawCommentaryBeforeTool),
  )
  if (!sawCommentaryBeforeTool) {
    throw new Error('user-visible progress commentary did not stream before the first tool action')
  }
  const sawStreamAfterUser = await page.evaluate(() => Boolean(window.__keithSawStreamAfterUser))
  if (!sawStreamAfterUser) {
    throw new Error('live assistant output did not render after the user message that triggered it')
  }

  await sendAndWaitForCommitted(
    page,
    composer,
    'My name is Rowan. Remember that Rowan is the name I chose for you to call me.',
  )
  const continuity = await sendAndWaitForCommitted(
    page,
    composer,
    'What name did I give you? Also compute 17 times 19. Answer in one short sentence.',
  )
  if (!/\bRowan\b/.test(continuity) || !/\b323\b/.test(continuity)) {
    throw new Error(`two-turn continuity failed: ${continuity}`)
  }

  const commentary = page.locator('.assistant-row[data-kind="commentary"]')
  const commentaryBeforeReload = await commentary.count()
  if (!commentaryBeforeReload) {
    throw new Error('completed turn did not retain any committed progress commentary')
  }
  await page.waitForTimeout(250)
  await page.reload({ waitUntil: 'domcontentloaded' })
  await ready.waitFor({ timeout: 20_000 })
  await page.waitForFunction(
    (count) => document.querySelectorAll('.assistant-row[data-kind="commentary"]').length >= count,
    commentaryBeforeReload,
    { timeout: 20_000 },
  )

  const workspace = page.getByRole('button', { name: "Show Keith's Computer" })
  await workspace.click()
  await page.getByRole('complementary', { name: "Keith's Computer" }).waitFor()
  await page.waitForFunction(() => {
    const responses = document.querySelectorAll('.assistant-row[data-kind="response"]')
    const response = responses.item(responses.length - 1)
    const dock = document.querySelector('.composer-dock')
    return response && dock && response.getBoundingClientRect().bottom <= dock.getBoundingClientRect().top
  }, undefined, { timeout: 10_000 })
  await assertAccessible(page, 'desktop conversation and computer')
  if (process.env.KEITH_BROWSER_DESKTOP_SCREENSHOT) {
    await page.screenshot({ path: process.env.KEITH_BROWSER_DESKTOP_SCREENSHOT, fullPage: true })
  }

  if (process.env.KEITH_BROWSER_MOBILE_SCREENSHOT) {
    await page.getByRole('button', { name: "Close Keith's Computer" }).click()
    await page.setViewportSize({ width: 390, height: 844 })
    await assertAccessible(page, 'mobile conversation')
    await page.screenshot({ path: process.env.KEITH_BROWSER_MOBILE_SCREENSHOT, fullPage: true })
  }

  const bootstrap = await page.request.get(`${origin}/api/bootstrap`)
  if (bootstrap.status() !== 200) throw new Error(`authenticated bootstrap returned ${bootstrap.status()}`)
  const wrongCsrf = await page.evaluate(async () => {
    const response = await fetch('/api/profiles/not-a-profile/commands', {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-keith-csrf': 'wrong' },
      body: '{}',
    })
    return response.status
  })
  if (wrongCsrf !== 403) throw new Error(`wrong CSRF returned ${wrongCsrf}`)
  if (requested.some((url) => /agent_web|\.wasm(?:$|\?)/.test(url))) {
    throw new Error('legacy Rust/WASM browser assets were requested')
  }
  if (failures.length) throw new Error(`browser failures:\n${failures.join('\n')}`)

  await browser.close()
  browser = undefined
})().catch((error) => {
  console.error(error)
  process.exitCode = 1
}).finally(async () => {
  if (browser) await browser.close()
})

async function assertAccessible(page, label) {
  const results = await page.evaluate(async () => window.axe.run(document, {
    runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'] },
  }))
  const serious = results.violations.filter((violation) =>
    violation.impact === 'serious' || violation.impact === 'critical')
  if (serious.length) {
    const detail = serious.map((violation) =>
      `${violation.id}: ${violation.nodes.map((node) => node.target.join(' ')).join(', ')}`,
    ).join('\n')
    throw new Error(`${label} accessibility violations:\n${detail}`)
  }
}

async function sendAndWaitForCommitted(page, composer, text) {
  const committed = page.locator('.assistant-row[data-committed]')
  const before = await committed.count()
  await composer.fill(text)
  const [command] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && response.url().includes('/commands'),
    ),
    page.getByRole('button', { name: 'Send' }).click(),
  ])
  if (!command.ok()) throw new Error(`prompt returned ${command.status()}`)
  await page.waitForFunction(
    (count) => document.querySelectorAll('.assistant-row[data-committed]').length > count,
    before,
    { timeout: 180_000 },
  )
  await page.getByRole('region', { name: 'Keith live activity' }).waitFor({ state: 'hidden', timeout: 20_000 })
  return (await committed.last().innerText()).trim()
}
