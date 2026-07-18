import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AffectedMemoriesCard } from "./MemoryCards";

const memories = [
  { key: "e2e_pacing", statement: "成片节奏偏快" },
  { key: "voice_pref", statement: "偏好男声解说" }
];

describe("AffectedMemoriesCard", () => {
  it("列出被回退对话波及的记忆(键+摘要)并标注不可恢复", () => {
    render(
      <AffectedMemoriesCard
        memories={memories}
        onRetract={vi.fn()}
        onDismiss={vi.fn()}
        retracting={false}
      />
    );
    const card = screen.getByTestId("affected-memories-card");
    expect(card.textContent).toContain("e2e_pacing");
    expect(card.textContent).toContain("成片节奏偏快");
    expect(card.textContent).toContain("voice_pref");
    expect(card.textContent).toContain("偏好男声解说");
    expect(card.textContent).toContain("撤回后不可恢复");
  });

  it("点「撤回这些记忆」触发撤回,点「保留」触发关闭", () => {
    const onRetract = vi.fn();
    const onDismiss = vi.fn();
    render(
      <AffectedMemoriesCard
        memories={memories}
        onRetract={onRetract}
        onDismiss={onDismiss}
        retracting={false}
      />
    );
    fireEvent.click(screen.getByRole("button", { name: "撤回这些记忆" }));
    expect(onRetract).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "保留" }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("撤回进行中禁用两个按钮并显示进度文案", () => {
    render(
      <AffectedMemoriesCard
        memories={memories}
        onRetract={vi.fn()}
        onDismiss={vi.fn()}
        retracting={true}
      />
    );
    expect((screen.getByRole("button", { name: "撤回中…" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "保留" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
