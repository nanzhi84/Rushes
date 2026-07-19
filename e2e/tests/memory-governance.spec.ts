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
  // 写入成功当场可见：「已记住长期记忆」卡片。
  await expect(page.getByTestId("memory-updated-card")).toBeVisible();

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
  // 面板就地编辑 statement：走 PATCH 落库并标注「手动修订」。
  await page.getByRole("button", { name: "编辑长期记忆 e2e_pacing" }).click();
  await page.getByRole("textbox", { name: "编辑长期记忆 e2e_pacing" }).fill("E2E 手动改为整体更紧凑");
  await page.getByRole("button", { name: "保存" }).click();
  await expect(page.getByText("E2E 手动改为整体更紧凑")).toBeVisible();
  await expect(page.getByText("手动修订")).toBeVisible();
  await page.getByRole("button", { name: "删除长期记忆 e2e_pacing" }).click();
  await expect(page.getByText("还没有长期记忆")).toBeVisible();
  await page.getByRole("button", { name: "关闭设置" }).click();

  await page.goto(targetDraftURL);
  await page.getByLabel("消息输入").fill("E2E_MEMORY_STATUS");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_ABSENT" })).toBeVisible();
});

test("编辑并重发后，被回退对话形成的长期记忆可在卡片上一并撤回", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);

  // 写入一条以「这条消息」为证据的长期记忆。
  await page.getByLabel("消息输入").fill("E2E_MEMORY_WRITE");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(
    page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_STORED" })
  ).toBeVisible();
  await expect(page.getByTestId("memory-updated-card")).toBeVisible();
  await expect(page.getByLabel("消息输入")).toBeEnabled({ timeout: 60_000 });

  // 编辑并重发这条消息:回退到它之前——它正是记忆的证据,故记忆被波及。新内容只读记忆状态
  // (E2E_MEMORY_STATUS),不再写入,避免复写把证据改到新消息上。
  const userMessage = page
    .getByLabel("消息列表")
    .locator("article")
    .filter({ has: page.locator("[data-user-message]"), hasText: "E2E_MEMORY_WRITE" })
    .last();
  await userMessage.getByRole("button", { name: "编辑并重发" }).click();
  await page.getByRole("textbox", { name: "编辑消息" }).fill("E2E_MEMORY_STATUS");
  await page.getByRole("button", { name: "重发", exact: true }).click();

  // 波及卡片出现,列出 e2e_pacing;点「撤回这些记忆」后卡片消失。
  const card = page.getByTestId("affected-memories-card");
  await expect(card).toBeVisible();
  await expect(card).toContainText("e2e_pacing");
  await card.getByRole("button", { name: "撤回这些记忆" }).click();
  await expect(card).toBeHidden();

  // 设置面板确认记忆已被撤回。
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "设置" }).click();
  await expect(page.getByText("还没有长期记忆")).toBeVisible();
});

test("写入回执默认展示语义证据，并可一键撤回且同步设置面板", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);

  await page.getByLabel("消息输入").fill("E2E_MEMORY_WRITE");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(
    page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_MEMORY_STORED" })
  ).toBeVisible();

  const receipt = page.getByTestId("memory-updated-card");
  await expect(receipt).toContainText("E2E 成片节奏偏快");
  await expect(receipt).toContainText("原话：“E2E_MEMORY_WRITE”");
  await receipt.getByRole("button", { name: "撤回", exact: true }).click();
  await expect(receipt.getByText("已撤回")).toBeVisible();

  await receipt.getByRole("button", { name: "在设置中查看和编辑" }).click();
  await expect(page.getByText("还没有长期记忆")).toBeVisible();
});
