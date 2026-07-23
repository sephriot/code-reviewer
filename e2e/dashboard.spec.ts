import { expect, test } from '@playwright/test';

test('dashboard starts isolated local control desk', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Control Desk' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Review setup' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Runtime activity' })).toBeVisible();
  await expect(page.getByText('Read model online')).toBeVisible();
  await expect(page.getByText('No observed pull requests yet.')).toBeVisible();
  await expect(page.getByText('Review runtime is disabled.')).toBeVisible();
});
