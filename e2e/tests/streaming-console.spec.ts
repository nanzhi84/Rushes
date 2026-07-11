import { expect, test } from "@playwright/test";

const TOKEN = "e2e-token";
const USER_MESSAGE = "把开头三秒删掉";

// E2E 栈不注入真实模型密钥，因此 Go agent 走确定性本地 fallback。该用例完整覆盖
// 「开始创作 → 202 入队 → text_delta → message_completed → turn_ended → 历史回放」。
const SCRIPTED_REPLY = "未配置模型密钥"; // 无密钥环境下 NoProviderPlanner 的回复锚点（e2e 不配真实密钥）

test("流式控制台：开始创作后发消息，对话流出现助手回复文本", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();

  // 全程走真实 UI/API：首页「开始创作」= POST /drafts → 直接进编辑器（无表单）。
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  await expect(page.getByRole("complementary", { name: "剪辑对话" })).toBeVisible();

  // 发送用户消息（POST /drafts/{id}/messages → 入 Turn Queue）。
  await page.getByLabel("消息输入").fill(USER_MESSAGE);
  await page.getByRole("button", { name: "发送" }).click();

  // 用户气泡出现（乐观渲染 + 落库回放）。
  await expect(page.getByText(USER_MESSAGE)).toBeVisible();

  // 助手回复气泡出现：kind=reply 的 article，携带非空散文文本。
  const replyBubble = page.locator('[data-message-kind="reply"]');
  await expect(replyBubble).toBeVisible();
  await expect(replyBubble).toContainText(SCRIPTED_REPLY);

  // turn-stream 终态后输入框恢复可用。
  await expect(page.getByLabel("消息输入")).toBeEnabled();
});
