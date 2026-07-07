"""M9 加验场景：真实风景素材静音混剪 + 上传 BGM（纯剪辑，无配音）。"""

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
    CaseDriver,
    RunError,
    RushesClient,
    ffprobe_duration_s,
    load_dotenv,
    stage_log,
    start_autostart,
    summarize_case_state,
    unique_id,
)

JsonMap = Mapping[str, Any]


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="M9 风景混剪：真实素材静音剪辑手动驱动脚本。")
    parser.add_argument("--api-url", default=os.environ.get("RUSHES_API_URL", DEFAULT_API_URL))
    parser.add_argument("--token", default=os.environ.get("RUSHES_API_TOKEN", DEFAULT_TOKEN))
    parser.add_argument("--workspace", type=Path, required=True)
    parser.add_argument("--footage-dir", type=Path, required=True, help="风景素材目录（mov/mp4）")
    parser.add_argument("--bgm", type=Path, default=None, help="BGM 音频文件（可选）")
    parser.add_argument("--clips", type=int, default=5, help="导入的素材段数上限")
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--autostart", action="store_true")
    parser.add_argument("--llm-timeout", type=float, default=240.0)
    parser.add_argument("--job-timeout", type=float, default=600.0)
    parser.add_argument("--render-timeout", type=float, default=900.0)
    return parser.parse_args()


def _footage_files(footage_dir: Path, limit: int) -> list[Path]:
    files = sorted(
        path
        for path in footage_dir.iterdir()
        if path.suffix.lower() in {".mov", ".mp4"} and not path.name.startswith(".")
    )
    if not files:
        raise RunError(f"素材目录没有视频文件：{footage_dir}")
    return files[: max(1, limit)]


def main() -> int:
    args = parse_args()
    load_dotenv()
    footage_dir = args.footage_dir.expanduser().resolve()
    out_dir = args.out_dir.expanduser().resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    footage = _footage_files(footage_dir, args.clips)
    bgm_path = args.bgm.expanduser().resolve() if args.bgm is not None else None

    group = None
    if args.autostart:
        fs_roots = [footage_dir]
        if bgm_path is not None:
            fs_roots.append(bgm_path.parent)
        group = start_autostart(
            api_url=args.api_url,
            token=args.token,
            workspace=args.workspace.expanduser().resolve(),
            fs_roots=fs_roots,
        )
    client = RushesClient(api_url=args.api_url, token=args.token)
    try:
        return _run(client, footage=footage, bgm_path=bgm_path, out_dir=out_dir, args=args)
    finally:
        client.close()
        if group is not None:
            group.stop()


def _run(
    client: RushesClient,
    *,
    footage: list[Path],
    bgm_path: Path | None,
    out_dir: Path,
    args: argparse.Namespace,
) -> int:
    project_id = unique_id("m9_scenery_project")
    case_id = unique_id("m9_scenery_case")
    stage_log(
        f"风景混剪：创建项目、导入 {len(footage)} 段素材" + ("（含 BGM）" if bgm_path else "")
    )
    client.create_project(project_id=project_id, name="M9 风景混剪")

    imported: list[str] = []
    for index, path in enumerate(footage, start=1):
        asset_id = unique_id(f"asset_scenery_{index:02d}")
        client.import_local_material(
            project_id=project_id,
            asset_id=asset_id,
            path=path,
        )
        imported.append(asset_id)
        stage_log(f"已导入素材 {index}/{len(footage)}：{path.name}")
    if bgm_path is not None:
        bgm_asset_id = unique_id("asset_bgm")
        client.import_local_material(
            project_id=project_id,
            asset_id=bgm_asset_id,
            path=bgm_path,
        )
        stage_log(f"已导入 BGM：{bgm_path.name}")

    client.create_case(
        project_id=project_id,
        case_id=case_id,
        name="风景混剪",
        goal="用风景素材剪一条约 30 秒的静音混剪，配上传的 BGM，无配音无字幕。",
    )
    for asset_id in imported:
        client.select_case_asset(project_id=project_id, case_id=case_id, asset_id=asset_id)

    driver = CaseDriver(client=client, project_id=project_id, case_id=case_id, scenario="scenery")
    client.enqueue_message(
        project_id=project_id,
        case_id=case_id,
        content=(
            "把这些风景素材剪成一条 30 秒左右的混剪。不需要配音（静音处理），"
            "挑画面质量好的片段，节奏明快一点，先给我预览。"
        ),
        message_id=unique_id("msg"),
    )

    driver.wait_until(
        "标注、静音确认与粗剪链路启动",
        lambda state: _audio_mode(state) == "silent" or _timeline_version(state) is not None,
        timeout_s=args.llm_timeout + args.job_timeout,
        idle_nudge=(
            "请检查当前 case 状态（素材标注/audio_plan/后台任务结果），"
            "只做尚未完成的下一步，不要重复已完成的步骤。"
        ),
    )
    case = driver.wait_until(
        "检索、timeline 与预览完成",
        lambda state: (
            _timeline_version(state) is not None
            and _string_field(state, "preview_current_id") is not None
        ),
        timeout_s=args.llm_timeout + args.job_timeout + args.render_timeout,
        idle_nudge=(
            "请检查当前 case 状态，cut_plan/candidate 就绪后依次完成检索、"
            "timeline 与预览渲染，不要重复已完成的步骤。"
        ),
    )
    preview_id = _string_field(case, "preview_current_id")
    if preview_id is not None:
        client.mark_preview_viewed(project_id=project_id, case_id=case_id, preview_id=preview_id)
        stage_log(f"已标记预览已观看：{preview_id}")

    client.enqueue_message(
        project_id=project_id,
        case_id=case_id,
        content="预览可以。跳过字幕，BGM 用我上传的那首，然后导出 MP4。",
        message_id=unique_id("msg"),
    )
    final_case = driver.wait_until(
        "BGM 合成与最终导出",
        lambda state: _string_field(state, "export_current_id") is not None,
        timeout_s=args.llm_timeout + args.render_timeout + args.job_timeout,
        idle_nudge=(
            "请检查当前 case 状态，按已确认结果完成 BGM patch 与最终导出，不要重复已完成的步骤。"
        ),
    )

    export_id = _string_field(final_case, "export_current_id")
    if export_id is None:
        raise RunError(f"导出完成但缺少 export_current_id：{summarize_case_state(final_case)}")
    output_path = out_dir / f"{case_id}_{export_id}.mp4"
    client.download_export(export_id=export_id, output_path=output_path)
    duration = ffprobe_duration_s(output_path)
    if duration <= 5.0:
        raise RunError(f"导出 MP4 时长异常：{duration:.2f}s")
    stage_log(f"风景混剪完成：{output_path} duration={duration:.2f}s")
    return 0


def _audio_mode(case_state: JsonMap) -> str | None:
    audio_plan = case_state.get("audio_plan")
    if not isinstance(audio_plan, Mapping):
        return None
    value = audio_plan.get("mode")
    return str(value) if value is not None else None


def _timeline_version(case_state: JsonMap) -> int | None:
    value = case_state.get("timeline_current_version")
    return value if type(value) is int else None


def _string_field(case_state: JsonMap, key: str) -> str | None:
    value = case_state.get(key)
    return value if isinstance(value, str) and value else None


if __name__ == "__main__":
    raise SystemExit(main())
