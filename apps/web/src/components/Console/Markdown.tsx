import { lazy, Suspense } from "react";
import type { ReactElement } from "react";

// react-markdown + remark-gfm（连带 micromark）体积可观，且 #116 后只在 message_completed
// 落库后才渲染。懒加载把它移出首屏入口 chunk：首个完成消息触发一次按需加载，加载期间用
// 纯文本兜底（与流式期间的纯文本观感一致），加载后由 React.lazy 缓存，后续消息即时渲染。
const MarkdownBody = lazy(async () => {
  const [{ default: ReactMarkdown }, { default: remarkGfm }] = await Promise.all([
    import("react-markdown"),
    import("remark-gfm")
  ]);
  return {
    default: function MarkdownBody({ text }: { text: string }): ReactElement {
      return <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>;
    }
  };
});

/** 聊天正文 Markdown 渲染：GFM（列表/表格/删除线），样式见 index.css 的 .md-body。 */
export function Markdown({ text }: { text: string }): ReactElement {
  return (
    <div className="md-body">
      <Suspense fallback={<p className="whitespace-pre-wrap">{text}</p>}>
        <MarkdownBody text={text} />
      </Suspense>
    </div>
  );
}
