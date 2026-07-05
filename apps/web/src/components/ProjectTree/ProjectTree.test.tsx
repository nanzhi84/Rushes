import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ProjectTreeProject } from "../../api/client";
import { ProjectTree } from "./ProjectTree";

const projects: ProjectTreeProject[] = [
  {
    project_id: "project_1",
    name: "Project A",
    status: "active",
    cases: [
      {
        case_id: "case_1",
        project_id: "project_1",
        name: "Case 001",
        status: "active"
      }
    ]
  }
];

describe("ProjectTree", () => {
  it("正确渲染 Project 和 Case 两级，且不出现 assets 或 memories 节点", () => {
    render(
      <ProjectTree
        projects={projects}
        expandedProjectIds={{}}
        selected={{ type: "projects" }}
        onToggleProject={vi.fn()}
        onSelectProjectsRoot={vi.fn()}
        onSelectProject={vi.fn()}
        onSelectCase={vi.fn()}
        onAction={vi.fn()}
      />
    );

    const tree = screen.getByLabelText("项目文件树");
    expect(within(tree).getByText("Project A")).toBeTruthy();
    expect(within(tree).getByText("Case 001")).toBeTruthy();
    expect(within(tree).queryByText(/assets/i)).toBeNull();
    expect(within(tree).queryByText(/memories/i)).toBeNull();
  });

  it("折叠 Project 后隐藏 Case", () => {
    const onToggleProject = vi.fn();
    render(
      <ProjectTree
        projects={projects}
        expandedProjectIds={{ project_1: false }}
        selected={{ type: "projects" }}
        onToggleProject={onToggleProject}
        onSelectProjectsRoot={vi.fn()}
        onSelectProject={vi.fn()}
        onSelectCase={vi.fn()}
        onAction={vi.fn()}
      />
    );

    expect(screen.queryByText("Case 001")).toBeNull();
    fireEvent.click(screen.getByLabelText("展开项目"));
    expect(onToggleProject).toHaveBeenCalledWith("project_1");
  });
});
