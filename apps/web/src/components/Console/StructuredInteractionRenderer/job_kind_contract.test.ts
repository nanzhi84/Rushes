import jobKinds from "../../../../../../go/internal/contracts/testdata/job_kinds.golden.json";
import { JOB_KIND_LABELS, PROGRESS_JOB_KINDS } from "./events";

describe("job kind 事实源", () => {
  it("Agent 等待集合与 Go catalog 对拍", () => {
    expect([...PROGRESS_JOB_KINDS].sort()).toEqual([...jobKinds.agent_waited].sort());
  });

  it("进度行标签与 Go catalog 对拍", () => {
    expect(JOB_KIND_LABELS).toEqual(jobKinds.progress_labels);
  });
});
