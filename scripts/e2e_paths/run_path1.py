"""Manual REST/SSE driver for M9 path 1: original voice rough cut."""

from __future__ import annotations

import argparse
import os
import sys
from collections.abc import Mapping
from pathlib import Path
from typing import Any

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))

from client import (
    DEFAULT_API_URL,
    DEFAULT_TOKEN,
    REPO_ROOT,
    DraftDriver,
    ManagedProcessGroup,
    RunError,
    RushesClient,
    ffprobe_duration_s,
    load_dotenv,
    stage_log,
    start_autostart,
    summarize_draft_state,
    unique_id,
)

JsonMap = Mapping[str, Any]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="M9 路径 1：原声口播粗剪手动驱动脚本。")
    parser.add_argument("--api-url", default=os.environ.get("RUSHES_API_URL", DEFAULT_API_URL))
    parser.add_argument("--token", default=os.environ.get("RUSHES_API_TOKEN", DEFAULT_TOKEN))
    parser.add_argument(
        "--workspace",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-workspace" / "path1",
        help="--autostart 时使用的 Rushes workspace。",
    )
    parser.add_argument(
        "--voiceover-video",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-fixtures" / "path1_voiceover_video.mp4",
        help="路径 1 输入视频；可替换为实拍带口播视频。",
    )
    parser.add_argument(
        "--out-dir",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-output" / "path1",
        help="导出 MP4 下载目录。",
    )
    parser.add_argument("--autostart", action="store_true", help="自动启动 API + worker。")
    parser.add_argument("--llm-timeout", type=float, default=120.0)
    parser.add_argument("--job-timeout", type=float, default=300.0)
    parser.add_argument("--render-timeout", type=float, default=300.0)
    parser.add_argument("--min-duration", type=float, default=5.0)
    parser.add_argument("--max-duration", type=float, default=120.0)
    return parser.parse_args()


def run(args: argparse.Namespace) -> int:
    load_dotenv()
    api_url = str(args.api_url)
    token = str(args.token)
    workspace = _resolve_path(args.workspace)
    voiceover_video = _resolve_path(args.voiceover_video)
    out_dir = _resolve_path(args.out_dir)
    llm_timeout = float(args.llm_timeout)
    job_timeout = float(args.job_timeout)
    render_timeout = float(args.render_timeout)
    min_duration = float(args.min_duration)
    max_duration = float(args.max_duration)

    if not voiceover_video.exists():
        raise RunError(f"路径 1 输入视频不存在：{voiceover_video}")
    out_dir.mkdir(parents=True, exist_ok=True)

    group: ManagedProcessGroup | None = None
    if bool(args.autostart):
        group = start_autostart(
            api_url=api_url,
            token=token,
            workspace=workspace,
            fs_roots=[voiceover_video.parent, out_dir],
        )

    try:
        with RushesClient(api_url, token) as client:
            client.wait_ready(timeout_s=60.0 if bool(args.autostart) else 10.0)
            return run_workflow(
                client,
                voiceover_video=voiceover_video,
                out_dir=out_dir,
                llm_timeout=llm_timeout,
                job_timeout=job_timeout,
                render_timeout=render_timeout,
                min_duration=min_duration,
                max_duration=max_duration,
            )
    finally:
        if group is not None:
            group.stop()


def run_workflow(
    client: RushesClient,
    *,
    voiceover_video: Path,
    out_dir: Path,
    llm_timeout: float,
    job_timeout: float,
    render_timeout: float,
    min_duration: float,
    max_duration: float,
) -> int:
    draft_id = unique_id("m9_path1_draft")
    asset_id = unique_id("asset_voiceover")
    stage_log("路径 1：创建草稿、导入口播视频")
    client.create_draft(
        draft_id=draft_id,
        name="M9 路径 1 原声口播粗剪",
        goal="原声口播粗剪，确认口癖候选后预览、patch、跳过字幕 BGM 并导出。",
    )
    client.import_local_material(
        draft_id=draft_id,
        asset_id=asset_id,
        path=voiceover_video,
    )

    driver = DraftDriver(client=client, draft_id=draft_id, scenario="path1")
    client.enqueue_message(
        draft_id=draft_id,
        content="帮我把这条口播粗剪一下，先用原声，识别口癖后给我粗剪预览。",
        message_id=unique_id("msg"),
    )

    driver.wait_until(
        "进入原声粗剪链路",
        lambda state: (
            _audio_mode(state) == "rough_cut"
            or "audio_mode" in driver.seen_decision_types
            or state.get("cut_plan") is not None
            or _timeline_version(state) is not None
            or _has_running_job_kind(state, ("asr", "rough", "render"))
        ),
        timeout_s=llm_timeout,
    )
    draft = driver.wait_until(
        "ASR、口癖候选确认、粗剪 timeline 与预览完成",
        lambda state: (
            _timeline_version(state) is not None
            and _string_field(state, "preview_current_id") is not None
            and (
                state.get("cut_plan") is not None
                or "approve_speech_cut" in driver.seen_decision_types
            )
        ),
        timeout_s=llm_timeout + job_timeout + render_timeout,
        idle_nudge=(
            "请检查当前草稿状态（audio_plan/cut_plan/后台任务结果），"
            "只做尚未完成的下一步，不要重复已完成的步骤。"
        ),
    )
    _mark_preview_viewed(client, draft_id, draft)

    version_before = _timeline_version(draft)
    if version_before is None:
        raise RunError(f"粗剪后缺少 timeline version：{summarize_draft_state(draft)}")
    client.enqueue_message(
        draft_id=draft_id,
        content="把 7 秒附近那段删掉。",
        message_id=unique_id("msg"),
    )
    patched_draft = driver.wait_until(
        "patch 后 timeline version 递增",
        lambda state: (_timeline_version(state) or 0) > version_before,
        timeout_s=llm_timeout + render_timeout,
        idle_nudge="继续应用 7 秒附近删除 patch。",
    )
    _mark_preview_viewed(client, draft_id, patched_draft)

    client.enqueue_message(
        draft_id=draft_id,
        content="这版可以，字幕和 BGM 都跳过，导出 MP4。",
        message_id=unique_id("msg"),
    )
    final_draft = driver.wait_until(
        "字幕/BGM 跳过并完成最终导出",
        lambda state: _string_field(state, "export_current_id") is not None,
        timeout_s=llm_timeout + render_timeout + job_timeout,
        idle_nudge="继续跳过字幕和 BGM，然后导出最终 MP4。",
    )
    driver.require_decisions_seen(["approve_speech_cut", "subtitle", "bgm", "export"])

    export_id = _string_field(final_draft, "export_current_id")
    if export_id is None:
        raise RunError(f"导出完成但缺少 export_current_id：{summarize_draft_state(final_draft)}")
    output_path = out_dir / f"{draft_id}_{export_id}.mp4"
    client.download_export(export_id=export_id, output_path=output_path)
    duration = ffprobe_duration_s(output_path)
    if not min_duration <= duration <= max_duration:
        raise RunError(
            f"导出 MP4 时长不在预期区间：duration={duration:.2f}s "
            f"expected=[{min_duration:.2f}, {max_duration:.2f}]"
        )
    stage_log(f"路径 1 完成：{output_path} duration={duration:.2f}s")
    return 0


def _resolve_path(path: Path) -> Path:
    return path.expanduser().resolve(strict=False)


def _audio_mode(draft_state: JsonMap) -> str | None:
    audio_plan = draft_state.get("audio_plan")
    if not isinstance(audio_plan, Mapping):
        return None
    value = audio_plan.get("mode")
    return str(value) if value is not None else None


def _timeline_version(draft_state: JsonMap) -> int | None:
    value = draft_state.get("timeline_current_version")
    return value if type(value) is int else None


def _string_field(draft_state: JsonMap, key: str) -> str | None:
    value = draft_state.get(key)
    return value if isinstance(value, str) and value else None


def _has_running_job_kind(draft_state: JsonMap, needles: tuple[str, ...]) -> bool:
    running_jobs = draft_state.get("running_jobs")
    if not isinstance(running_jobs, list):
        return False
    for job in running_jobs:
        if not isinstance(job, Mapping):
            continue
        kind = str(job.get("kind") or "").lower()
        if any(needle in kind for needle in needles):
            return True
    return False


def _mark_preview_viewed(
    client: RushesClient,
    draft_id: str,
    draft_state: JsonMap,
) -> None:
    preview_id = _string_field(draft_state, "preview_current_id")
    if preview_id is None:
        return
    client.mark_preview_viewed(draft_id=draft_id, preview_id=preview_id)
    stage_log(f"已标记预览已看：{preview_id}")


def main() -> int:
    try:
        return run(parse_args())
    except RunError as exc:
        stage_log(f"失败：{exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
