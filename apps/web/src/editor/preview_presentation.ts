import type { TimelineClipJson, TimelineJson, TimelineTrackJson } from "../api/client";

export type SubtitlePreviewStyle = {
  y: `${number}%`;
  fontSize: number;
  outline: number;
  bold: boolean;
  assBorderStyle: 1 | 3;
  background?: {
    fill: "#000000";
    opacity: number;
    borderRadius: number;
    padding: { x: number; y: number };
  };
};

const subtitlePreviewStyles: Record<string, SubtitlePreviewStyle> = {
  default: { y: "92%", fontSize: 42, outline: 3, bold: false, assBorderStyle: 1 },
  large_center: { y: "50%", fontSize: 62, outline: 4, bold: true, assBorderStyle: 1 },
  top_bar: {
    y: "7%", fontSize: 44, outline: 1, bold: true, assBorderStyle: 3,
    background: { fill: "#000000", opacity: 44, borderRadius: 0, padding: { x: 18, y: 10 } }
  },
  minimal: { y: "94%", fontSize: 36, outline: 1, bold: false, assBorderStyle: 1 },
  bold_bottom: { y: "91%", fontSize: 52, outline: 5, bold: true, assBorderStyle: 1 }
};

const duckingAttackSeconds = 0.015;
const duckingReleaseSeconds = 0.250;

export function subtitlePreviewPreset(
  style: TimelineClipJson["subtitle_style"],
  compositionHeight = 1080
): SubtitlePreviewStyle {
  const preset = subtitlePreviewStyles[typeof style === "string" ? style : ""] ?? subtitlePreviewStyles.default;
  const scale = Number.isFinite(compositionHeight) && compositionHeight > 0
    ? compositionHeight / 1080
    : 1;
  return {
    ...preset,
    fontSize: Math.max(1, Math.round(preset.fontSize * scale)),
    outline: Math.max(0, Math.round(preset.outline * scale))
  };
}

export function duckedPreviewVolume(
  timeline: TimelineJson,
  targetTrack: TimelineTrackJson,
  baseVolume: number,
  frame: number,
  soloAudio = false,
  hasVideoAudio: (clip: TimelineClipJson) => boolean = () => true
): number {
  const ducking = targetTrack.ducking;
  if (!ducking?.enabled) {
    return baseVolume;
  }
  const envelopes: number[] = [];
  for (const trackId of ducking.trigger_tracks) {
    const trigger = timeline.tracks.find((track) => track.track_id === trackId);
    if (!trigger || trigger.muted === true ||
      (soloAudio && isAudioTrack(trigger.track_id) && trigger.solo !== true)) {
      continue;
    }
    let clips = trigger.clips ?? [];
    if (trackId === "original_audio" && clips.length === 0) {
      const visualBase = timeline.tracks.find((track) => track.track_id === "visual_base");
      if (visualBase?.muted !== true) {
        clips = (visualBase?.clips ?? []).filter((clip) =>
          clip.asset_kind === "video" && hasVideoAudio(clip)
        );
      }
    }
    for (const clip of clips) {
      const start = integerFrame(clip.timeline_start_frame);
      const end = integerFrame(clip.timeline_end_frame);
      if (end > start) {
        envelopes.push(duckingEnvelope(
          frame,
          start,
          end,
          timeline.fps * duckingAttackSeconds,
          timeline.fps * duckingReleaseSeconds
        ));
      }
    }
  }
  const duckDB = typeof ducking.duck_db === "number" && Number.isFinite(ducking.duck_db)
    ? ducking.duck_db
    : 0;
  const duckGain = 10 ** (Math.min(0, duckDB) / 20);
  const envelope = envelopes.reduce((gain, value) => Math.max(gain, value), 0);
  return baseVolume * (1 - (1 - duckGain) * envelope);
}

function duckingEnvelope(
  frame: number,
  start: number,
  end: number,
  attack: number,
  release: number
): number {
  if (frame < start || frame >= end + release) {
    return 0;
  }
  if (frame < end) {
    return attack > 0 ? Math.min(1, (frame - start) / Math.min(attack, end - start)) : 1;
  }
  return release > 0 ? 1 - (frame - end) / release : 0;
}

function integerFrame(value: unknown): number {
  return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : 0;
}

function isAudioTrack(trackId: string): boolean {
  return ["voiceover", "bgm", "sfx", "original_audio"].includes(trackId);
}
