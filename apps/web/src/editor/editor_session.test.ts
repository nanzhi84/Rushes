import { describe, expect, it, vi } from "vitest";
import type { TimelineJson } from "../api/client";
import {
  EditorSession,
  applyLocalTimelineOperation,
  compactEditorOperations
} from "./editor_session";

describe("EditorSession", () => {
  it("人工操作立即更新本地时间线，并合并连续的同类操作", () => {
    const session = new EditorSession(fixtureTimeline());

    session.apply({ kind: "adjust_gain", timeline_clip_id: "visual_a", gain_db: -3 });
    session.apply({ kind: "adjust_gain", timeline_clip_id: "visual_a", gain_db: -8 });

    const snapshot = session.snapshot();
    expect(findClip(snapshot.timeline, "visual_a").gain_db).toBe(-8);
    expect(snapshot.saveState).toBe("dirty");
    expect(snapshot.pendingCount).toBe(1);
    expect(session.beginSave()).toEqual([
      { kind: "adjust_gain", timeline_clip_id: "visual_a", gain_db: -8 }
    ]);
    expect(session.snapshot().saveState).toBe("saving");
  });

  it("保存进行中仍可继续编辑，并在服务端响应后重放未保存操作", () => {
    const initial = fixtureTimeline();
    const session = new EditorSession(initial);
    session.apply({ kind: "adjust_gain", timeline_clip_id: "visual_a", gain_db: -6 });
    expect(session.beginSave()).toHaveLength(1);

    session.apply({ kind: "set_track_state", track_id: "bgm", muted: true });
    expect(session.snapshot().saveState).toBe("dirty");

    const serverAfterFirstSave = applyLocalTimelineOperation(initial, {
      kind: "adjust_gain",
      timeline_clip_id: "visual_a",
      gain_db: -6
    });
    session.acceptSaved(serverAfterFirstSave);

    const rebased = session.snapshot();
    expect(findClip(rebased.timeline, "visual_a").gain_db).toBe(-6);
    expect(findTrack(rebased.timeline, "bgm").muted).toBe(true);
    expect(rebased.saveState).toBe("dirty");
    expect(rebased.pendingCount).toBe(1);
    expect(session.beginSave()).toEqual([
      { kind: "set_track_state", track_id: "bgm", muted: true }
    ]);
  });

  it("分割联动视频时同步分割原声，但保持 BGM 与 SFX 独立", () => {
    const result = applyLocalTimelineOperation(fixtureTimeline(), {
      kind: "split_clip",
      timeline_clip_id: "visual_a",
      split_frame: 15
    });

    expect(findTrack(result, "visual_base").clips?.map((clip) => clip.timeline_clip_id)).toEqual([
      "visual_a",
      "visual_a_split_15",
      "visual_b"
    ]);
    expect(findTrack(result, "original_audio").clips?.map((clip) => clip.timeline_clip_id)).toEqual([
      "original_a",
      "original_a_split_15",
      "original_b"
    ]);
    expect(findTrack(result, "bgm").clips).toHaveLength(1);
    expect(findTrack(result, "sfx").clips).toHaveLength(1);
    expect(result.duration_frames).toBe(90);
  });

  it("覆盖移动在本地立即裁开主视觉，结果与服务端覆盖语义一致", () => {
    const result = applyLocalTimelineOperation(fixtureTimeline(), {
      kind: "move_clip",
      timeline_clip_id: "overlay_a",
      target_track_id: "visual_base",
      target_frame: 30,
      mode: "overwrite"
    });

    expect(findTrack(result, "visual_overlay").clips).toEqual([]);
    expect(
      findTrack(result, "visual_base").clips?.map((clip) => ({
        id: clip.timeline_clip_id,
        start: clip.timeline_start_frame,
        end: clip.timeline_end_frame
      }))
    ).toEqual([
      { id: "visual_a", start: 0, end: 30 },
      { id: "overlay_a", start: 30, end: 50 },
      { id: "visual_b_after_50", start: 50, end: 90 }
    ]);
    expect(result.duration_frames).toBe(90);
  });

  it("只替换可安全折叠的操作，结构性操作保持原始顺序", () => {
    expect(
      compactEditorOperations([
        { kind: "split_clip", timeline_clip_id: "visual_a", split_frame: 10 },
        { kind: "move_clip", timeline_clip_id: "visual_b", target_frame: 0 },
        { kind: "move_clip", timeline_clip_id: "visual_b", target_frame: 30 },
        { kind: "delete_clip", timeline_clip_id: "visual_a" }
      ])
    ).toEqual([
      { kind: "split_clip", timeline_clip_id: "visual_a", split_frame: 10 },
      { kind: "move_clip", timeline_clip_id: "visual_b", target_frame: 30 },
      { kind: "delete_clip", timeline_clip_id: "visual_a" }
    ]);
  });

  it("保存失败会把飞行中操作放回队列，供下一次完整重试", () => {
    const session = new EditorSession(fixtureTimeline());
    session.apply({ kind: "set_track_state", track_id: "sfx", muted: true });
    session.beginSave();
    session.apply({ kind: "set_track_state", track_id: "bgm", gain_db: -12 });

    session.rejectSave(new Error("network down"));

    expect(session.snapshot()).toMatchObject({
      saveState: "error",
      pendingCount: 2,
      error: "network down"
    });
    // 用户下一次操作会重新进入 dirty 状态，随后正常触发批量保存。
    session.apply({ kind: "set_track_state", track_id: "bgm", gain_db: -10 });
    expect(session.beginSave()).toEqual([
      { kind: "set_track_state", track_id: "sfx", muted: true },
      { kind: "set_track_state", track_id: "bgm", gain_db: -10 }
    ]);
  });

  it("批量保存部分成功后只重试未执行后缀，并基于服务端最新时间线重放", () => {
    const initial = fixtureTimeline();
    const session = new EditorSession(initial);
    const split = { kind: "split_clip", timeline_clip_id: "visual_a", split_frame: 15 };
    const mute = { kind: "set_track_state", track_id: "bgm", muted: true };
    session.apply(split);
    session.apply(mute);
    expect(session.beginSave()).toEqual([split, mute]);
    session.apply({ kind: "adjust_gain", timeline_clip_id: "music_a", gain_db: -9 });

    const serverAfterSplit = applyLocalTimelineOperation(initial, split);
    session.rejectPartiallySaved(serverAfterSplit, 1, new Error("第二项保存失败"));

    const snapshot = session.snapshot();
    expect(snapshot).toMatchObject({
      saveState: "error",
      pendingCount: 2,
      error: "第二项保存失败"
    });
    expect(findTrack(snapshot.timeline, "visual_base").clips?.map((clip) => clip.timeline_clip_id))
      .toContain("visual_a_split_15");
    expect(findTrack(snapshot.timeline, "bgm").muted).toBe(true);
    expect(findClip(snapshot.timeline, "music_a").gain_db).toBe(-9);
    expect(session.beginSave()).toEqual([
      mute,
      { kind: "adjust_gain", timeline_clip_id: "music_a", gain_db: -9 }
    ]);
  });

  it("字幕样式在编辑、插入和保存失败后保持前后端同义", () => {
    const initial = fixtureTimeline();
    findTrack(initial, "subtitles").clips = [{
      timeline_clip_id: "subtitle_a",
      track_id: "subtitles",
      timeline_start_frame: 0,
      timeline_end_frame: 20,
      text: "旧字幕",
      subtitle_style: "default"
    }];
    const session = new EditorSession(initial);
    session.apply({
      kind: "edit_subtitle_text",
      timeline_clip_id: "subtitle_a",
      text: "新字幕",
      style: "large_center"
    });
    expect(findClip(session.snapshot().timeline, "subtitle_a")).toMatchObject({
      text: "新字幕",
      subtitle_style: "large_center"
    });
    const styleOnly = applyLocalTimelineOperation(initial, {
      kind: "edit_subtitle_text",
      timeline_clip_id: "subtitle_a",
      style: "bold_bottom"
    });
    expect(findClip(styleOnly, "subtitle_a")).toMatchObject({
      text: "旧字幕",
      subtitle_style: "bold_bottom"
    });
    session.beginSave();
    session.rejectSave(new Error("network down"));
    expect(findClip(session.snapshot().timeline, "subtitle_a").subtitle_style).toBe("large_center");
    expect(session.beginSave()).toEqual([{
      kind: "edit_subtitle_text",
      timeline_clip_id: "subtitle_a",
      text: "新字幕",
      style: "large_center"
    }]);

    const inserted = applyLocalTimelineOperation(initial, {
      kind: "insert_subtitle",
      timeline_clip_id: "subtitle_b",
      start_frame: 20,
      end_frame: 40,
      text: "新增字幕",
      style: "bold_bottom"
    });
    expect(findClip(inserted, "subtitle_b").subtitle_style).toBe("bold_bottom");
    const defaulted = applyLocalTimelineOperation(initial, {
      kind: "insert_subtitle",
      timeline_clip_id: "subtitle_c",
      start_frame: 20,
      end_frame: 40,
      text: "默认字幕"
    });
    expect(findClip(defaulted, "subtitle_c").subtitle_style).toBe("default");
  });

  it("字幕乐观操作拒绝未知或空白样式", () => {
    const initial = fixtureTimeline();
    findTrack(initial, "subtitles").clips = [{
      timeline_clip_id: "subtitle_a", track_id: "subtitles",
      timeline_start_frame: 0, timeline_end_frame: 20, text: "字幕"
    }];
    expect(() => applyLocalTimelineOperation(initial, {
      kind: "edit_subtitle_text", timeline_clip_id: "subtitle_a", text: "字幕", style: " "
    })).toThrow("字幕 style 必须是");
    expect(() => applyLocalTimelineOperation(initial, {
      kind: "insert_subtitle", start_frame: 20, end_frame: 40, text: "字幕", style: "karaoke"
    })).toThrow("字幕 style 必须是");
    expect(() => applyLocalTimelineOperation(initial, {
      kind: "edit_subtitle_text", timeline_clip_id: "subtitle_a"
    })).toThrow("至少需要提供 text 或 style");
    expect(() => applyLocalTimelineOperation(initial, {
      kind: "edit_subtitle_text", timeline_clip_id: "subtitle_a", text: " ", style: "default"
    })).toThrow("字幕文字不能为空");
  });

  it("音频淡入淡出按整数帧乐观更新，并折叠连续拖动", () => {
    const session = new EditorSession(fixtureTimeline());
    session.apply({
      kind: "set_clip_fades",
      timeline_clip_id: "music_a",
      fade_in_frames: 3,
      fade_out_frames: 9
    });
    session.apply({
      kind: "set_clip_fades",
      timeline_clip_id: "music_a",
      fade_in_frames: 6,
      fade_out_frames: 12
    });

    expect(findClip(session.snapshot().timeline, "music_a")).toMatchObject({
      fade_in_frames: 6,
      fade_out_frames: 12
    });
    expect(session.beginSave()).toEqual([{
      kind: "set_clip_fades",
      timeline_clip_id: "music_a",
      fade_in_frames: 6,
      fade_out_frames: 12
    }]);
  });

  it("联动视频淡入淡出同时更新画面与代理预览原声", () => {
    const result = applyLocalTimelineOperation(fixtureTimeline(), {
      kind: "set_clip_fades",
      timeline_clip_id: "visual_a",
      fade_in_frames: 6,
      fade_out_frames: 12
    });

    expect(findClip(result, "visual_a")).toMatchObject({
      fade_in_frames: 6,
      fade_out_frames: 12
    });
    expect(findClip(result, "original_a")).toMatchObject({
      fade_in_frames: 6,
      fade_out_frames: 12
    });
  });

  it("服务端重复返回相同时间线时不通知订阅者", () => {
    const initial = fixtureTimeline();
    const session = new EditorSession(initial);
    const listener = vi.fn();
    session.subscribe(listener);

    session.replaceFromServer(structuredClone(initial));

    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("服务端时间线内容真正变化时只通知一次", () => {
    const initial = fixtureTimeline();
    const session = new EditorSession(initial);
    const listener = vi.fn();
    session.subscribe(listener);
    const changed = structuredClone(initial);
    changed.duration_frames = 91;

    session.replaceFromServer(changed);

    expect(listener).toHaveBeenCalledTimes(2);
    expect(session.snapshot().timeline.duration_frames).toBe(91);
  });
});

function fixtureTimeline(): TimelineJson {
  return {
    fps: 30,
    duration_frames: 90,
    tracks: [
      {
        track_id: "visual_base",
        track_type: "video",
        clips: [
          {
            timeline_clip_id: "visual_a",
            track_id: "visual_base",
            asset_id: "asset_a",
            asset_kind: "video",
            timeline_start_frame: 0,
            timeline_end_frame: 30,
            source_start_frame: 0,
            source_end_frame: 30,
            playback_rate: 1,
            linked: true,
            parent_block_id: "block_a"
          },
          {
            timeline_clip_id: "visual_b",
            track_id: "visual_base",
            asset_id: "asset_b",
            asset_kind: "video",
            timeline_start_frame: 30,
            timeline_end_frame: 90,
            source_start_frame: 0,
            source_end_frame: 60,
            playback_rate: 1,
            linked: true,
            parent_block_id: "block_b"
          }
        ]
      },
      {
        track_id: "visual_overlay",
        track_type: "visual_overlay",
        clips: [
          {
            timeline_clip_id: "overlay_a",
            track_id: "visual_overlay",
            asset_id: "asset_overlay",
            asset_kind: "video",
            timeline_start_frame: 10,
            timeline_end_frame: 30,
            source_start_frame: 0,
            source_end_frame: 20,
            playback_rate: 1
          }
        ]
      },
      {
        track_id: "original_audio",
        track_type: "audio",
        clips: [
          {
            timeline_clip_id: "original_a",
            track_id: "original_audio",
            asset_id: "asset_a",
            asset_kind: "video",
            timeline_start_frame: 0,
            timeline_end_frame: 30,
            source_start_frame: 0,
            source_end_frame: 30,
            playback_rate: 1,
            linked: true,
            parent_block_id: "block_a"
          },
          {
            timeline_clip_id: "original_b",
            track_id: "original_audio",
            asset_id: "asset_b",
            asset_kind: "video",
            timeline_start_frame: 30,
            timeline_end_frame: 90,
            source_start_frame: 0,
            source_end_frame: 60,
            playback_rate: 1,
            linked: true,
            parent_block_id: "block_b"
          }
        ]
      },
      {
        track_id: "bgm",
        track_type: "audio",
        clips: [
          {
            timeline_clip_id: "music_a",
            track_id: "bgm",
            asset_id: "music",
            asset_kind: "audio",
            timeline_start_frame: 0,
            timeline_end_frame: 90,
            source_start_frame: 0,
            source_end_frame: 90,
            gain_db: -8
          }
        ]
      },
      {
        track_id: "sfx",
        track_type: "audio",
        clips: [
          {
            timeline_clip_id: "sfx_a",
            track_id: "sfx",
            asset_id: "sfx",
            asset_kind: "audio",
            timeline_start_frame: 20,
            timeline_end_frame: 30,
            source_start_frame: 0,
            source_end_frame: 10,
            gain_db: -3
          }
        ]
      },
      { track_id: "subtitles", track_type: "subtitle", clips: [] }
    ]
  };
}

function findTrack(timeline: TimelineJson, trackId: string) {
  const track = timeline.tracks.find((candidate) => candidate.track_id === trackId);
  if (!track) {
    throw new Error(`missing track ${trackId}`);
  }
  return track;
}

function findClip(timeline: TimelineJson, clipId: string) {
  const clip = timeline.tracks
    .flatMap((track) => track.clips ?? [])
    .find((candidate) => candidate.timeline_clip_id === clipId);
  if (!clip) {
    throw new Error(`missing clip ${clipId}`);
  }
  return clip;
}
