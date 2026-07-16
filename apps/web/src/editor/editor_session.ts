import type { TimelineClipJson, TimelineJson, TimelineTrackJson } from "../api/client";

export type TimelineOperation = Record<string, unknown> & { kind: string };
export type EditorSaveState = "saved" | "dirty" | "saving" | "error";

export type EditorSessionSnapshot = {
  timeline: TimelineJson;
  saveState: EditorSaveState;
  pendingCount: number;
  error: string | null;
};

type Listener = (snapshot: EditorSessionSnapshot) => void;

// EditorSession 是浏览器内唯一的人工编辑会话。指针移动只作用于组件预览；
// pointerup/按钮命令在这里乐观更新当前时间线，并在短暂空闲后批量提交。
export class EditorSession {
  private timeline: TimelineJson;
  private pending: TimelineOperation[] = [];
  private inFlight: TimelineOperation[] = [];
  private state: EditorSaveState = "saved";
  private error: string | null = null;
  private listeners = new Set<Listener>();

  constructor(initialTimeline: TimelineJson) {
    this.timeline = cloneTimeline(initialTimeline);
  }

  snapshot(): EditorSessionSnapshot {
    return {
      timeline: this.timeline,
      saveState: this.state,
      pendingCount: this.pending.length + this.inFlight.length,
      error: this.error
    };
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    listener(this.snapshot());
    return () => this.listeners.delete(listener);
  }

  apply(operation: TimelineOperation): void {
    this.timeline = applyLocalTimelineOperation(this.timeline, operation);
    this.pending = compactEditorOperations([...this.pending, cloneOperation(operation)]);
    this.state = "dirty";
    this.error = null;
    this.emit();
  }

  beginSave(): TimelineOperation[] {
    if (this.inFlight.length > 0 || this.pending.length === 0) {
      return [];
    }
    this.inFlight = this.pending;
    this.pending = [];
    this.state = "saving";
    this.emit();
    return this.inFlight.map(cloneOperation);
  }

  acceptSaved(serverTimeline: TimelineJson): void {
    this.inFlight = [];
    let rebased = cloneTimeline(serverTimeline);
    for (const operation of this.pending) {
      rebased = applyLocalTimelineOperation(rebased, operation);
    }
    this.timeline = rebased;
    this.state = this.pending.length > 0 ? "dirty" : "saved";
    this.error = null;
    this.emit();
  }

  rejectSave(error: unknown): void {
    this.pending = compactEditorOperations([...this.inFlight, ...this.pending]);
    this.inFlight = [];
    this.state = "error";
    this.error = error instanceof Error ? error.message : "时间线保存失败";
    this.emit();
  }

  replaceFromServer(serverTimeline: TimelineJson): void {
    if (this.pending.length > 0 || this.inFlight.length > 0) {
      return;
    }
    // React Query/SSE 可能多次返回内容完全相同、引用不同的时间线对象。
    // 相同内容不应触发订阅者更新，否则会让预览引擎在播放中重复 sync/seek。
    if (timelineContentEquals(this.timeline, serverTimeline)) {
      return;
    }
    this.timeline = cloneTimeline(serverTimeline);
    this.state = "saved";
    this.error = null;
    this.emit();
  }

  private emit(): void {
    const snapshot = this.snapshot();
    for (const listener of this.listeners) {
      listener(snapshot);
    }
  }
}

function timelineContentEquals(left: TimelineJson, right: TimelineJson): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

export function compactEditorOperations(operations: TimelineOperation[]): TimelineOperation[] {
  const result: TimelineOperation[] = [];
  const replaceable = new Map<string, number>();
  for (const operation of operations) {
    const key = operationCoalesceKey(operation);
    if (key) {
      const existing = replaceable.get(key);
      if (existing !== undefined) {
        result[existing] = cloneOperation(operation);
        continue;
      }
      replaceable.set(key, result.length);
    }
    result.push(cloneOperation(operation));
  }
  return result;
}

export function applyLocalTimelineOperation(
  current: TimelineJson,
  operation: TimelineOperation
): TimelineJson {
  const timeline = cloneTimeline(current);
  switch (operation.kind) {
    case "split_clip":
      splitClip(timeline, operation);
      break;
    case "move_clip":
    case "reorder_clip":
      moveClip(timeline, operation);
      break;
    case "trim_clip_edge":
      trimClipEdge(timeline, operation);
      break;
    case "delete_clip":
      deleteClip(timeline, operation);
      break;
    case "set_track_state":
      setTrackState(timeline, operation);
      break;
    case "set_clip_linked":
      setClipLinked(timeline, operation);
      break;
    case "adjust_gain":
      updateClip(timeline, operation, (clip) => {
        const gain = requiredNumber(operation.gain_db, "gain_db");
        if (gain < -60 || gain > 12) {
          throw new Error("gain_db 必须在 [-60,12] 范围内");
        }
        clip.gain_db = gain;
      });
      break;
    case "set_clip_fades":
      setClipFades(timeline, operation);
      break;
    case "edit_subtitle_text":
      updateClip(timeline, operation, (clip, track) => {
        if (track.track_id !== "subtitles") {
          throw new Error("只能编辑字幕轨文字");
        }
        const hasText = Object.prototype.hasOwnProperty.call(operation, "text");
        const hasStyle = Object.prototype.hasOwnProperty.call(operation, "style");
        if (!hasText && !hasStyle) {
          throw new Error("字幕编辑至少需要提供 text 或 style");
        }
        if (hasText) {
          const text = String(operation.text ?? "").trim();
          if (!text) {
            throw new Error("字幕文字不能为空");
          }
          clip.text = text;
        }
        if (Object.prototype.hasOwnProperty.call(operation, "style")) {
          clip.subtitle_style = requiredSubtitleStyle(operation.style);
        }
      });
      break;
    case "insert_subtitle":
      insertSubtitle(timeline, operation);
      break;
    case "set_playback_rate":
      updateClip(timeline, operation, (clip, track) => {
        const rate = requiredNumber(operation.playback_rate, "playback_rate");
        if (rate <= 0 || rate > 8) {
          throw new Error("playback_rate 必须在 (0,8]");
        }
        const oldDuration = clipDuration(clip);
        const sourceDuration = numberValue(clip.source_end_frame) - numberValue(clip.source_start_frame);
        const newDuration = Math.max(1, Math.round(sourceDuration / rate));
        clip.playback_rate = rate;
        clip.timeline_end_frame = numberValue(clip.timeline_start_frame) + newDuration;
        if (track.track_id === "visual_base") {
          shiftAfter(timeline, numberValue(clip.timeline_start_frame) + oldDuration, newDuration - oldDuration);
        }
      });
      break;
    case "trim_clip":
      trimClip(timeline, operation);
      break;
    case "remove_track_clips": {
      const track = findTrack(timeline, String(operation.track_id ?? ""));
      if (track.track_id === "visual_base" || track.locked) {
        throw new Error("不能清空锁定轨道或主视觉轨");
      }
      track.clips = [];
      break;
    }
    case "insert_clip":
      insertClip(timeline, operation);
      break;
    default:
      throw new Error(`暂不支持本地时间线操作：${operation.kind}`);
  }
  clampTimelineFades(timeline);
  return timeline;
}

function splitClip(timeline: TimelineJson, operation: TimelineOperation): void {
  const selected = locateEditableClip(timeline, targetClipId(operation));
  const splitFrame = requiredInteger(operation.split_frame, "split_frame");
  const group = selected.clip.linked && selected.clip.parent_block_id
    ? String(selected.clip.parent_block_id)
    : "";
  const rightGroup = group ? `${group}_split_${splitFrame}` : "";
  const selectedId = String(selected.clip.timeline_clip_id);
  for (const track of timeline.tracks) {
    const next: TimelineClipJson[] = [];
    for (const clip of track.clips ?? []) {
      const shouldSplit =
        clip.timeline_clip_id === selectedId ||
        (group !== "" && clip.linked === true && clip.parent_block_id === group);
      if (!shouldSplit) {
        next.push(clip);
        continue;
      }
      if (track.locked) {
        throw new Error(`轨道 ${track.track_id} 已锁定`);
      }
      const start = numberValue(clip.timeline_start_frame);
      const end = numberValue(clip.timeline_end_frame);
      if (!clip.asset_id || splitFrame <= start || splitFrame >= end) {
        throw new Error("分割点必须位于片段内部");
      }
      const rate = Math.max(0.01, numberValue(clip.playback_rate, 1));
      const sourceSplit = numberValue(clip.source_start_frame) + Math.round((splitFrame - start) * rate);
      if (
        sourceSplit <= numberValue(clip.source_start_frame) ||
        sourceSplit >= numberValue(clip.source_end_frame)
      ) {
        throw new Error("分割后的素材源范围无效");
      }
      const left = {
        ...clip,
        timeline_end_frame: splitFrame,
        source_end_frame: sourceSplit,
        fade_out_frames: 0
      };
      const right = {
        ...clip,
        timeline_clip_id:
          clip.timeline_clip_id === selectedId && typeof operation.new_timeline_clip_id === "string"
            ? operation.new_timeline_clip_id
            : `${clip.timeline_clip_id}_split_${splitFrame}`,
        timeline_start_frame: splitFrame,
        source_start_frame: sourceSplit,
        fade_in_frames: 0
      };
      if (clip.linked) {
        left.parent_block_id = group;
        right.parent_block_id = rightGroup;
      }
      next.push(left, right);
    }
    track.clips = next;
  }
}

function moveClip(timeline: TimelineJson, operation: TimelineOperation): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  const targetFrame = requiredInteger(operation.target_frame, "target_frame");
  const mode = String(operation.mode ?? "insert");
  if (mode !== "insert" && mode !== "overwrite") {
    throw new Error("移动模式必须是 insert 或 overwrite");
  }
  const targetTrackId = String(operation.target_track_id ?? located.track.track_id);
  const targetTrack = findTrack(timeline, targetTrackId);
  assertTrackUnlocked(targetTrack);
  if (!tracksCompatible(located.track, targetTrack, located.clip)) {
    throw new Error(`片段不能从 ${located.track.track_id} 移到 ${targetTrackId}`);
  }
  if (located.clip.linked && located.clip.parent_block_id) {
    if (targetTrackId !== located.track.track_id) {
      throw new Error("跨轨移动前请先取消片段联动");
    }
    const primary = linkedMembers(timeline, located.clip).find(
      (member) => member.track.track_id === "visual_base"
    );
    if (primary) {
      reorderPrimary(timeline, primary.clip, targetFrame);
      return;
    }
  }
  if (located.track.track_id === "visual_base" && targetTrackId === "visual_base") {
    reorderPrimary(timeline, located.clip, targetFrame);
    return;
  }
  const duration = clipDuration(located.clip);
  if (duration <= 0) {
    throw new Error("移动片段时长无效");
  }
  const sourceTrackId = located.track.track_id;
  const sourceStart = numberValue(located.clip.timeline_start_frame);
  const sourceEnd = numberValue(located.clip.timeline_end_frame);
  const moving = { ...located.clip };
  located.track.clips = (located.track.clips ?? []).filter(
    (clip) => clip.timeline_clip_id !== located.clip.timeline_clip_id
  );

  let destinationFrame = targetFrame;
  if (sourceTrackId === "visual_base") {
    if ((located.track.clips ?? []).length === 0) {
      throw new Error("主视觉轨至少保留一个片段");
    }
    deleteRange(timeline, sourceStart, sourceEnd);
    if (destinationFrame > sourceEnd) {
      destinationFrame -= duration;
    }
  }

  if (targetTrackId === "visual_base") {
    moving.track_id = targetTrackId;
    moving.linked = false;
    moving.parent_block_id = "";
    if (mode === "insert") {
      insertIntoPrimary(timeline, moving, destinationFrame);
    } else {
      overwritePrimary(timeline, moving, destinationFrame);
    }
    return;
  }

  const destination = findTrack(timeline, targetTrackId);
  if (duration > timeline.duration_frames) {
    throw new Error("片段长于时间线，不能放入目标轨");
  }
  const start = clamp(destinationFrame, 0, Math.max(0, timeline.duration_frames - duration));
  if (mode === "insert") {
    shiftTrackForInsert(destination, start, duration, timeline.duration_frames);
  } else {
    eraseTrackRange(destination, start, start + duration);
  }
  moving.track_id = targetTrackId;
  moving.timeline_start_frame = start;
  moving.timeline_end_frame = start + duration;
  if (targetTrackId !== sourceTrackId) {
    moving.linked = false;
    moving.parent_block_id = "";
  }
  destination.clips = [...(destination.clips ?? []), moving].sort(byStartFrame);
}

function reorderPrimary(timeline: TimelineJson, moving: TimelineClipJson, targetFrame: number): void {
  const primary = findTrack(timeline, "visual_base");
  assertTrackUnlocked(primary);
  if (targetFrame < 0 || targetFrame > timeline.duration_frames) {
    throw new Error("移动位置超出时间线");
  }
  for (const member of linkedMembers(timeline, moving)) {
    if (member.track.track_id !== "visual_base") {
      assertTrackUnlocked(member.track);
    }
  }
  const clips = [...(primary.clips ?? [])].sort(byStartFrame);
  const without = clips.filter((clip) => clip.timeline_clip_id !== moving.timeline_clip_id);
  let insertAt = without.length;
  for (let index = 0; index < without.length; index += 1) {
    const clip = without[index]!;
    const midpoint = numberValue(clip.timeline_start_frame) + clipDuration(clip) / 2;
    if (targetFrame < midpoint) {
      insertAt = index;
      break;
    }
  }
  without.splice(insertAt, 0, moving);
  let cursor = 0;
  for (const clip of without) {
    const duration = clipDuration(clip);
    clip.timeline_start_frame = cursor;
    clip.timeline_end_frame = cursor + duration;
    cursor += duration;
    syncLinkedTiming(timeline, clip);
  }
  primary.clips = without;
  sortTracks(timeline);
}

function insertIntoPrimary(
  timeline: TimelineJson,
  moving: TimelineClipJson,
  requestedFrame: number
): void {
  const primary = findTrack(timeline, "visual_base");
  assertTrackUnlocked(primary);
  const targetFrame = clamp(requestedFrame, 0, timeline.duration_frames);
  const duration = clipDuration(moving);
  ensureRippleUnlocked(timeline, targetFrame, "visual_base");
  splitPrimaryAt(timeline, targetFrame);
  for (const member of allLocatedClips(timeline)) {
    if (numberValue(member.clip.timeline_start_frame) >= targetFrame) {
      member.clip.timeline_start_frame = numberValue(member.clip.timeline_start_frame) + duration;
      member.clip.timeline_end_frame = numberValue(member.clip.timeline_end_frame) + duration;
    }
  }
  timeline.duration_frames += duration;
  moving.timeline_start_frame = targetFrame;
  moving.timeline_end_frame = targetFrame + duration;
  primary.clips = [...(primary.clips ?? []), moving].sort(byStartFrame);
}

function overwritePrimary(
  timeline: TimelineJson,
  moving: TimelineClipJson,
  requestedFrame: number
): void {
  const primary = findTrack(timeline, "visual_base");
  assertTrackUnlocked(primary);
  const duration = clipDuration(moving);
  if (duration > timeline.duration_frames) {
    throw new Error("覆盖片段长于当前时间线");
  }
  const targetFrame = clamp(requestedFrame, 0, timeline.duration_frames - duration);
  eraseTrackRange(primary, targetFrame, targetFrame + duration);
  moving.timeline_start_frame = targetFrame;
  moving.timeline_end_frame = targetFrame + duration;
  primary.clips = [...(primary.clips ?? []), moving].sort(byStartFrame);
}

function splitPrimaryAt(timeline: TimelineJson, frame: number): void {
  if (frame <= 0 || frame >= timeline.duration_frames) {
    return;
  }
  const primary = findTrack(timeline, "visual_base");
  const clip = (primary.clips ?? []).find(
    (candidate) =>
      frame > numberValue(candidate.timeline_start_frame) &&
      frame < numberValue(candidate.timeline_end_frame)
  );
  if (clip?.timeline_clip_id) {
    splitClip(timeline, {
      kind: "split_clip",
      timeline_clip_id: clip.timeline_clip_id,
      split_frame: frame
    });
  }
}

function eraseTrackRange(track: TimelineTrackJson, start: number, end: number): void {
  const kept: TimelineClipJson[] = [];
  for (const original of track.clips ?? []) {
    const clip = { ...original };
    const clipStart = numberValue(clip.timeline_start_frame);
    const clipEnd = numberValue(clip.timeline_end_frame);
    if (clipEnd <= start || clipStart >= end) {
      kept.push(clip);
      continue;
    }
    const rate = effectiveRate(clip);
    if (clipStart < start) {
      const left = { ...clip };
      const removed = clipEnd - start;
      left.timeline_end_frame = start;
      if (left.asset_id) {
        left.source_end_frame = numberValue(left.source_end_frame) - Math.round(removed * rate);
      }
      kept.push(left);
    }
    if (clipEnd > end) {
      const right = { ...clip };
      const removed = end - clipStart;
      right.timeline_clip_id = `${String(clip.timeline_clip_id)}_after_${end}`;
      right.timeline_start_frame = end;
      if (right.asset_id) {
        right.source_start_frame = numberValue(right.source_start_frame) + Math.round(removed * rate);
      }
      kept.push(right);
    }
  }
  track.clips = kept.sort(byStartFrame);
}

function shiftTrackForInsert(
  track: TimelineTrackJson,
  frame: number,
  duration: number,
  timelineDuration: number
): void {
  const kept: TimelineClipJson[] = [];
  for (const original of track.clips ?? []) {
    const clip = { ...original };
    if (numberValue(clip.timeline_start_frame) >= frame) {
      clip.timeline_start_frame = numberValue(clip.timeline_start_frame) + duration;
      clip.timeline_end_frame = numberValue(clip.timeline_end_frame) + duration;
    }
    if (numberValue(clip.timeline_start_frame) >= timelineDuration) {
      continue;
    }
    if (numberValue(clip.timeline_end_frame) > timelineDuration) {
      const overflow = numberValue(clip.timeline_end_frame) - timelineDuration;
      clip.timeline_end_frame = timelineDuration;
      if (clip.asset_id) {
        clip.source_end_frame =
          numberValue(clip.source_end_frame) - Math.round(overflow * effectiveRate(clip));
      }
    }
    kept.push(clip);
  }
  track.clips = kept.sort(byStartFrame);
}

function trimClipEdge(timeline: TimelineJson, operation: TimelineOperation): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  const frame = requiredInteger(operation.timeline_frame, "timeline_frame");
  const edge = operation.edge;
  const start = numberValue(located.clip.timeline_start_frame);
  const end = numberValue(located.clip.timeline_end_frame);
  if (edge !== "start" && edge !== "end") {
    throw new Error("裁剪边必须是 start 或 end");
  }
  if (frame <= start || frame >= end) {
    throw new Error("裁剪点必须位于片段内部");
  }
  const members = linkedMembers(timeline, located.clip);
  for (const member of members) {
    assertTrackUnlocked(member.track);
  }
  if (members.some((member) => member.track.track_id === "visual_base")) {
    deleteRange(timeline, edge === "start" ? start : frame, edge === "start" ? frame : end);
    return;
  }
  for (const member of members) {
    const rate = Math.max(0.01, numberValue(member.clip.playback_rate, 1));
    if (edge === "start") {
      const delta = frame - numberValue(member.clip.timeline_start_frame);
      member.clip.timeline_start_frame = frame;
      member.clip.source_start_frame = numberValue(member.clip.source_start_frame) + Math.round(delta * rate);
    } else {
      const delta = numberValue(member.clip.timeline_end_frame) - frame;
      member.clip.timeline_end_frame = frame;
      member.clip.source_end_frame = numberValue(member.clip.source_end_frame) - Math.round(delta * rate);
    }
  }
}

function deleteClip(timeline: TimelineJson, operation: TimelineOperation): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  const members = linkedMembers(timeline, located.clip);
  for (const member of members) {
    assertTrackUnlocked(member.track);
    if (member.track.track_id === "visual_base" && (member.track.clips ?? []).length <= 1) {
      throw new Error("主视觉轨至少保留一个片段");
    }
  }
  if (members.some((member) => member.track.track_id === "visual_base")) {
    deleteRange(
      timeline,
      numberValue(located.clip.timeline_start_frame),
      numberValue(located.clip.timeline_end_frame)
    );
    return;
  }
  const group = located.clip.linked ? String(located.clip.parent_block_id ?? "") : "";
  for (const track of timeline.tracks) {
    track.clips = (track.clips ?? []).filter(
      (clip) =>
        clip.timeline_clip_id !== located.clip.timeline_clip_id &&
        !(group && clip.linked === true && clip.parent_block_id === group)
    );
  }
}

function deleteRange(timeline: TimelineJson, start: number, end: number): void {
  const delta = end - start;
  if (start < 0 || end > timeline.duration_frames || delta <= 0 || delta >= timeline.duration_frames) {
    throw new Error("删除范围无效");
  }
  ensureRippleUnlocked(timeline, start, "");
  for (const track of timeline.tracks) {
    const kept: TimelineClipJson[] = [];
    for (const original of track.clips ?? []) {
      const clip = { ...original };
      const clipStart = numberValue(clip.timeline_start_frame);
      const clipEnd = numberValue(clip.timeline_end_frame);
      if (clipEnd <= start) {
        kept.push(clip);
      } else if (clipStart >= end) {
        clip.timeline_start_frame = clipStart - delta;
        clip.timeline_end_frame = clipEnd - delta;
        kept.push(clip);
      } else if (clipStart < start && clipEnd > end) {
        clip.timeline_end_frame = clipEnd - delta;
        clip.source_end_frame = numberValue(clip.source_end_frame) - Math.round(delta * effectiveRate(clip));
        kept.push(clip);
      } else if (clipStart < start) {
        clip.timeline_end_frame = start;
        clip.source_end_frame =
          numberValue(clip.source_start_frame) + Math.round((start - clipStart) * effectiveRate(clip));
        kept.push(clip);
      } else if (clipEnd > end) {
        const removed = end - clipStart;
        clip.timeline_start_frame = start;
        clip.timeline_end_frame = clipEnd - delta;
        clip.source_start_frame = numberValue(clip.source_start_frame) + Math.round(removed * effectiveRate(clip));
        kept.push(clip);
      }
    }
    track.clips = kept.sort(byStartFrame);
  }
  timeline.duration_frames -= delta;
}

function setTrackState(timeline: TimelineJson, operation: TimelineOperation): void {
  const track = findTrack(timeline, String(operation.track_id ?? ""));
  let changed = false;
  for (const key of ["muted", "solo", "locked"] as const) {
    if (typeof operation[key] === "boolean") {
      if (key === "muted" && track.track_id === "visual_base" && operation[key] === true) {
        throw new Error("主视觉轨不能静音");
      }
      track[key] = operation[key];
      changed = true;
    }
  }
  if (typeof operation.gain_db === "number") {
    if (trackFamily(track) !== "audio" || operation.gain_db < -60 || operation.gain_db > 12) {
      throw new Error("只有音频轨支持 [-60,12] dB 的轨道音量");
    }
    track.gain_db = operation.gain_db;
    changed = true;
  }
  if (!changed) {
    throw new Error("轨道状态操作没有可更新字段");
  }
}

function setClipLinked(timeline: TimelineJson, operation: TimelineOperation): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  const linked = operation.linked === true;
  if (!linked) {
    const group = String(located.clip.parent_block_id ?? "");
    located.clip.linked = false;
    located.clip.parent_block_id = "";
    const remaining = allLocatedClips(timeline).filter(
      (member) => member.clip.linked === true && member.clip.parent_block_id === group
    );
    if (remaining.length === 1) {
      remaining[0]!.clip.linked = false;
      remaining[0]!.clip.parent_block_id = "";
    }
    return;
  }
  const wantedTrack = located.track.track_id === "visual_base" ? "original_audio" : "visual_base";
  const partner = allLocatedClips(timeline).find(
    (member) =>
      member.track.track_id === wantedTrack &&
      member.clip.asset_id === located.clip.asset_id &&
      member.clip.timeline_start_frame === located.clip.timeline_start_frame &&
      member.clip.timeline_end_frame === located.clip.timeline_end_frame
  );
  if (!partner) {
    throw new Error("没有可联动的同源音画片段");
  }
  assertTrackUnlocked(partner.track);
  const group =
    String(located.clip.parent_block_id ?? partner.clip.parent_block_id ?? "") ||
    `link_${located.clip.timeline_clip_id}`;
  located.clip.linked = true;
  located.clip.parent_block_id = group;
  partner.clip.linked = true;
  partner.clip.parent_block_id = group;
}

function trimClip(timeline: TimelineJson, operation: TimelineOperation): void {
  updateClip(timeline, operation, (clip, track) => {
    const sourceStart = requiredInteger(operation.source_start_frame, "source_start_frame");
    const sourceEnd = requiredInteger(operation.source_end_frame, "source_end_frame");
    if (sourceStart < 0 || sourceEnd <= sourceStart) {
      throw new Error("素材裁剪范围无效");
    }
    const oldDuration = clipDuration(clip);
    const rate = effectiveRate(clip);
    const newDuration = Math.max(1, Math.round((sourceEnd - sourceStart) / rate));
    clip.source_start_frame = sourceStart;
    clip.source_end_frame = sourceEnd;
    clip.timeline_end_frame = numberValue(clip.timeline_start_frame) + newDuration;
    if (track.track_id === "visual_base") {
      shiftAfter(timeline, numberValue(clip.timeline_start_frame) + oldDuration, newDuration - oldDuration);
    }
  });
}

function insertSubtitle(timeline: TimelineJson, operation: TimelineOperation): void {
  const track = findTrack(timeline, "subtitles");
  assertTrackUnlocked(track);
  const start = requiredInteger(operation.start_frame, "start_frame");
  const end = requiredInteger(operation.end_frame, "end_frame");
  const text = String(operation.text ?? "").trim();
  if (start < 0 || end <= start || end > timeline.duration_frames || !text) {
    throw new Error("字幕时间范围或文字无效");
  }
  const subtitleStyle = optionalSubtitleStyle(operation.style);
  track.clips = [
    ...(track.clips ?? []),
    {
      timeline_clip_id: String(operation.timeline_clip_id ?? `subtitle_${Date.now()}`),
      track_id: "subtitles",
      timeline_start_frame: start,
      timeline_end_frame: end,
      text,
      subtitle_style: subtitleStyle
    }
  ].sort(byStartFrame);
}

type SubtitleStyle = NonNullable<TimelineClipJson["subtitle_style"]>;

const subtitleStyles = new Set<SubtitleStyle>([
  "default", "large_center", "top_bar", "minimal", "bold_bottom"
]);

function requiredSubtitleStyle(value: unknown): SubtitleStyle {
  const style = String(value ?? "").trim();
  if (!subtitleStyles.has(style as SubtitleStyle)) {
    throw new Error("字幕 style 必须是 default、large_center、top_bar、minimal 或 bold_bottom");
  }
  return style as SubtitleStyle;
}

function optionalSubtitleStyle(value: unknown): SubtitleStyle {
  const style = String(value ?? "").trim();
  return style === "" ? "default" : requiredSubtitleStyle(style);
}

function insertClip(timeline: TimelineJson, operation: TimelineOperation): void {
  const track = findTrack(timeline, String(operation.track_id ?? "visual_base"));
  const sourceStart = requiredInteger(operation.source_start_frame, "source_start_frame");
  const sourceEnd = requiredInteger(operation.source_end_frame, "source_end_frame");
  const start = track.track_id === "visual_base"
    ? timeline.duration_frames
    : requiredInteger(operation.timeline_start_frame ?? 0, "timeline_start_frame");
  const clip: TimelineClipJson = {
    timeline_clip_id: String(operation.timeline_clip_id ?? `clip_local_${Date.now()}`),
    track_id: track.track_id,
    timeline_start_frame: start,
    timeline_end_frame: start + sourceEnd - sourceStart,
    source_start_frame: sourceStart,
    source_end_frame: sourceEnd,
    asset_id: String(operation.asset_id ?? ""),
    asset_kind: String(operation.asset_kind ?? ""),
    role: String(operation.role ?? "b_roll"),
    playback_rate: 1
  };
  track.clips = [...(track.clips ?? []), clip].sort(byStartFrame);
  if (track.track_id === "visual_base") {
    timeline.duration_frames += sourceEnd - sourceStart;
  }
}

function updateClip(
  timeline: TimelineJson,
  operation: TimelineOperation,
  update: (clip: TimelineClipJson, track: TimelineTrackJson) => void
): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  update(located.clip, located.track);
}

function setClipFades(timeline: TimelineJson, operation: TimelineOperation): void {
  const located = locateEditableClip(timeline, targetClipId(operation));
  if (trackFamily(located.track) !== "audio" && located.clip.asset_kind !== "video") {
    throw new Error("只有音频片段或带声音的视频片段支持淡入淡出");
  }
  const fadeIn = requiredInteger(operation.fade_in_frames, "fade_in_frames");
  const fadeOut = requiredInteger(operation.fade_out_frames, "fade_out_frames");
  const members = trackFamily(located.track) === "visual" && located.clip.asset_kind === "video"
    ? [located, ...linkedMembers(timeline, located.clip).filter(
      (member) => member.track.track_id === "original_audio"
    )]
    : [located];
  for (const member of members) {
    assertTrackUnlocked(member.track);
    if (fadeIn < 0 || fadeOut < 0 || fadeIn + fadeOut > clipDuration(member.clip)) {
      throw new Error("淡入与淡出必须为非负整数帧，且总和不能超过片段时长");
    }
  }
  for (const member of members) {
    member.clip.fade_in_frames = fadeIn;
    member.clip.fade_out_frames = fadeOut;
  }
}

function linkedMembers(timeline: TimelineJson, clip: TimelineClipJson): LocatedClip[] {
  const group = clip.linked === true ? String(clip.parent_block_id ?? "") : "";
  if (!group) {
    return [locateClip(timeline, String(clip.timeline_clip_id))];
  }
  return allLocatedClips(timeline).filter(
    (member) => member.clip.linked === true && member.clip.parent_block_id === group
  );
}

function syncLinkedTiming(timeline: TimelineJson, primary: TimelineClipJson): void {
  if (!primary.linked || !primary.parent_block_id) {
    return;
  }
  for (const member of allLocatedClips(timeline)) {
    if (
      member.track.track_id !== "visual_base" &&
      member.clip.linked === true &&
      member.clip.parent_block_id === primary.parent_block_id
    ) {
      member.clip.timeline_start_frame = primary.timeline_start_frame;
      member.clip.timeline_end_frame = primary.timeline_end_frame;
    }
  }
}

function shiftAfter(timeline: TimelineJson, boundary: number, delta: number): void {
  if (delta === 0) {
    return;
  }
  for (const member of allLocatedClips(timeline)) {
    if (numberValue(member.clip.timeline_start_frame) >= boundary) {
      member.clip.timeline_start_frame = numberValue(member.clip.timeline_start_frame) + delta;
      member.clip.timeline_end_frame = numberValue(member.clip.timeline_end_frame) + delta;
    }
  }
  timeline.duration_frames += delta;
}

type LocatedClip = { track: TimelineTrackJson; clip: TimelineClipJson };

function allLocatedClips(timeline: TimelineJson): LocatedClip[] {
  return timeline.tracks.flatMap((track) =>
    (track.clips ?? []).map((clip) => ({ track, clip }))
  );
}

function locateClip(timeline: TimelineJson, clipId: string): LocatedClip {
  for (const track of timeline.tracks) {
    const clip = (track.clips ?? []).find((candidate) => candidate.timeline_clip_id === clipId);
    if (clip) {
      return { track, clip };
    }
  }
  throw new Error(`找不到时间线片段：${clipId}`);
}

function findTrack(timeline: TimelineJson, trackId: string): TimelineTrackJson {
  const track = timeline.tracks.find((candidate) => candidate.track_id === trackId);
  if (!track) {
    throw new Error(`找不到时间线轨道：${trackId}`);
  }
  return track;
}

function locateEditableClip(timeline: TimelineJson, clipId: string): LocatedClip {
  const located = locateClip(timeline, clipId);
  assertTrackUnlocked(located.track);
  return located;
}

function assertTrackUnlocked(track: TimelineTrackJson): void {
  if (track.locked) {
    throw new Error(`轨道 ${track.track_id} 已锁定`);
  }
}

function ensureRippleUnlocked(
  timeline: TimelineJson,
  boundary: number,
  exceptTrackId: string
): void {
  for (const track of timeline.tracks) {
    if (!track.locked || track.track_id === exceptTrackId) {
      continue;
    }
    if ((track.clips ?? []).some((clip) => numberValue(clip.timeline_end_frame) > boundary)) {
      throw new Error(`轨道 ${track.track_id} 已锁定，不能执行波纹编辑`);
    }
  }
}

function tracksCompatible(
  source: TimelineTrackJson,
  target: TimelineTrackJson,
  clip: TimelineClipJson
): boolean {
  const sourceFamily = trackFamily(source);
  const targetFamily = trackFamily(target);
  if (sourceFamily !== targetFamily) {
    return false;
  }
  const kind = String(clip.asset_kind ?? "");
  if (targetFamily === "visual") {
    return Boolean(clip.asset_id) && (!kind || kind === "video" || kind === "image");
  }
  if (targetFamily === "audio") {
    return Boolean(clip.asset_id) && (!kind || kind === "video" || kind === "audio");
  }
  if (targetFamily === "text") {
    return Boolean(String(clip.text ?? "").trim());
  }
  return source.track_type === target.track_type;
}

function trackFamily(track: TimelineTrackJson): string {
  if (["visual_base", "visual_overlay"].includes(track.track_id)) {
    return "visual";
  }
  if (["original_audio", "voiceover", "bgm", "sfx"].includes(track.track_id)) {
    return "audio";
  }
  if (track.track_id === "subtitles") {
    return "text";
  }
  if (["primary_visual", "visual_overlay", "video"].includes(String(track.track_type ?? ""))) {
    return "visual";
  }
  if (track.track_type === "audio") {
    return "audio";
  }
  if (track.track_type === "text") {
    return "text";
  }
  return String(track.track_type ?? "");
}

function sortTracks(timeline: TimelineJson): void {
  for (const track of timeline.tracks) {
    track.clips = [...(track.clips ?? [])].sort(byStartFrame);
  }
}

function operationCoalesceKey(operation: TimelineOperation): string | null {
  const target = targetClipId(operation, false) || String(operation.track_id ?? "");
  switch (operation.kind) {
    case "move_clip":
    case "reorder_clip":
    case "adjust_gain":
    case "set_clip_fades":
    case "set_clip_linked":
    case "edit_subtitle_text":
    case "set_playback_rate":
    case "trim_clip":
      return `${operation.kind}:${target}`;
    case "trim_clip_edge":
      return `${operation.kind}:${target}:${String(operation.edge ?? "")}`;
    case "set_track_state":
      return `${operation.kind}:${String(operation.track_id ?? "")}`;
    default:
      return null;
  }
}

function targetClipId(operation: TimelineOperation, required = true): string {
  const value = operation.timeline_clip_id ?? operation.clip_id;
  if (typeof value === "string" && value) {
    return value;
  }
  if (required) {
    throw new Error("时间线操作缺少 timeline_clip_id");
  }
  return "";
}

function clipDuration(clip: TimelineClipJson): number {
  return numberValue(clip.timeline_end_frame) - numberValue(clip.timeline_start_frame);
}

function clampTimelineFades(timeline: TimelineJson): void {
  for (const track of timeline.tracks) {
    for (const clip of track.clips ?? []) {
      const duration = Math.max(0, clipDuration(clip));
      const fadeIn = clamp(Math.round(numberValue(clip.fade_in_frames)), 0, duration);
      const fadeOut = clamp(Math.round(numberValue(clip.fade_out_frames)), 0, duration - fadeIn);
      clip.fade_in_frames = fadeIn;
      clip.fade_out_frames = fadeOut;
    }
  }
}

function effectiveRate(clip: TimelineClipJson): number {
  return Math.max(0.01, numberValue(clip.playback_rate, 1));
}

function byStartFrame(left: TimelineClipJson, right: TimelineClipJson): number {
  return numberValue(left.timeline_start_frame) - numberValue(right.timeline_start_frame);
}

function requiredInteger(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isInteger(value)) {
    throw new Error(`${name} 必须是整数帧`);
  }
  return value;
}

function requiredNumber(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${name} 必须是数字`);
  }
  return value;
}

function numberValue(value: unknown, fallback = 0): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

function cloneTimeline(timeline: TimelineJson): TimelineJson {
  return JSON.parse(JSON.stringify(timeline)) as TimelineJson;
}

function cloneOperation(operation: TimelineOperation): TimelineOperation {
  return JSON.parse(JSON.stringify(operation)) as TimelineOperation;
}
