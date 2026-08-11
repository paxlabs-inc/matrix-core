import { test, expect } from '@playwright/test'

test('Workforce owner completes a seven-department verified-receipt chain', async ({
  page,
}) => {
  test.skip(
    process.env.WORKFORCE_BROWSER_INTEGRATION !== '1',
    'requires a real loopback workforced, Postgres, Vault, scheduler, and isolated workers',
  )
  test.setTimeout(15 * 60_000)
  await page.goto('/en/workforce?dev=1')
  await expect(page.getByRole('heading', { level: 1, name: 'Workforce' })).toBeVisible({
    timeout: 30_000,
  })
  const cookieDialog = page.getByRole('dialog', { name: 'We use cookies' })
  if (await cookieDialog.isVisible()) {
    await cookieDialog.getByRole('button', { name: 'Reject non-essential' }).click()
  }

  const activation = page.getByRole('heading', {
    level: 2,
    name: 'Activate your organization',
  })
  const departmentMetric = page.getByText('7/7', { exact: true }).first()
  const activate = page.getByRole('button', { name: 'Review, sign, and activate' })
  await expect(activation.or(departmentMetric).first()).toBeVisible({ timeout: 30_000 })
  if (await activation.isVisible()) {
    await page.getByLabel('Organization name').fill('Centra AI Local Workforce')
  }
  await expect
    .poll(
      async () => {
        if (await departmentMetric.isVisible()) return 'active'
        if ((await activation.isVisible()) && (await activate.isEnabled())) return 'activate'
        return 'loading'
      },
      { timeout: 30_000 },
    )
    .not.toBe('loading')
  if (!(await departmentMetric.isVisible())) {
    const previewResponse = page.waitForResponse(
      (response) =>
        response.url().endsWith('/api/workforce/v1/workforce/activation/preview') &&
        response.request().method() === 'POST',
    )
    await activate.click()
    const response = await previewResponse
    expect(
      response.status(),
      JSON.stringify({
        url: response.url(),
        location: response.headers()['location'],
      }),
    ).toBe(200)
    await expect(response.json()).resolves.toMatchObject({
      schema_version: 'workforce.control.v1',
      seed: {
        organization: {
          organization_id: expect.any(String),
          departments: expect.any(Array),
        },
      },
      skill_contracts: expect.any(Array),
    })
  }

  await expect(page.getByText('7/7', { exact: true }).first()).toBeVisible({ timeout: 30_000 })
  await expect(page.getByText('21/21', { exact: true }).first()).toBeVisible()
  await page.getByRole('button', { name: 'Create Work Order' }).click()
  await page
    .getByLabel('Objective')
    .fill(
      'Produce a receipt-backed launch readiness chain using exact predecessor evidence and no publication or payment',
    )
  await page
    .getByLabel('Acceptance criteria')
    .fill('evidence_hash: the fenced Developer observation is content-addressed')
  await page.getByLabel('Project ID').fill('project:matrix')
  await page.getByLabel('Workspace ID').fill('workspace:matrix')
  await page.getByLabel('Scoped files').fill('workforce/internal/wakeruntime/recovery.go')
  await page.getByLabel('Scoped symbols').fill('RunClaim')
  for (const department of [
    'Executive',
    'Research & Development',
    'Marketing & Social',
    'Legal',
    'Accounting',
    'Back Office',
  ]) {
    const checkbox = page.getByLabel(department, { exact: true })
    await checkbox.locator('..').click()
    await expect(checkbox).toBeChecked()
  }
  await expect(page.getByLabel('Model ID')).toHaveValue('mimo-v2.5-pro')
  await page.getByLabel('MGS genome reference').fill('mgs:workforce:v1')
  await page.getByLabel('MGS genome SHA-256').fill('a'.repeat(64))
  await page.getByRole('button', { name: 'Review the exact change' }).click()
  await expect(page.getByText('Prepared Work Order does not match')).toHaveCount(0)
  await page.getByRole('button', { name: 'Approve and sign' }).click()

  const workOrderCard = page
    .getByRole('article')
    .filter({
      has: page.getByRole('heading', {
        name: 'Produce a receipt-backed launch readiness chain using exact predecessor evidence and no publication or payment',
      }),
    })
    .first()
  await expect(workOrderCard).toBeVisible({ timeout: 30_000 })
  await expect(workOrderCard.getByText('completed', { exact: true })).toBeVisible({
    timeout: 12 * 60_000,
  })

  const verifiedMetric = page
    .getByRole('article')
    .filter({ hasText: 'Verified completion' })
    .first()
  await expect(verifiedMetric.getByText('1', { exact: true })).toBeVisible()
  await page.getByRole('tab', { name: 'Receipts' }).click()
  const receiptPanel = page.getByRole('tabpanel')
  await expect(receiptPanel.locator('article')).toHaveCount(7)
  await expect(receiptPanel.getByText('Verified completion', { exact: true })).toHaveCount(1)
})
