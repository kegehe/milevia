import { useCallback, useEffect, useState } from "react";
import type { OpenFile } from "./file-model";
import { getFileIcon } from "./file-model";
import { FileIcon } from "./FileIcon";

interface FileTabsProps {
  openFiles: OpenFile[];
  activeFilePath: string | null;
  onTabSelect: (path: string) => void;
  onTabClose: (path: string) => void;
  onCloseOthers: (path: string) => void;
  onCloseAll: () => void;
  onCloseLeft: (path: string) => void;
  onCloseRight: (path: string) => void;
}

export function FileTabs({
  openFiles,
  activeFilePath,
  onTabSelect,
  onTabClose,
  onCloseOthers,
  onCloseAll,
  onCloseLeft,
  onCloseRight,
}: FileTabsProps) {
  const [contextMenu, setContextMenu] = useState<{
    x: number;
    y: number;
    path: string;
    index: number;
  } | null>(null);

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

  const handleTabKeyDown = useCallback((event: React.KeyboardEvent<HTMLButtonElement>, path: string) => {
    const currentIndex = openFiles.findIndex((file) => file.path === path);
    if (currentIndex < 0) return;
    let nextIndex = currentIndex;
    if (event.key === "ArrowRight") nextIndex = (currentIndex + 1) % openFiles.length;
    else if (event.key === "ArrowLeft") nextIndex = (currentIndex - 1 + openFiles.length) % openFiles.length;
    else if (event.key === "Home") nextIndex = 0;
    else if (event.key === "End") nextIndex = openFiles.length - 1;
    else return;
    event.preventDefault();
    const next = openFiles[nextIndex];
    if (!next) return;
    onTabSelect(next.path);
    requestAnimationFrame(() => document.querySelector<HTMLButtonElement>(`[data-file-tab="${CSS.escape(next.path)}"]`)?.focus());
  }, [onTabSelect, openFiles]);

  const handleContextMenu = useCallback(
    (e: React.MouseEvent, path: string, index: number) => {
      e.preventDefault();
      e.stopPropagation();
      setContextMenu({ x: e.clientX, y: e.clientY, path, index });
    },
    []
  );

  // 点击其他地方关闭菜单
  useEffect(() => {
    if (!contextMenu) return;
    const close = () => setContextMenu(null);
    document.addEventListener("click", close);
    document.addEventListener("contextmenu", close);
    return () => {
      document.removeEventListener("click", close);
      document.removeEventListener("contextmenu", close);
    };
  }, [contextMenu]);

  if (openFiles.length === 0) return null;

  return (
    <div className="file-tabs" role="tablist" aria-label="已打开文件">
      {openFiles.map((file, index) => {
        const isActive = file.path === activeFilePath;
        const icon = getFileIcon({ name: file.name, path: file.path, isDir: false });
        return (
          <div
            key={file.path}
            className={`file-tab ${isActive ? "active" : ""}`}
            onContextMenu={(e) => handleContextMenu(e, file.path, index)}
          >
            <button type="button" className="file-tab-select" role="tab" aria-selected={isActive} aria-controls="file-content-panel" tabIndex={isActive ? 0 : -1} data-file-tab={file.path} title={file.path} onClick={() => onTabSelect(file.path)} onMouseDown={(e) => handleMiddleClick(e, file.path)} onKeyDown={(event) => handleTabKeyDown(event, file.path)}>
              <span className="file-tab-icon"><FileIcon iconKey={icon} size={14} /></span>
              <span className="file-tab-name">{file.name}</span>
              {file.isDirty && <span className="file-tab-dirty" aria-label="未保存" />}
            </button>
            <button
              type="button"
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

      {/* 右键菜单 */}
      {contextMenu && (
        <div
          className="file-tree-context-menu"
          style={{
            left: Math.min(contextMenu.x, window.innerWidth - 160),
            top: Math.min(contextMenu.y, window.innerHeight - 200),
          }}
          onClick={(e) => e.stopPropagation()}
        >
          <button onClick={() => { onTabClose(contextMenu.path); setContextMenu(null); }}>
            关闭
          </button>
          <button onClick={() => { onCloseOthers(contextMenu.path); setContextMenu(null); }}>
            关闭其他
          </button>
          <button
            onClick={() => { onCloseLeft(contextMenu.path); setContextMenu(null); }}
            disabled={contextMenu.index === 0}
          >
            关闭左侧
          </button>
          <button
            onClick={() => { onCloseRight(contextMenu.path); setContextMenu(null); }}
            disabled={contextMenu.index === openFiles.length - 1}
          >
            关闭右侧
          </button>
          <div className="file-tree-context-menu-separator" />
          <button onClick={() => { onCloseAll(); setContextMenu(null); }}>
            关闭所有
          </button>
        </div>
      )}
    </div>
  );
}
