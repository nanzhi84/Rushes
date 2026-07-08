import { expect, test } from "@playwright/test";

const TOKEN = "e2e-token";
const USER_MESSAGE = "把开头三秒删掉";

// e2e 栈未配置 LLM key（global-setup 不注入 RUSHES_*_API_KEY），
// 因此 harness 的 planner 落到空的 ScriptedPlanner：它对首个 turn 返回
// 一个纯 content 的 PlannerStep（"（脚本耗尽，结束本回合）"）。按 Spec B 的
// content 即散文协议，纯 content = 最终回复 → 落 messages(kind=reply) + TurnEnded。
// 该用例因此在 CI 无外网环境下确定性地走通「开始创作 → 发消息 → content 回复 →
// GET messages → 前端渲染助手气泡」这条 Spec B 新链路（断最终文本可见，稳定）。
const SCRIPTED_REPLY = "未配置模型密钥"; // 无密钥环境下 NoProviderPlanner 的回复锚点（e2e 不配真实密钥）

test("流式控制台：开始创作后发消息，对话流出现助手回复文本", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();

  // 全程走真实 UI/API：首页「开始创作」= POST /drafts → 直接进编辑器（无表单）。
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  await expect(page.getByText("剪辑对话")).toBeVisible();

  // 发送用户消息（POST /drafts/{id}/messages → 入 Turn Queue）。
  await page.getByLabel("消息输入").fill(USER_MESSAGE);
  await page.getByRole("button", { name: "发送" }).click();

  // 用户气泡出现（乐观渲染 + 落库回放）。
  await expect(page.getByText(USER_MESSAGE)).toBeVisible();

  // 助手回复气泡出现：kind=reply 的 article，携带非空散文文本。
  const replyBubble = page.locator('[data-message-kind="reply"]');
  await expect(replyBubble).toBeVisible();
  await expect(replyBubble).toContainText(SCRIPTED_REPLY);

  // 本轮结束后输入框恢复可用（TurnEnded 解锁）。
  await expect(page.getByLabel("消息输入")).toBeEnabled();
});
