import { expect, test } from "@playwright/test";

const TOKEN = "e2e-token";
const USER_MESSAGE = "把开头三秒删掉";

// E2E 栈不注入真实模型密钥，因此 Go agent 走确定性本地 fallback。该用例完整覆盖
// 「开始创作 → 202 入队 → text_delta → message_completed → turn_ended → 历史回放」。
const SCRIPTED_REPLY = "未配置模型密钥"; // 无密钥环境下 NoProviderPlanner 的回复锚点（e2e 不配真实密钥）

test("token 首次授权后跨浏览器会话保留", async ({ page, context }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await expect(page).not.toHaveURL(/#t=/);
  expect(await page.evaluate(() => window.localStorage.getItem("rushes.launch_token"))).toBe(TOKEN);

  await page.close();
  const reopened = await context.newPage();
  await reopened.goto("/");

  await expect(reopened.getByRole("heading", { name: "草稿" })).toBeVisible();
  await expect(reopened.getByText("请从后端启动 URL 打开 Rushes")).toHaveCount(0);
});

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

test("Stop Gate UI 生命周期：rejected 可区分，blocked→passed 原位更新", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);

  await page.getByLabel("消息输入").fill("E2E_STOP_GATE_LIFECYCLE");
  await page.getByRole("button", { name: "发送" }).click();

  const tools = page.getByTestId("tool-activity-group");
  await expect(tools).toContainText("有调用未执行");
  await expect(tools.getByText("未执行", { exact: true })).toBeVisible();
  await expect(tools).not.toContainText("工具执行失败");
  await expect(page.getByText("检查时间线", { exact: true })).toHaveCount(0);

  const gate = page.getByTestId("stop-gate-group");
  await expect(gate).toHaveAttribute("data-stop-gate-status", "blocked");
  await expect(gate).toContainText("主视觉时长尚未满足目标");
  await expect(gate).toContainText("validation:draft_e2e:v1");
  await expect(gate).toContainText("<1ms");

  await expect(gate).toHaveAttribute("data-stop-gate-status", "passed");
  await expect(gate).toHaveAttribute("data-timeline-id", "draft_e2e:v2");
  await expect(gate).toContainText("终验通过");
  await expect(gate).toHaveCount(1);
  await expect(page.locator('[data-message-kind="reply"]')).toContainText("E2E_STOP_GATE_PASSED");
});
