import type { ReactElement } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

/** 聊天正文 Markdown 渲染：GFM（列表/表格/删除线），样式见 index.css 的 .md-body。 */
export function Markdown({ text }: { text: string }): ReactElement {
  return (
    <div className="md-body">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  );
}
