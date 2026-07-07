import { useState } from "react";
import type { ChangeEvent, DragEvent, ReactElement } from "react";
import { api } from "../../api/client";
import { ApiError } from "../../auth";

type UploadDropzoneProps = {
  projectId: string;
  onUploaded: () => Promise<void> | void;
};

type UploadItem = {
  name: string;
  progress: number;
  status: "uploading" | "completed" | "failed";
};

type UploadRejection = {
  filename: string;
  message: string;
};

const CHUNK_SIZE = 5 * 1024 * 1024;

export function UploadDropzone({ projectId, onUploaded }: UploadDropzoneProps): ReactElement {
  const [dragging, setDragging] = useState(false);
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const [rejections, setRejections] = useState<UploadRejection[]>([]);

  async function uploadFiles(files: FileList | File[]): Promise<void> {
    setRejections([]);
    const collected: UploadRejection[] = [];
    for (const file of Array.from(files)) {
      try {
        await uploadOne(file);
      } catch (error) {
        collected.push({ filename: file.name, message: rejectionMessage(error) });
      }
    }
    if (collected.length > 0) {
      setRejections(collected);
    }
  }

  async function uploadOne(file: File): Promise<void> {
    upsertUpload(file.name, { progress: 0, status: "uploading" });
    try {
      const init = await api.initUpload({
        project_id: projectId,
        filename: file.name,
        size: file.size
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
      await api.completeUpload(init.complete_url, { project_id: projectId });
      upsertUpload(file.name, { progress: 100, status: "completed" });
      await onUploaded();
    } catch (error) {
      upsertUpload(file.name, { progress: 100, status: "failed" });
      throw error;
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
    <section className="rounded-lg border border-line bg-panel p-4">
      <div>
        <h2 className="font-semibold">上传文件</h2>
        <p className="mt-1 text-sm text-fg-muted">拖拽或选择文件，前端按分片上传，类型由文件后缀自动识别。</p>
      </div>

      <label
        className={`mt-4 flex min-h-32 cursor-pointer flex-col items-center justify-center rounded-lg border border-dashed px-4 py-6 text-center ${
          dragging ? "border-accent bg-raised" : "border-line-strong bg-raised"
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
        <span className="text-sm font-medium text-fg">拖拽文件到这里</span>
        <span className="mt-1 text-xs text-fg-muted">或点击选择本地文件</span>
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
              <div className="flex items-center justify-between gap-3 text-xs text-fg-muted">
                <span className="truncate">{item.name}</span>
                <span>{uploadStatusLabel(item)}</span>
              </div>
              <div className="mt-1 h-1.5 rounded bg-line">
                <div className="h-1.5 rounded bg-accent" style={{ width: `${item.progress}%` }} />
              </div>
            </div>
          ))}
        </div>
      ) : null}

      {rejections.length > 0 ? (
        <div className="mt-4 rounded-md bg-danger/15 px-3 py-2 text-sm text-danger">
          <p className="font-medium">{`拒收 ${rejections.length} 个`}</p>
          <ul className="mt-1 space-y-1">
            {rejections.map((item, index) => (
              <li key={`${item.filename}-${index}`} className="text-xs">
                {`${item.filename}：${item.message}`}
              </li>
            ))}
          </ul>
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

function rejectionMessage(error: unknown): string {
  if (error instanceof ApiError) {
    const payload = error.payload;
    if (payload && typeof payload === "object" && "detail" in payload) {
      const detail = (payload as { detail?: unknown }).detail;
      if (detail && typeof detail === "object" && "message" in detail) {
        const message = (detail as { message?: unknown }).message;
        if (typeof message === "string" && message.trim().length > 0) {
          return message;
        }
      }
    }
  }
  return "上传失败，请稍后重试。";
}
