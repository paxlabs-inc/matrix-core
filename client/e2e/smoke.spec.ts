import { test, expect } from '@playwright/test'

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
    await expect(page.locator('#main-content, main')).toBeVisible({ timeout: 30_000 })
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
