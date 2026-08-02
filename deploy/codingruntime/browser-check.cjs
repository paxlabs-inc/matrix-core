#!/usr/bin/env node
'use strict'

const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')

function fail(message) {
  process.stderr.write(`${message}\n`)
  process.exit(1)
}

function option(name, fallback = '') {
  const index = process.argv.indexOf(`--${name}`)
  return index === -1 ? fallback : process.argv[index + 1] || ''
}

function redact(value) {
  return String(value)
    .replace(/\bBearer\s+[A-Za-z0-9._~+/=-]{8,}/gi, 'Bearer [redacted]')
    .replace(/\b(api[_-]?key|access[_-]?token|auth[_-]?token|password|secret)\s*[:=]\s*\S+/gi, '$1=[redacted]')
}

function reportUrl(value) {
  try {
    const parsed = new URL(value)
    return `${parsed.protocol}//${parsed.host}${parsed.pathname}`
  } catch {
    return value.split(/[?#]/, 1)[0]
  }
}

async function main() {
  const mode = option('mode')
  const url = option('url')
  const clientRoot = path.resolve(option('client-root'))
  const artifactDir = path.resolve(option('artifact-dir'))
  if (!mode || !url || !clientRoot || !artifactDir) fail('mode, url, client-root and artifact-dir are required')
  fs.mkdirSync(artifactDir, { recursive: true })

  const requireFromClient = createRequire(path.join(clientRoot, 'package.json'))
  const { chromium } = requireFromClient('@playwright/test')
  const storageState = option('storage-state')
  const browser = await chromium.launch({ headless: true })
  const context = await browser.newContext(storageState ? { storageState } : {})
  const page = await context.newPage()
  const consoleErrors = []
  page.on('console', (entry) => {
    if (entry.type() === 'error') consoleErrors.push(redact(entry.text()))
  })
  page.on('pageerror', (error) => consoleErrors.push(redact(error.message)))

  const result = { mode, url: reportUrl(url), artifact_dir: artifactDir, console_errors: consoleErrors }
  try {
    if (mode === 'build-reload') {
      const jobID = option('job-id')
      if (!jobID) fail('job-id is required for build-reload')
      const selector = `[data-build-job-id="${jobID.replace(/["\\]/g, '\\$&')}"]`
      await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 45_000 })
      await page.locator(selector).waitFor({ state: 'visible', timeout: 45_000 })
      const before = await page.locator(selector).getAttribute('data-build-status')
      const beforeText = await page.locator(selector).innerText()
      const beforeEvidence = await page.locator(selector).locator('[aria-label="Recorded Build evidence"] > *').count()
      await page.reload({ waitUntil: 'domcontentloaded', timeout: 45_000 })
      await page.locator(selector).waitFor({ state: 'visible', timeout: 45_000 })
      const after = await page.locator(selector).getAttribute('data-build-status')
      const afterText = await page.locator(selector).innerText()
      const afterEvidence = await page.locator(selector).locator('[aria-label="Recorded Build evidence"] > *').count()
      const expectedStatus = option('expected-status')
      if (expectedStatus && after !== expectedStatus) throw new Error(`expected Build status ${expectedStatus}, got ${after}`)
      if (expectedStatus && beforeText !== afterText) throw new Error('terminal Build card changed across browser reload')
      result.visible_before_reload = true
      result.visible_after_reload = true
      result.status_before_reload = before
      result.status_after_reload = after
      result.evidence_before_reload = beforeEvidence
      result.evidence_after_reload = afterEvidence
      result.terminal_projection_stable = expectedStatus ? beforeText === afterText && beforeEvidence === afterEvidence : null
    } else if (mode === 'responsive-site') {
      for (const width of [390, 1280]) {
        await page.setViewportSize({ width, height: 800 })
        await page.goto(url, { waitUntil: 'networkidle', timeout: 45_000 })
        await page.getByTestId('hero').waitFor({ state: 'visible', timeout: 20_000 })
        await page.getByTestId('primary-cta').waitFor({ state: 'visible', timeout: 20_000 })
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)
        if (overflow) throw new Error(`horizontal overflow at ${width}px`)
        await page.screenshot({ path: path.join(artifactDir, `responsive-${width}.png`), fullPage: true })
      }
      result.viewports = [390, 1280]
      result.hero_visible = true
      result.primary_cta_visible = true
      result.no_horizontal_overflow = true
    } else if (mode === 'state-form') {
      await page.goto(url, { waitUntil: 'networkidle', timeout: 45_000 })
      await page.getByTestId('item-input').fill('Qualification item')
      await page.getByTestId('add-item').click()
      await page.getByTestId('item-list').getByText('Qualification item').waitFor({ state: 'visible', timeout: 10_000 })
      await page.reload({ waitUntil: 'networkidle', timeout: 45_000 })
      await page.getByTestId('item-list').getByText('Qualification item').waitFor({ state: 'visible', timeout: 10_000 })
      await page.screenshot({ path: path.join(artifactDir, 'state-form.png'), fullPage: true })
      result.form_interaction = true
      result.state_survived_reload = true
    } else {
      fail(`unsupported mode: ${mode}`)
    }
    if (consoleErrors.length) throw new Error(`browser console errors: ${consoleErrors.join(' | ')}`)
    result.passed = true
  } catch (error) {
    result.passed = false
    result.error = redact(error instanceof Error ? error.message : String(error))
    try {
      await page.screenshot({ path: path.join(artifactDir, `${mode}-failure.png`), fullPage: true })
    } catch {}
  } finally {
    await browser.close()
  }
  process.stdout.write(`${JSON.stringify(result)}\n`)
  if (!result.passed) process.exit(1)
}

main().catch((error) => fail(error instanceof Error ? error.stack || error.message : String(error)))
