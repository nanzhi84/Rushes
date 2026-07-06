import { expect, test } from "@playwright/test";

const TOKEN = "e2e-token";
const PROJECT_NAME = "流式控制台项目";
const CASE_GOAL = "护肤口播流式用例";
const USER_MESSAGE = "把开头三秒删掉";

// e2e 栈未配置 LLM key（global-setup 不注入 RUSHES_*_API_KEY），
// 因此 harness 的 planner 落到空的 ScriptedPlanner：它对首个 turn 返回
// 一个纯 content 的 PlannerStep（"（脚本耗尽，结束本回合）"）。按 Spec B 的
// content 即散文协议，纯 content = 最终回复 → 落 messages(kind=reply) + TurnEnded。
// 该用例因此在 CI 无外网环境下确定性地走通「发消息 → content 回复 → GET messages
// → 前端渲染助手气泡」这条 Spec B 新链路（断最终文本可见，稳定）。
const SCRIPTED_REPLY = /脚本耗尽/;

test("流式控制台：发消息后对话流出现助手回复文本", async ({ page }) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "项目总览" })).toBeVisible();

  // 全程走真实 UI/API：新建项目 → 在项目页创建 Case 并进入控制台。
  await page.getByLabel("项目名称").fill(PROJECT_NAME);
  await page.getByRole("button", { name: "新建项目" }).click();
  await expect(page.getByRole("heading", { name: PROJECT_NAME })).toBeVisible();

  await page.getByLabel("目标文本").fill(CASE_GOAL);
  await page.getByRole("button", { name: "创建并进入控制台" }).click();
  await expect(page.getByText("剪辑控制台")).toBeVisible();

  // 发送用户消息（POST /cases/{cid}/messages → 入 Turn Queue）。
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
