import { expect, test } from "@playwright/test";

const TOKEN = "e2e-token";

test("长期记忆可跨草稿查看、删除，并在下一回合停止注入", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);

  await page.getByLabel("消息输入").fill("E2E_MEMORY_WRITE");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_STORED" })).toBeVisible();

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  const targetDraftURL = page.url();
  await page.getByLabel("消息输入").fill("E2E_MEMORY_STATUS");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_PRESENT" })).toBeVisible();

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "设置" }).click();
  await expect(page.getByText("E2E 成片节奏偏快")).toBeVisible();
  await page.getByRole("button", { name: "删除长期记忆 e2e_pacing" }).click();
  await expect(page.getByText("还没有长期记忆")).toBeVisible();
  await page.getByRole("button", { name: "关闭设置" }).click();

  await page.goto(targetDraftURL);
  await page.getByLabel("消息输入").fill("E2E_MEMORY_STATUS");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_ABSENT" })).toBeVisible();
});
