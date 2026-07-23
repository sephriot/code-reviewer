import { expect, test } from '@playwright/test';

test('dashboard completes safe fixture review workflow', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Control Desk' })).toBeVisible();
  await expect(page.getByText('Read model online')).toBeVisible();
  const card = page.locator('.inbox-item').first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByText('canonical diff ready')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'Canonical evidence ready' })).toBeDisabled();
  await expect(page.getByText('Runtime enabled. Engine argv remains private.')).toBeVisible();
  await expect(page.locator('#selected-facts > div').filter({ hasText: 'Review runs' }).locator('dd')).toHaveText('1', { timeout: 20_000 });

  await expect(page.locator('#proposal-id')).not.toHaveValue('', { timeout: 20_000 });
  await expect(page.locator('#proposal-revision-id')).not.toHaveValue('');
  const approval = page.waitForResponse((response) =>
    response.url().includes('/api/v1/mutate/proposals/') && response.url().endsWith('/decisions') && response.request().method() === 'POST' && response.status() === 201,
  );
  await page.getByRole('button', { name: 'Approve revision' }).click();
  await approval;

  await expect(page.locator('#proposal-revision-id')).not.toHaveValue('', { timeout: 20_000 });
  const simulation = page.waitForResponse((response) =>
    response.url().includes('/publication/simulate') && response.request().method() === 'POST' && response.status() === 201,
  );
  await page.getByRole('button', { name: 'Simulate approved revision' }).click();
  await simulation;
  await expect(page.locator('#timeline-items')).toContainText('publication_effect', { timeout: 20_000 });
});
