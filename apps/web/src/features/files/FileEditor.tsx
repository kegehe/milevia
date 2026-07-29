import { useCallback, useEffect, useRef, useState } from "react";
import type { FileInfo } from "./file-model";
import { detectLanguage } from "./file-model";

interface FileEditorProps {
  content: string;
  stat: FileInfo;
  isSaving: boolean;
  onChange: (content: string) => void;
  onSave: () => void;
  onCancel: () => void;
  fontSize?: number;
}

// CodeMirror 语言包懒加载映射
const languageLoaders: Record<string, () => Promise<unknown>> = {
  javascript: () => import("@codemirror/lang-javascript").then((m) => m.javascript()),
  typescript: () =>
    import("@codemirror/lang-javascript").then((m) => m.javascript({ typescript: true })),
  tsx: () =>
    import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true, typescript: true })),
  jsx: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true })),
  css: () => import("@codemirror/lang-css").then((m) => m.css()),
  html: () => import("@codemirror/lang-html").then((m) => m.html()),
  json: () => import("@codemirror/lang-json").then((m) => m.json()),
  python: () => import("@codemirror/lang-python").then((m) => m.python()),
  go: () => import("@codemirror/lang-go").then((m) => m.go()),
};

export function FileEditor({ content, stat, isSaving, onChange, onSave, onCancel, fontSize = 13 }: FileEditorProps) {
  const [EditorModule, setEditorModule] = useState<typeof import("@uiw/react-codemirror") | null>(
    null
  );
  const [languageExt, setLanguageExt] = useState<unknown>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const editorRef = useRef<HTMLDivElement>(null);

  // 懒加载 CodeMirror
  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const [codemirrorModule, langExt] = await Promise.all([
          import("@uiw/react-codemirror"),
          loadLanguageExtension(detectLanguage(stat.name)),
        ]);
        if (cancelled) return;
        setEditorModule(codemirrorModule);
        setLanguageExt(langExt);
        setLoading(false);
      } catch (err) {
        if (cancelled) return;
        setLoadError(err instanceof Error ? err.message : "加载编辑器失败");
        setLoading(false);
      }
    };
    load();
    return () => {
      cancelled = true;
    };
  }, [stat.name]);

  // Ctrl+S / Cmd+S 保存（使用 capture 阶段确保优先于其他处理器）
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "s") {
        e.preventDefault();
        e.stopPropagation();
        onSave();
      }
    };
    document.addEventListener("keydown", handler, true);
    return () => document.removeEventListener("keydown", handler, true);
  }, [onSave]);

  const handleChange = useCallback(
    (value: string) => {
      onChange(value);
    },
    [onChange]
  );

  if (loading) {
    return (
      <div className="file-editor-loading">
        <div className="file-editor-spinner" />
        <span>加载编辑器...</span>
      </div>
    );
  }

  if (loadError || !EditorModule) {
    return (
      <div className="file-editor-error">
        <p>编辑器加载失败：{loadError}</p>
        <button onClick={onCancel}>返回查看</button>
      </div>
    );
  }

  const extensions = Array.isArray(languageExt) ? languageExt : [languageExt];

  return (
    <div className="file-editor" ref={editorRef} onKeyDown={(e) => {
      if (e.key === "Escape") {
        // 如果事件来自 CodeMirror 编辑器内部（如搜索面板），不让它处理
        const cmEditor = editorRef.current?.querySelector('.cm-editor');
        if (cmEditor && cmEditor.contains(e.target as Node)) return;
        e.stopPropagation();
        onCancel();
      }
    }}>
      <EditorModule.default
        value={content}
        onChange={handleChange}
        extensions={extensions}
        theme="light"
        basicSetup={{
          lineNumbers: true,
          highlightActiveLine: true,
          bracketMatching: true,
          closeBrackets: true,
          indentOnInput: true,
          foldGutter: true,
          searchKeymap: true,
        }}
        className="file-editor-codemirror"
        style={{ '--cm-font-size': `${fontSize}px` } as React.CSSProperties}
      />
      <div className="file-editor-toolbar">
        <button className="file-editor-save primary" onClick={onSave} disabled={isSaving}>
          {isSaving ? "保存中..." : "保存 (Ctrl+S)"}
        </button>
        <button className="file-editor-cancel" onClick={onCancel}>
          取消
        </button>
      </div>
    </div>
  );
}

async function loadLanguageExtension(lang: string): Promise<unknown[]> {
  const loader = languageLoaders[lang];
  if (!loader) return [];
  try {
    const ext = await loader();
    return [ext];
  } catch {
    return [];
  }
}
