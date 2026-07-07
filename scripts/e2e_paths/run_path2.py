"""Manual REST/SSE driver for M9 path 2: TTS product recommendation video."""

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
    CaseDriver,
    ManagedProcessGroup,
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

BROLL_FILENAMES = ("path2_broll_01.mp4", "path2_broll_02.mp4", "path2_broll_03.mp4")
IMAGE_FILENAME = "path2_product_image.png"
SCRIPT_FILENAME = "path2_script.txt"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="M9 路径 2：TTS 种草视频手动驱动脚本。")
    parser.add_argument("--api-url", default=os.environ.get("RUSHES_API_URL", DEFAULT_API_URL))
    parser.add_argument("--token", default=os.environ.get("RUSHES_API_TOKEN", DEFAULT_TOKEN))
    parser.add_argument(
        "--workspace",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-workspace" / "path2",
        help="--autostart 时使用的 Rushes workspace。",
    )
    parser.add_argument(
        "--fixture-dir",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-fixtures",
        help="路径 2 素材目录，默认 .e2e-paths-fixtures/。",
    )
    parser.add_argument(
        "--script-file",
        type=Path,
        default=None,
        help="覆盖默认 path2_script.txt 的种草文案文件。",
    )
    parser.add_argument(
        "--out-dir",
        type=Path,
        default=REPO_ROOT / ".e2e-paths-output" / "path2",
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
    fixture_dir = _resolve_path(args.fixture_dir)
    out_dir = _resolve_path(args.out_dir)
    script_file = (
        _resolve_path(args.script_file)
        if isinstance(args.script_file, Path)
        else fixture_dir / SCRIPT_FILENAME
    )
    llm_timeout = float(args.llm_timeout)
    job_timeout = float(args.job_timeout)
    render_timeout = float(args.render_timeout)
    min_duration = float(args.min_duration)
    max_duration = float(args.max_duration)

    material_paths = _fixture_paths(fixture_dir, script_file)
    for path in material_paths:
        if not path.exists():
            raise RunError(f"路径 2 输入素材不存在：{path}")
    out_dir.mkdir(parents=True, exist_ok=True)

    group: ManagedProcessGroup | None = None
    if bool(args.autostart):
        group = start_autostart(
            api_url=api_url,
            token=token,
            workspace=workspace,
            fs_roots=[fixture_dir, out_dir],
        )

    try:
        with RushesClient(api_url, token) as client:
            client.wait_ready(timeout_s=60.0 if bool(args.autostart) else 10.0)
            return run_workflow(
                client,
                fixture_dir=fixture_dir,
                script_file=script_file,
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
    fixture_dir: Path,
    script_file: Path,
    out_dir: Path,
    llm_timeout: float,
    job_timeout: float,
    render_timeout: float,
    min_duration: float,
    max_duration: float,
) -> int:
    project_id = unique_id("m9_path2_project")
    case_id = unique_id("m9_path2_case")
    stage_log("路径 2：创建项目、导入 B-roll/图片、创建 Case")
    client.create_project(project_id=project_id, name="M9 路径 2 TTS 种草")

    imported_asset_ids: list[str] = []
    for index, filename in enumerate(BROLL_FILENAMES, start=1):
        asset_id = unique_id(f"asset_broll_{index}")
        client.import_local_material(
            project_id=project_id,
            asset_id=asset_id,
            path=fixture_dir / filename,
        )
        imported_asset_ids.append(asset_id)
    image_asset_id = unique_id("asset_product_image")
    client.import_local_material(
        project_id=project_id,
        asset_id=image_asset_id,
        path=fixture_dir / IMAGE_FILENAME,
    )
    imported_asset_ids.append(image_asset_id)

    client.create_case(
        project_id=project_id,
        case_id=case_id,
        name="M9 路径 2",
        goal="无声 B-roll + 图，按种草文案生成 TTS、理解素材、compose_initial、字幕/BGM 并导出。",
    )
    for asset_id in imported_asset_ids:
        client.select_case_asset(project_id=project_id, case_id=case_id, asset_id=asset_id)

    script_text = script_file.read_text(encoding="utf-8").strip()
    if not script_text:
        raise RunError(f"路径 2 文案为空：{script_file}")

    driver = CaseDriver(client=client, project_id=project_id, case_id=case_id, scenario="path2")
    client.enqueue_message(
        project_id=project_id,
        case_id=case_id,
        content=f"用这段文案做一条种草视频，配 TTS。\n\n{script_text}",
        message_id=unique_id("msg"),
    )

    driver.wait_until(
        "内容计划确认或进入 TTS 链路",
        lambda state: (
            state.get("content_plan") is not None
            or _audio_mode(state) == "tts"
            or _has_running_job_kind(state, ("tts",))
        ),
        timeout_s=llm_timeout,
        idle_nudge="继续先生成并确认内容计划。",
    )
    case = driver.wait_until(
        "TTS、素材理解、timeline 与预览完成",
        lambda state: (
            _timeline_version(state) is not None
            and _string_field(state, "preview_current_id") is not None
            and _audio_mode(state) == "tts"
        ),
        timeout_s=llm_timeout + job_timeout + render_timeout,
        idle_nudge="继续完成 TTS、素材理解、timeline 组装与预览渲染。",
    )
    _mark_preview_viewed(client, project_id, case_id, case)

    client.enqueue_message(
        project_id=project_id,
        case_id=case_id,
        content="预览确认，字幕选一个模板，BGM 从项目已上传素材中选择、无则跳过，然后导出 MP4。",
        message_id=unique_id("msg"),
    )
    final_case = driver.wait_until(
        "字幕、BGM（从已上传素材选择或跳过）并完成最终导出",
        lambda state: _string_field(state, "export_current_id") is not None,
        timeout_s=llm_timeout + render_timeout + job_timeout,
        idle_nudge="继续确认字幕模板、BGM（从已上传素材选择或跳过），然后导出最终 MP4。",
    )
    driver.require_decisions_seen(["approve_content_plan", "subtitle", "bgm", "export"])

    export_id = _string_field(final_case, "export_current_id")
    if export_id is None:
        raise RunError(f"导出完成但缺少 export_current_id：{summarize_case_state(final_case)}")
    output_path = out_dir / f"{case_id}_{export_id}.mp4"
    client.download_export(export_id=export_id, output_path=output_path)
    duration = ffprobe_duration_s(output_path)
    if not min_duration <= duration <= max_duration:
        raise RunError(
            f"导出 MP4 时长不在预期区间：duration={duration:.2f}s "
            f"expected=[{min_duration:.2f}, {max_duration:.2f}]"
        )
    stage_log(f"路径 2 完成：{output_path} duration={duration:.2f}s")
    return 0


def _fixture_paths(fixture_dir: Path, script_file: Path) -> list[Path]:
    return [
        *(fixture_dir / filename for filename in BROLL_FILENAMES),
        fixture_dir / IMAGE_FILENAME,
        script_file,
    ]


def _resolve_path(path: Path) -> Path:
    return path.expanduser().resolve(strict=False)


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


def _has_running_job_kind(case_state: JsonMap, needles: tuple[str, ...]) -> bool:
    running_jobs = case_state.get("running_jobs")
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
    project_id: str,
    case_id: str,
    case_state: JsonMap,
) -> None:
    preview_id = _string_field(case_state, "preview_current_id")
    if preview_id is None:
        return
    client.mark_preview_viewed(project_id=project_id, case_id=case_id, preview_id=preview_id)
    stage_log(f"已标记预览已看：{preview_id}")


def main() -> int:
    try:
        return run(parse_args())
    except RunError as exc:
        stage_log(f"失败：{exc}")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
