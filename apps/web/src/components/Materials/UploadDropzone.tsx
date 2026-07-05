import { useState } from "react";
import type { ChangeEvent, DragEvent, ReactElement } from "react";
import { api, type MaterialKind } from "../../api/client";
import { MaterialKindSelect } from "./MaterialKindSelect";

type UploadDropzoneProps = {
  projectId: string;
  onUploaded: () => Promise<void> | void;
};

type UploadItem = {
  name: string;
  progress: number;
  status: "uploading" | "completed" | "failed";
};

const CHUNK_SIZE = 5 * 1024 * 1024;

export function UploadDropzone({ projectId, onUploaded }: UploadDropzoneProps): ReactElement {
  const [kind, setKind] = useState<MaterialKind>("video");
  const [dragging, setDragging] = useState(false);
  const [uploads, setUploads] = useState<UploadItem[]>([]);

  async function uploadFiles(files: FileList | File[]): Promise<void> {
    for (const file of Array.from(files)) {
      await uploadOne(file);
    }
  }

  async function uploadOne(file: File): Promise<void> {
    upsertUpload(file.name, { progress: 0, status: "uploading" });
    try {
      const init = await api.initUpload({
        project_id: projectId,
        filename: file.name,
        size: file.size,
        kind
      });
      const partCount = Math.max(1, Math.ceil(file.size / CHUNK_SIZE));
      for (let index = 0; index < partCount; index += 1) {
        const partNumber = index + 1;
        const start = index * CHUNK_SIZE;
        const end = Math.min(file.size, start + CHUNK_SIZE);
        const blob = file.slice(start, end);
        const partUrl = init.part_url_template.replace("{part_number}", String(partNumber));
        await api.uploadPart(partUrl, blob);
        upsertUpload(file.name, {
          progress: Math.round((partNumber / partCount) * 90),
          status: "uploading"
        });
      }
      await api.completeUpload(init.complete_url, { project_id: projectId, kind });
      upsertUpload(file.name, { progress: 100, status: "completed" });
      await onUploaded();
    } catch {
      upsertUpload(file.name, { progress: 100, status: "failed" });
    }
  }

  function upsertUpload(name: string, patch: Partial<UploadItem>): void {
    setUploads((current) => {
      const existing = current.find((item) => item.name === name);
      if (!existing) {
        return [...current, { name, progress: patch.progress ?? 0, status: patch.status ?? "uploading" }];
      }
      return current.map((item) => (item.name === name ? { ...item, ...patch } : item));
    });
  }

  return (
    <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="font-semibold">上传文件</h2>
          <p className="mt-1 text-sm text-[#64748b]">拖拽或选择文件，前端按分片上传。</p>
        </div>
        <div className="w-36">
          <MaterialKindSelect value={kind} onChange={setKind} />
        </div>
      </div>

      <label
        className={`mt-4 flex min-h-32 cursor-pointer flex-col items-center justify-center rounded-lg border border-dashed px-4 py-6 text-center ${
          dragging ? "border-[#2563eb] bg-[#eff6ff]" : "border-[#cbd5e1] bg-[#f8fafc]"
        }`}
        onDragOver={(event: DragEvent<HTMLLabelElement>) => {
          event.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(event: DragEvent<HTMLLabelElement>) => {
          event.preventDefault();
          setDragging(false);
          void uploadFiles(event.dataTransfer.files);
        }}
      >
        <span className="text-sm font-medium text-[#334155]">拖拽文件到这里</span>
        <span className="mt-1 text-xs text-[#64748b]">或点击选择本地文件</span>
        <input
          aria-label="选择上传文件"
          className="sr-only"
          multiple
          type="file"
          onChange={(event: ChangeEvent<HTMLInputElement>) => {
            if (event.currentTarget.files) {
              void uploadFiles(event.currentTarget.files);
            }
            event.currentTarget.value = "";
          }}
        />
      </label>

      {uploads.length > 0 ? (
        <div className="mt-4 space-y-2">
          {uploads.map((item) => (
            <div key={item.name}>
              <div className="flex items-center justify-between gap-3 text-xs text-[#475569]">
                <span className="truncate">{item.name}</span>
                <span>{uploadStatusLabel(item)}</span>
              </div>
              <div className="mt-1 h-1.5 rounded bg-[#e2e8f0]">
                <div className="h-1.5 rounded bg-[#2563eb]" style={{ width: `${item.progress}%` }} />
              </div>
            </div>
          ))}
        </div>
      ) : null}
    </section>
  );
}

function uploadStatusLabel(item: UploadItem): string {
  if (item.status === "completed") {
    return "上传完成";
  }
  if (item.status === "failed") {
    return "上传失败";
  }
  return `${item.progress}%`;
}
