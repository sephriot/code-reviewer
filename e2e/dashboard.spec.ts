import { expect, test } from '@playwright/test';

test('dashboard reconciles and hydrates fixture pull request', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Control Desk' })).toBeVisible();
  await expect(page.getByText('Read model online')).toBeVisible();
  const card = page.getByRole('button', { name: /Fixture pull request/ });
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByText('canonical diff ready')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByRole('button', { name: 'Canonical evidence ready' })).toBeDisabled();
  await expect(page.getByText('Review runtime is disabled.')).toBeVisible();
});
