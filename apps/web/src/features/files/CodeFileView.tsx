import { useEffect, useState } from "react";
import { loadLanguageExtension } from "./source-language";

interface CodeFileViewProps {
  content: string;
  filename: string;
  fontSize: number;
  editable?: boolean;
  onChange?: (value: string) => void;
}

export function CodeFileView({ content, filename, fontSize, editable = false, onChange }: CodeFileViewProps) {
  const [EditorModule, setEditorModule] = useState<typeof import("@uiw/react-codemirror") | null>(null);
  const [extensions, setExtensions] = useState<any[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setEditorModule(null);
    setExtensions([]);
    setError(null);
    void Promise.all([import("@uiw/react-codemirror"), loadLanguageExtension(filename)])
      .then(([module, nextExtensions]) => {
        if (cancelled) return;
        setEditorModule(module);
        setExtensions(nextExtensions as any[]);
      })
      .catch(() => {
        if (!cancelled) setError("无法加载源码查看器，请刷新页面后重试。");
      });
    return () => { cancelled = true; };
  }, [filename]);

  if (error) return <div className="file-editor-error"><p>{error}</p></div>;
  if (!EditorModule) return <div className="file-editor-loading"><div className="file-editor-spinner" /><span>加载源码查看器...</span></div>;

  return (
    <EditorModule.default
      value={content}
      onChange={editable ? onChange : undefined}
      extensions={extensions}
      editable={editable}
      theme="light"
      basicSetup={{
        lineNumbers: true,
        highlightActiveLine: true,
        bracketMatching: true,
        closeBrackets: editable,
        indentOnInput: editable,
        foldGutter: true,
        searchKeymap: true,
      }}
      className="file-editor-codemirror"
      style={{ "--cm-font-size": `${fontSize}px` } as React.CSSProperties}
    />
  );
}
