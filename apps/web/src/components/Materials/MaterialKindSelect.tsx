import type { ReactElement } from "react";
import type { MaterialKind } from "../../api/client";

type MaterialKindSelectProps = {
  value: MaterialKind;
  onChange: (kind: MaterialKind) => void;
};

const KIND_OPTIONS: { value: MaterialKind; label: string }[] = [
  { value: "video", label: "视频" },
  { value: "audio", label: "音频" },
  { value: "image", label: "图片" },
  { value: "voiceover", label: "配音" },
  { value: "bgm", label: "BGM" },
  { value: "font", label: "字体" },
  { value: "subtitle_template", label: "字幕模板" }
];

export function MaterialKindSelect({ value, onChange }: MaterialKindSelectProps): ReactElement {
  return (
    <label className="block text-sm font-medium text-[#334155]">
      类型
      <select
        className="mt-2 w-full rounded-md border border-[#cbd5e1] bg-white px-3 py-2 outline-none focus:border-[#2563eb]"
        value={value}
        onChange={(event) => onChange(event.target.value as MaterialKind)}
      >
        {KIND_OPTIONS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
}
