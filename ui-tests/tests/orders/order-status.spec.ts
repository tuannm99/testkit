import { test, expect } from '@playwright/test';

// The order id comes from the scenario (step ui.run, env ORDER_ID).
const orderId = process.env.ORDER_ID ?? '';
const expected = process.env.EXPECTED_STATUS ?? 'Đã thanh toán';

test('trang đơn hàng hiển thị đúng trạng thái và mã thanh toán', async ({ page }) => {
  expect(orderId, 'ORDER_ID is required').not.toBe('');
  await page.goto(`/orders/${orderId}`);
  await expect(page.getByTestId('order-id')).toHaveText(orderId);
  // Polls until the status is shown (the worker may still be processing).
  await expect(page.getByTestId('order-status')).toHaveText(expected);
  if (expected === 'Đã thanh toán') {
    await expect(page.getByTestId('payment-ref')).not.toHaveText('—');
  }
});

test('đơn không tồn tại trả 404', async ({ page }) => {
  const resp = await page.goto(`/orders/${orderId}-khong-ton-tai`);
  expect(resp?.status()).toBe(404);
});
