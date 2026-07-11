import { useRef } from "react";
import type { PointerEvent as ReactPointerEvent, ReactElement } from "react";

type ResizeHandleProps = {
  orientation: "vertical" | "horizontal";
  value: number;
  onChange: (next: number) => void;
  ariaLabel: string;
  /** true 时拖动方向取反（如时间线顶边：向上拖增高）。 */
  invert?: boolean;
};

/** 面板分隔拖拽手柄；宽/高的钳制由 store 负责。 */
export function ResizeHandle({
  orientation,
  value,
  onChange,
  ariaLabel,
  invert = false
}: ResizeHandleProps): ReactElement {
  const dragRef = useRef<{ startPos: number; startValue: number } | null>(null);

  const readPos = (event: ReactPointerEvent<HTMLDivElement>): number =>
    orientation === "vertical" ? event.clientX : event.clientY;

  return (
    <div
      role="separator"
      aria-orientation={orientation}
      aria-label={ariaLabel}
      className={
        orientation === "vertical"
          ? "w-1 shrink-0 cursor-col-resize bg-line transition-colors hover:bg-accent/50"
          : "h-1 shrink-0 cursor-row-resize bg-line transition-colors hover:bg-accent/50"
      }
      onPointerDown={(event) => {
        dragRef.current = { startPos: readPos(event), startValue: value };
        event.currentTarget.setPointerCapture(event.pointerId);
      }}
      onPointerMove={(event) => {
        if (!dragRef.current) {
          return;
        }
        const delta = readPos(event) - dragRef.current.startPos;
        onChange(dragRef.current.startValue + (invert ? -delta : delta));
      }}
      onPointerUp={() => {
        dragRef.current = null;
      }}
      onPointerCancel={() => {
        dragRef.current = null;
      }}
    />
  );
}
