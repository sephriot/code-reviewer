import { expect, test } from '@playwright/test';

test('dashboard completes safe fixture review workflow', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Control Desk' })).toBeVisible();
  await expect(page.getByText('Read model online')).toBeVisible();
  const card = page.locator('.inbox-item').first();
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByText('canonical diff ready')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'Request review' })).toBeEnabled();
  await expect(page.getByText('Runtime enabled. Engine argv remains private.')).toBeVisible();
  await expect(page.locator('#selected-facts > div').filter({ hasText: 'Review runs' }).locator('dd')).toHaveText('1', { timeout: 20_000 });

  const repositoryFilter = page.getByLabel('Filter by repository');
  await page.locator('#proposal-body').fill('Unsaved feedback must stay on this PR.');
  await repositoryFilter.fill('no-such-repository');
  await expect(page.locator('.inbox-item:visible')).toHaveCount(0);
  await expect(page.locator('#selected-pr')).toBeVisible();
  await expect(page.locator('#inbox-message')).toContainText('feedback draft remains attached');
  await expect(page.locator('#inbox-count')).toContainText('0');

  await page.reload();
  await expect(repositoryFilter).toHaveValue('no-such-repository');
  await page.getByRole('button', { name: 'Clear filters' }).click();
  await expect(page.locator('.inbox-item:visible')).toHaveCount(1, { timeout: 20_000 });
  await expect(page.locator('#selected-pr')).toBeVisible();

  await expect(page.locator('#proposal-id')).not.toHaveValue('', { timeout: 20_000 });
  await expect(page.locator('#proposal-revision-id')).not.toHaveValue('');
  const approval = page.waitForResponse((response) =>
    response.url().includes('/api/v1/mutate/proposals/') && response.url().endsWith('/decisions') && response.request().method() === 'POST' && response.status() === 201,
  );
  await page.getByRole('button', { name: 'Approve feedback' }).click();
  await approval;
  await expect(page.locator('#inbox-count')).toContainText('0', { timeout: 20_000 });
});
