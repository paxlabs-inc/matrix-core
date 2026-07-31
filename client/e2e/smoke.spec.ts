import { test, expect } from '@playwright/test'
import axe from 'axe-core'

test.describe('Matrix smoke', () => {
  test('login page renders', async ({ page }) => {
    await page.goto('/en/login')
    await expect(page.getByRole('main')).toBeVisible()
  })

  test('root redirects to default locale', async ({ page }) => {
    await page.goto('/')
    await expect(page).toHaveURL(/\/en/)
  })

  test('dev dashboard loads with escape hatch', async ({ page }) => {
    await page.goto('/en?dev=1')
    await expect(page.locator('main')).toBeVisible({ timeout: 30_000 })
  })

  test('Workforce command center route renders honest backend state', async ({ page }) => {
    await authorizeProtectedRoute(page)
    await page.goto('/en/workforce?dev=1')
    await expect(page.getByRole('heading', { level: 1, name: 'Workforce' })).toBeVisible({
      timeout: 30_000,
    })
    await expect(
      page
        .getByRole('heading', { level: 2, name: 'Activate your organization' })
        .or(page.getByRole('heading', { level: 2, name: 'Seven departments' }))
        .first(),
    ).toBeVisible()
  })

  test('Workforce command center remains viewport-contained on mobile', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await authorizeProtectedRoute(page)
    await page.goto('/en/workforce?dev=1')
    await expect(page.getByRole('heading', { level: 1, name: 'Workforce' })).toBeVisible({
      timeout: 30_000,
    })
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    )
    expect(overflow).toBeLessThanOrEqual(1)
  })

  test('Workforce command center has no serious accessibility violations', async ({ page }) => {
    await authorizeProtectedRoute(page)
    await page.goto('/en/workforce?dev=1')
    await expect(page.getByRole('heading', { level: 1, name: 'Workforce' })).toBeVisible({
      timeout: 30_000,
    })
    await page.addScriptTag({ content: axe.source })
    const violations = await page.evaluate(async () => {
      const axe = (
        window as typeof window & {
          axe: {
            run: (
              root: Document,
              options: { runOnly: { type: 'tag'; values: string[] } },
            ) => Promise<{
              violations: Array<{
                id: string
                impact: string | null
                help: string
                nodes: Array<{ target: string[]; failureSummary?: string }>
              }>
            }>
          }
        }
      ).axe
      const result = await axe.run(document, {
        runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21aa'] },
      })
      return result.violations.filter(
        (violation) => violation.impact === 'critical' || violation.impact === 'serious',
      )
    })
    expect(violations, JSON.stringify(violations, null, 2)).toEqual([])
  })

  test('privacy page is public', async ({ page }) => {
    await page.goto('/en/privacy')
    await expect(page.getByRole('heading', { level: 1 })).toContainText(/privacy/i)
  })

  test('locale prefix is shareable', async ({ page }) => {
    await page.goto('/de/login')
    await expect(page).toHaveURL(/\/de\/login/)
  })
})

async function authorizeProtectedRoute(page: import('@playwright/test').Page) {
  await page.context().addCookies([
    {
      name: 'mx-session',
      value: 'playwright-route-proof',
      domain: '127.0.0.1',
      path: '/',
      sameSite: 'Lax',
    },
  ])
}
