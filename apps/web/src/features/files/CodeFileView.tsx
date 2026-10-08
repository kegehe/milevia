import { useEffect, useState } from "react";
import { loadLanguageExtension } from "./source-language";

interface CodeFileViewProps {
  content: string;
  filename: string;
  fontSize: number;
  editable?: boolean;
  onChange?: (value: string) => void;
  /**
   * 软换行。手机端**必须**开：代码在窄屏上横向滚动是没法读的，
   * 而且横向滚动和"从边缘滑出返回手势"会互相抢事件。
   *
   * `EditorView` 不用新增依赖 —— `@uiw/react-codemirror` 已经
   * `export * from '@codemirror/view'` 了。
   */
  wrap?: boolean;
}

function noMoveSelection(editorView: typeof import("@uiw/react-codemirror").EditorView) {
  // 签名是 (event, view)，`view` 由 CM 推断为 EditorView
  return editorView.domEventHandlers({
    mousedown(event, view) {
      if (event.button !== 0 || event.shiftKey || event.metaKey || event.ctrlKey || event.altKey) return;
      const { main } = view.state.selection;
      if (main.empty) return;
      const pos = view.posAtCoords({ x: event.clientX, y: event.clientY }, false);
      if (pos == null) return;
      if (pos >= main.from && pos <= main.to) {
        view.dispatch({ selection: { anchor: pos }, userEvent: "select.pointer" });
      }
    },
  });
}

export function CodeFileView({ content, filename, fontSize, editable = false, onChange, wrap = false }: CodeFileViewProps) {
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
        const languageExtensions = nextExtensions as any[];
        // 只改“查看”模式：查看时“选区内按下再拖”被 CodeMirror 当作“移动整块选中内容”，
        // 于是拖到目标行它却不进选区（鼠标起点落在已有选区内时最容易踩中）；这里在主键
        // mousedown 且落点落在当前选区内部时，先把选区折叠成该点游标，内建逻辑就会把它
        // 当作“从按下点新开并扩展选区”。编辑模式保留 CM 默认的拖拽移动行为，不在其中生效。
        const viewExtras = editable ? [] : [noMoveSelection(module.EditorView)];
        setExtensions(
          wrap
            ? [...languageExtensions, module.EditorView.lineWrapping, ...viewExtras]
            : [...languageExtensions, ...viewExtras]
        );
      })
      .catch(() => {
        if (!cancelled) setError("无法加载源码查看器，请刷新页面后重试。");
      });
    return () => { cancelled = true; };
  }, [filename, wrap, editable]);

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
