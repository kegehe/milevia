import { useEffect } from "react";
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
}

export function FileEditor({ content, stat, isSaving, onChange, onSave, onCancel, fontSize = 13 }: FileEditorProps) {
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

  return <div className="file-editor">
    <CodeFileView content={content} filename={stat.name} fontSize={fontSize} editable onChange={onChange} />
    <div className="file-editor-toolbar"><button className="file-editor-save primary" onClick={onSave} disabled={isSaving}>{isSaving ? "保存中..." : "保存 (Ctrl+S)"}</button><button className="file-editor-cancel" onClick={onCancel}>取消</button></div>
  </div>;
}
