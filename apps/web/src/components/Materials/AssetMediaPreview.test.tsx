import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { MaterialAsset } from "../../api/client";
import { AssetMediaPreview } from "./AssetMediaPreview";

// 素材试看：原片优先 + 原片播不动时回落 proxy / 转码中提示。
describe("AssetMediaPreview 原片优先与回落", () => {
  it("视频默认直连原片 source", () => {
    render(<AssetMediaPreview asset={makeAsset({ asset_id: "v", kind: "video" })} />);
    const video = screen.getByLabelText("v.mp4 视频试看");
    expect(video.getAttribute("src")).toContain("/api/media/v/source");
  });

  it("原片 onError 且 proxy 就绪时回落到 proxy", () => {
    render(
      <AssetMediaPreview asset={makeAsset({ asset_id: "v", kind: "video", proxy_ready: true })} />
    );
    fireEvent.error(screen.getByLabelText("v.mp4 视频试看"));
    // 回落后重挂的媒体元素改指向 proxy
    expect(screen.getByLabelText("v.mp4 视频试看").getAttribute("src")).toContain(
      "/api/media/v/proxy"
    );
  });

  it("原片 onError 但 proxy 未就绪时提示转码中", () => {
    render(
      <AssetMediaPreview asset={makeAsset({ asset_id: "v", kind: "video", proxy_ready: false })} />
    );
    fireEvent.error(screen.getByLabelText("v.mp4 视频试看"));
    expect(screen.getByText("转码中，稍候可预览。")).toBeTruthy();
  });

  it("图片走原图直连，不进 proxy 回落逻辑", () => {
    render(
      <AssetMediaPreview
        asset={makeAsset({ asset_id: "p", kind: "image", filename: "p.jpg" })}
      />
    );
    const img = screen.getByAltText("p.jpg");
    expect(img.getAttribute("src")).toContain("/api/media/p/source");
  });
});

function makeAsset(overrides: Partial<MaterialAsset> = {}): MaterialAsset {
  return {
    asset_id: "asset_1",
    storage_mode: "reference",
    kind: "video",
    source: "local_path",
    filename: `${overrides.asset_id ?? "asset_1"}.mp4`,
    hash: "hash_1",
    size: 1024,
    mtime: 0,
    ingest_status: "indexed",
    understanding_status: "none",
    usable: true,
    rel_dir: null,
    probe: null,
    duration_sec: null,
    proxy_object_hash: null,
    proxy_ready: false,
    thumbnail_ready: true,
    invalid: false,
    failure: null,
    jobs: [],
    ...overrides
  };
}
