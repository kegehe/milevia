import { useCallback } from "react";
import type { OpenFile } from "./file-model";
import { getFileIcon } from "./file-model";
import { FileIcon } from "./FileIcon";

interface FileTabsProps {
  openFiles: OpenFile[];
  activeFilePath: string | null;
  onTabSelect: (path: string) => void;
  onTabClose: (path: string) => void;
}

export function FileTabs({
  openFiles,
  activeFilePath,
  onTabSelect,
  onTabClose,
}: FileTabsProps) {
  const handleMiddleClick = useCallback(
    (e: React.MouseEvent, path: string) => {
      if (e.button === 1) {
        // 中键点击关闭标签
        e.preventDefault();
        onTabClose(path);
      }
    },
    [onTabClose]
  );

  if (openFiles.length === 0) return null;

  return (
    <div className="file-tabs">
      {openFiles.map((file) => {
        const isActive = file.path === activeFilePath;
        const icon = getFileIcon({ name: file.name, path: file.path, isDir: false });
        return (
          <div
            key={file.path}
            className={`file-tab ${isActive ? "active" : ""}`}
            onClick={() => onTabSelect(file.path)}
            onMouseDown={(e) => handleMiddleClick(e, file.path)}
            title={file.path}
          >
            <span className="file-tab-icon">
              <FileIcon iconKey={icon} size={14} />
            </span>
            <span className="file-tab-name">{file.name}</span>
            {file.isDirty && <span className="file-tab-dirty" />}
            <button
              className="file-tab-close"
              onClick={(e) => {
                e.stopPropagation();
                onTabClose(file.path);
              }}
              title="关闭"
            >
              ×
            </button>
          </div>
        );
      })}
    </div>
  );
}
