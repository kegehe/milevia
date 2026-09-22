import { useEffect, useRef } from "react";
import type { FileInfo } from "./file-model";
import { CodeFileView } from "./CodeFileView";

interface FileEditorProps {
  content: string;
  stat: FileInfo;
  isSaving: boolean;
  onChange: (content: string) => void;
  onSave: () => void;
  onCancel: () => void;
  fontSize?: number;
  /** 手机端：软换行 + 软键盘避让。 */
  mobile?: boolean;
}

export function FileEditor({ content, stat, isSaving, onChange, onSave, onCancel, fontSize = 13, mobile = false }: FileEditorProps) {
  const editorRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const handler = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") {
        event.preventDefault();
        onSave();
      }
    };
    document.addEventListener("keydown", handler, true);
    return () => document.removeEventListener("keydown", handler, true);
  }, [onSave]);

  // 软键盘弹出时把整个编辑器压进**还看得见**的那块区域。
  //
  // 不做这件事的话，键盘会盖住「保存」按钮与光标所在行 —— 用户在手机上改到一半，
  // 必须先手动收起键盘才能按保存（Android WebView 实测如此）。
  // 用 `visualViewport` 而不是 `window.innerHeight`：后者在键盘弹出时**不变**，
  // 拿它算高度等于什么都没有做。
  useEffect(() => {
    if (!mobile) return;
    const viewport = window.visualViewport;
    if (!viewport) return;
    const apply = () => {
      const keyboardUp = viewport.height < window.innerHeight - 1;
      if (!keyboardUp) {
        document.documentElement.style.removeProperty("--file-editor-visible-height");
        return;
      }
      // 量的是"从编辑器顶边到键盘顶边"这一段，而不是整个可视高度：
      // 编辑器上方还压着页头（会话页顶栏是 sticky 的），只按可视高度压的话，
      // 工具栏仍会落在键盘下面 —— 看起来像"避让没生效"。
      const top = editorRef.current?.getBoundingClientRect().top ?? 0;
      const available = Math.max(200, Math.round(viewport.height - top));
      document.documentElement.style.setProperty("--file-editor-visible-height", `${available}px`);
    };
    apply();
    viewport.addEventListener("resize", apply);
    viewport.addEventListener("scroll", apply);
    return () => {
      viewport.removeEventListener("resize", apply);
      viewport.removeEventListener("scroll", apply);
      document.documentElement.style.removeProperty("--file-editor-visible-height");
    };
  }, [mobile]);

  return <div className={`file-editor${mobile ? " file-editor-mobile" : ""}`} ref={editorRef}>
    <CodeFileView content={content} filename={stat.name} fontSize={fontSize} editable onChange={onChange} wrap={mobile} />
    <div className="file-editor-toolbar"><button className="file-editor-save primary" onClick={onSave} disabled={isSaving}>{isSaving ? "保存中..." : "保存"}{!mobile && " (Ctrl+S)"}</button><button className="file-editor-cancel" onClick={onCancel}>取消</button></div>
  </div>;
}
