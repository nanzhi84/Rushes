# packages/timeline — 帧级时间线

- **六轨模型**（`materializer.py`）：`TimelineState` 固定六条轨——`visual_base`（primary_visual）/ `visual_overlay` / `original_audio` / `voiceover` / `bgm` / `subtitles`。`materialize_from_clips` 从「摘要级 clip（asset_id + 源秒区间 + 角色）」**从零组装**帧级时间线（fps、秒→帧换算、源钳位、主轨连续）。所有帧级细节封在本包边界内，别泄漏到上层。
- **patch 两阶段**（`patch_apply.py` + `contracts/patch.py`）：`TimelinePatchRequest`（Agent/上层给的语义请求，可含 anchor 待解析）→ 经 `anchor.resolve_anchor` 定位 → `ResolvedTimelinePatch`（帧级确定的操作）→ `apply_patch` 落新版本。合法 patch op 集合由 `check_contracts.py` 的 `EXPECTED_PATCH_OPS` 卡（delete_range / trim_clip / insert_clip / …）。
- **`validator.py` 是提交前不变量**（PRD §10.2）：`validate_timeline` 校验结构不变量，产 `TimelineValidationReport`；`build_timeline_invariant_hook` 供 agent_harness 在写事务提交前挂钩（校验失败发 `TimelineValidationFailed`）。
- `version_store.py` 存/取/回滚时间线版本（`TimelineVersionCreated` 事件 + `timeline_versions` 表）。
- 注意：本包**不在** `check_contracts.py` 的导入边界组里，但按约定只应被 `packages/tools` 调用。
