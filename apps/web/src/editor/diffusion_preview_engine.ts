import * as core from "@diffusionstudio/core";
import type { TimelineClipJson, TimelineJson, TimelineTrackJson } from "../api/client";
import { frameOffsetTime, frameTime } from "./frame_time";
import { previewCoverLayout } from "./preview_layout";
import { resumePreviewClock } from "./preview_clock";
import { timelineRuntimeSignature } from "./preview_timeline_signature";

export type DiffusionMediaResolver = (assetId: string, assetKind: string) => string;

type RuntimeClip = {
  clip: core.Clip;
  sourceKey: string;
  trackId: string;
};

// DiffusionPreviewEngine 只做代理素材即时预览。Rushes 的整数帧时间线仍是唯一
// 业务事实；传给 Core 的所有时间值都使用 `123f`，不在持久层引入第二种时间基。
export class DiffusionPreviewEngine {
  readonly composition: core.Composition;
  private readonly sources = new Map<string, core.BaseSource>();
  private readonly runtimeClips = new Map<string, RuntimeClip>();
  private readonly layers = new Map<string, core.Layer>();
  private structureSignature = "";
  private runtimeSignature = "";
  private hasSynced = false;
  private syncTail: Promise<void> = Promise.resolve();
  private fps = 30;
  private disposed = false;

  constructor(private readonly resolveMedia: DiffusionMediaResolver) {
    this.composition = new core.Composition({
      width: 1920,
      height: 1080,
      background: "#000000",
      playbackEndBehavior: "stop"
    });
  }

  mount(element: HTMLElement): void {
    this.composition.mount(element);
    const canvas = this.composition.renderer.canvas;
    canvas.dataset.diffusionPreview = "true";
    canvas.style.display = "block";
    canvas.style.width = "100%";
    canvas.style.height = "100%";
    canvas.style.objectFit = "contain";
  }

  sync(timeline: TimelineJson): Promise<void> {
    if (this.disposed) {
      return Promise.resolve();
    }
    const queued = this.syncTail.then(() => this.syncNow(timeline));
    // 同一时刻最多允许一次 rebuild/update。失败只反馈给当前调用者，不让
    // 队列永久进入 rejected 状态，后续真实编辑仍可再次同步。
    this.syncTail = queued.catch(() => undefined);
    return queued;
  }

  private async syncNow(timeline: TimelineJson): Promise<void> {
    if (this.disposed) {
      return;
    }
    const fps = timeline.fps > 0 ? timeline.fps : 30;
    core.env.experimental_timeBase = fps;
    this.fps = fps;
    const structureSignature = timelineStructureSignature(timeline);
    const runtimeSignature = timelineRuntimeSignature(timeline);
    if (this.hasSynced && runtimeSignature === this.runtimeSignature) {
      return;
    }
    try {
      if (this.hasSynced && structureSignature === this.structureSignature) {
        await this.updateExisting(timeline);
      } else {
        await this.rebuild(timeline, structureSignature);
      }
      this.runtimeSignature = runtimeSignature;
      this.hasSynced = true;
    } catch (error) {
      this.runtimeSignature = "";
      this.hasSynced = false;
      throw error;
    }
  }

  async seekFrame(frame: number): Promise<void> {
    await withPreviewTimeout(
      this.composition.seek(frameTime(frame)),
      5_000,
      "代理预览定位超时"
    );
  }

  async play(): Promise<void> {
    await this.resumeAt();
  }

  async pause(): Promise<void> {
    await this.composition.pause();
  }

  get playing(): boolean {
    return this.composition.playing;
  }

  get clockState(): string {
    return this.composition.renderer.audioCtx.state;
  }

  get clockTime(): number {
    return this.composition.renderer.audioCtx.currentTime;
  }

  get currentFrame(): number {
    return Math.round(this.composition.currentTime * this.fps);
  }

  get durationFrames(): number {
    return Math.round(this.composition.duration * this.fps);
  }

  async recoverPlayback(): Promise<void> {
    const frame = Math.min(this.currentFrame, this.durationFrames);
    await withPreviewTimeout(
      (async () => {
        await this.composition.pause().catch(() => undefined);
        await this.composition.seek(frameTime(frame));
        await this.resumeAt(frame);
      })(),
      5_000,
      "代理预览自动恢复超时"
    );
  }

  onTime(callback: (seconds: number) => void): () => void {
    return this.composition.on("playback:time", (seconds) => {
      if (typeof seconds === "number") {
        callback(seconds);
      }
    });
  }

  async dispose(): Promise<void> {
    if (this.disposed) {
      return;
    }
    this.disposed = true;
    await this.composition.pause().catch(() => undefined);
    this.composition.unmount();
    this.composition.clear();
    this.sources.clear();
    this.runtimeClips.clear();
    this.layers.clear();
    this.structureSignature = "";
    this.runtimeSignature = "";
    this.hasSynced = false;
    const audioContext = this.composition.renderer.audioCtx;
    if (audioContext instanceof AudioContext && audioContext.state !== "closed") {
      await audioContext.close().catch(() => undefined);
    }
  }

  private async updateExisting(timeline: TimelineJson): Promise<void> {
    const currentFrame = Math.min(this.currentFrame, timeline.duration_frames);
    const wasPlaying = this.composition.playing;
    if (wasPlaying) {
      await this.composition.pause();
    }
    const originalAudio = findTrack(timeline, "original_audio");
    const soloAudio = timeline.tracks.some(
      (track) => isAudioTrack(track.track_id, true) && track.solo === true
    );
    for (const track of timeline.tracks) {
      const layer = this.layers.get(track.track_id);
      if (layer) {
        layer.disabled = layerDisabled(track, soloAudio);
      }
      for (const clipJson of track.clips ?? []) {
        const id = stringValue(clipJson.timeline_clip_id);
        const runtime = this.runtimeClips.get(id);
        if (!runtime) {
          continue;
        }
        applyRuntimeTiming(runtime.clip, clipJson);
        applyRuntimeAudio(runtime.clip, track, clipJson, originalAudio, soloAudio, this.fps);
        if (runtime.clip instanceof core.TextClip) {
          runtime.clip.text = stringValue(clipJson.text) || "字幕";
        }
      }
    }
    await this.composition.seek(frameTime(currentFrame));
    if (wasPlaying) {
      await this.resumeAt(currentFrame);
    }
  }

  private async rebuild(timeline: TimelineJson, signature: string): Promise<void> {
    const wasPlaying = this.composition.playing;
    const currentFrame = Math.min(this.currentFrame, timeline.duration_frames);
    if (wasPlaying) {
      await this.composition.pause();
    }
    this.composition.clear();
    this.runtimeClips.clear();
    this.layers.clear();
    this.structureSignature = "";

    const width = positiveNumber(timeline.width, 1920);
    const height = positiveNumber(timeline.height, 1080);
    if (this.composition.width !== width || this.composition.height !== height) {
      this.composition.resize(width, height);
    }
    const originalAudio = findTrack(timeline, "original_audio");
    const soloAudio = timeline.tracks.some(
      (track) => isAudioTrack(track.track_id, true) && track.solo === true
    );
    for (const track of timeline.tracks) {
      // 原声由对应 VideoClip 自带的音频流播放，避免把同一画面作为第二个视频层
      // 重复绘制；BGM、SFX、配音仍分别建立独立 AudioClip/Layer。
      if (track.track_id === "original_audio" || (track.clips ?? []).length === 0) {
        continue;
      }
      const layer = new core.Layer({ mode: "DEFAULT" });
      layer.id = `rushes_${track.track_id}`;
      layer.data = { rushesTrackId: track.track_id };
      layer.disabled = layerDisabled(track, soloAudio);
      // Composition 的 index=0 是顶层。按 base → overlay → subtitles 遍历时，
      // 后加入的视觉层放到顶端，确保叠加画面和字幕不会被主视频遮住。
      await this.composition.add(layer, 0);
      this.layers.set(track.track_id, layer);

      for (const clipJson of track.clips ?? []) {
        const runtime = await this.createRuntimeClip(track, clipJson, originalAudio, soloAudio);
        if (!runtime) {
          continue;
        }
        await layer.add(runtime.clip);
        this.runtimeClips.set(stringValue(clipJson.timeline_clip_id), runtime);
      }
    }
    this.structureSignature = signature;
    await this.composition.seek(frameTime(currentFrame));
    if (wasPlaying) {
      await this.resumeAt(currentFrame);
    }
  }

  private async resumeAt(frame?: number): Promise<void> {
    await resumePreviewClock(this.composition.renderer.audioCtx);
    await withPreviewTimeout(
      this.composition.play(frame === undefined ? undefined : frameTime(frame)),
      5_000,
      "代理预览启动超时"
    );
  }

  private async createRuntimeClip(
    track: TimelineTrackJson,
    clipJson: TimelineClipJson,
    originalAudio: TimelineTrackJson | null,
    soloAudio: boolean
  ): Promise<RuntimeClip | null> {
    const id = stringValue(clipJson.timeline_clip_id);
    if (!id) {
      return null;
    }
    const timelineStartFrame = numberValue(clipJson.timeline_start_frame);
    const start = frameTime(timelineStartFrame);
    const duration = frameTime(
      numberValue(clipJson.timeline_end_frame) - numberValue(clipJson.timeline_start_frame)
    );
    let clip: core.Clip;
    let sourceKey = "";

    if (track.track_id === "subtitles") {
      clip = new core.TextClip({
        text: stringValue(clipJson.text) || "字幕",
        delay: start,
        duration,
        x: "50%",
        y: "84%",
        align: "center",
        baseline: "middle",
        maxWidth: "82%",
        fontSize: 48,
        color: "#FFFFFF",
        strokes: [{ width: 3, color: "#000000" }]
      });
    } else {
      const assetId = stringValue(clipJson.asset_id);
      if (!assetId) {
        return null;
      }
      const assetKind = stringValue(clipJson.asset_kind) || inferAssetKind(track.track_id);
      sourceKey = `${assetKind}:${assetId}`;
      const source = await this.source(sourceKey, assetId, assetKind);
      const sourceStartFrame = numberValue(clipJson.source_start_frame);
      const range: [core.Time, core.Time] = [
        frameTime(sourceStartFrame),
        frameTime(numberValue(clipJson.source_end_frame))
      ];
      // AudioClip/VideoClip 的 start = delay + range[0]。直接把时间线起点
      // 写成 delay 会把所有取自素材中段的镜头再向后平移一遍，造成随机黑屏。
      const mediaDelay = frameOffsetTime(timelineStartFrame - sourceStartFrame);
      if (assetKind === "image") {
        const visualSource = source as core.ImageSource;
        const layout = previewCoverLayout(
          visualSource.width,
          visualSource.height,
          this.composition.width,
          this.composition.height
        );
        clip = new core.ImageClip(source as core.ImageSource, {
          delay: start,
          duration,
          position: "center",
          ...layout
        });
      } else if (isAudioTrack(track.track_id) || assetKind === "audio") {
        const audio = audioSettings(track, clipJson, soloAudio);
        clip = new core.AudioClip(source as core.AudioSource, {
          delay: mediaDelay,
          range,
          volume: audio.volume,
          muted: audio.muted
        });
      } else {
        const audio = videoAudioSettings(track, clipJson, originalAudio, soloAudio);
        const visualSource = source as core.VideoSource;
        const layout = previewCoverLayout(
          visualSource.width,
          visualSource.height,
          this.composition.width,
          this.composition.height
        );
        clip = new core.VideoClip(source as core.VideoSource, {
          delay: mediaDelay,
          range,
          position: "center",
          ...layout,
          volume: audio.volume,
          muted: audio.muted
        });
      }
    }
    clip.id = id;
    clip.name = stringValue(clipJson.asset_id) || id;
    clip.data = {
      rushesTrackId: track.track_id,
      rushesAssetId: stringValue(clipJson.asset_id) || null,
      rushesRole: stringValue(clipJson.role) || null
    };
    applyRuntimeAudio(clip, track, clipJson, originalAudio, soloAudio, this.fps);
    return { clip, sourceKey, trackId: track.track_id };
  }

  private async source(
    key: string,
    assetId: string,
    assetKind: string
  ): Promise<core.BaseSource> {
    const cached = this.sources.get(key);
    if (cached) {
      return cached;
    }
    const url = this.resolveMedia(assetId, assetKind);
    let source: core.BaseSource;
    if (assetKind === "audio") {
      source = await core.Source.from<core.AudioSource>(url, { mimeType: "audio/mpeg" });
    } else if (assetKind === "image") {
      source = await core.Source.from<core.ImageSource>(url);
    } else {
      source = await core.Source.from<core.VideoSource>(url, { mimeType: "video/mp4" });
    }
    this.sources.set(key, source);
    return source;
  }
}

function applyRuntimeTiming(clip: core.Clip, value: TimelineClipJson): void {
  const timelineStartFrame = numberValue(value.timeline_start_frame);
  const durationFrames =
    numberValue(value.timeline_end_frame) - numberValue(value.timeline_start_frame);
  if (clip instanceof core.AudioClip) {
    const sourceStartFrame = numberValue(value.source_start_frame);
    clip.range = [
      frameTime(sourceStartFrame),
      frameTime(numberValue(value.source_end_frame))
    ];
    clip.delay = frameOffsetTime(timelineStartFrame - sourceStartFrame);
  } else {
    clip.delay = frameTime(timelineStartFrame);
    clip.duration = frameTime(durationFrames);
  }
}

function applyRuntimeAudio(
  clip: core.Clip,
  track: TimelineTrackJson,
  value: TimelineClipJson,
  originalAudio: TimelineTrackJson | null,
  soloAudio: boolean,
  fps: number
): void {
  if (!(clip instanceof core.AudioClip)) {
    return;
  }
  const settings = clip instanceof core.VideoClip
    ? videoAudioSettings(track, value, originalAudio, soloAudio)
    : audioSettings(track, value, soloAudio);
  clip.volume = settings.volume;
  clip.muted = settings.muted;
  clip.fadeInDurationSeconds = settings.fadeInFrames / fps;
  clip.fadeOutDurationSeconds = settings.fadeOutFrames / fps;
}

type RuntimeAudioSettings = {
  volume: number;
  muted: boolean;
  fadeInFrames: number;
  fadeOutFrames: number;
};

function videoAudioSettings(
  track: TimelineTrackJson,
  clip: TimelineClipJson,
  originalAudio: TimelineTrackJson | null,
  soloAudio: boolean
): RuntimeAudioSettings {
  if (track.track_id !== "visual_base") {
    return { volume: 0, muted: true, fadeInFrames: 0, fadeOutFrames: 0 };
  }
  const match = (originalAudio?.clips ?? []).find((candidate) =>
    clip.parent_block_id
      ? candidate.parent_block_id === clip.parent_block_id
      : candidate.asset_id === clip.asset_id &&
        candidate.timeline_start_frame === clip.timeline_start_frame &&
        candidate.timeline_end_frame === clip.timeline_end_frame
  );
  if (!match) {
    return {
      volume: 1,
      muted: soloAudio,
      fadeInFrames: frameValue(clip.fade_in_frames),
      fadeOutFrames: frameValue(clip.fade_out_frames)
    };
  }
  return audioSettings(originalAudio!, match, soloAudio);
}

function audioSettings(
  track: TimelineTrackJson,
  clip: TimelineClipJson,
  soloAudio: boolean
): RuntimeAudioSettings {
  const gain = numberValue(track.gain_db) + numberValue(clip.gain_db);
  return {
    volume: Math.min(1, Math.max(0, 10 ** (gain / 20))),
    muted: track.muted === true || (soloAudio && track.solo !== true) || gain <= -60,
    fadeInFrames: frameValue(clip.fade_in_frames),
    fadeOutFrames: frameValue(clip.fade_out_frames)
  };
}

function layerDisabled(track: TimelineTrackJson, soloAudio: boolean): boolean {
  if (isVisualTrack(track.track_id)) {
    return track.muted === true;
  }
  return track.muted === true || (soloAudio && isAudioTrack(track.track_id, true) && track.solo !== true);
}

function timelineStructureSignature(timeline: TimelineJson): string {
  return [
    timeline.width,
    timeline.height,
    ...timeline.tracks
    .flatMap((track) =>
      (track.clips ?? []).map(
        (clip) =>
          `${track.track_id}:${String(clip.timeline_clip_id)}:${String(clip.asset_id)}:${String(clip.asset_kind)}`
      )
    )
  ].join("|");
}

async function withPreviewTimeout<T>(
  promise: Promise<T>,
  timeoutMs: number,
  message: string
): Promise<T> {
  let timeout: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_resolve, reject) => {
        timeout = setTimeout(() => reject(new Error(message)), timeoutMs);
      })
    ]);
  } finally {
    if (timeout !== undefined) {
      clearTimeout(timeout);
    }
  }
}

function findTrack(timeline: TimelineJson, trackId: string): TimelineTrackJson | null {
  return timeline.tracks.find((track) => track.track_id === trackId) ?? null;
}

function isVisualTrack(trackId: string): boolean {
  return trackId === "visual_base" || trackId === "visual_overlay";
}

function isAudioTrack(trackId: string, includeOriginal = false): boolean {
  return ["voiceover", "bgm", "sfx", ...(includeOriginal ? ["original_audio"] : [])].includes(
    trackId
  );
}

function inferAssetKind(trackId: string): string {
  return isAudioTrack(trackId) ? "audio" : "video";
}

function positiveNumber(value: unknown, fallback: number): number {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : fallback;
}

function numberValue(value: unknown, fallback = 0): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function frameValue(value: unknown): number {
  return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : 0;
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}
