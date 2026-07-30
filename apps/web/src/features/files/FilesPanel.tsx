import { useCallback, useEffect, useRef, useState } from "react";
import { ProjectFileTree } from "./ProjectFileTree";
import { FileViewer } from "./FileViewer";
import { FileEditor } from "./FileEditor";
import { FileTabs } from "./FileTabs";
import type { FileContent, OpenFile } from "./file-model";
import { detectLanguage, isEditableFile } from "./file-model";
import { FileIcon } from "./FileIcon";
import { useCodeFontSize } from "./useCodeFontSize";

interface FilesPanelProps {
  projectId: string;
  runner: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  isWorkspaceOccupied: boolean;
}

const MAX_OPEN_TABS = 10;

function isPathAtOrBelow(path: string, directory: string): boolean {
  return path === directory || path.startsWith(`${directory}/`);
}

function remapPath(path: string, oldPath: string, newPath: string): string {
  if (path === oldPath) return newPath;
  return path.startsWith(`${oldPath}/`) ? `${newPath}${path.slice(oldPath.length)}` : path;
}

function baseName(path: string): string {
  const slash = path.lastIndexOf("/");
  return slash >= 0 ? path.slice(slash + 1) : path;
}

export function FilesPanel({
  projectId,
  runner,
  request,
  isWorkspaceOccupied,
}: FilesPanelProps) {
  const { fontSize, increase, decrease, canIncrease, canDecrease } = useCodeFontSize();
  const [openFiles, setOpenFiles] = useState<OpenFile[]>([]);
  const [activeFilePath, setActiveFilePath] = useState<string | null>(null);
  const [editingFile, setEditingFile] = useState<string | null>(null);
  const [showNewFileDialog, setShowNewFileDialog] = useState<{
    dirPath: string;
    type: "file" | "dir";
  } | null>(null);
  const [newFileName, setNewFileName] = useState("");
  const [showRenameDialog, setShowRenameDialog] = useState<{
    path: string;
    name: string;
  } | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const [showDeleteConfirm, setShowDeleteConfirm] = useState<{
    path: string;
    name: string;
    isDir: boolean;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isSaving, setIsSaving] = useState(false);
  const [mobileView, setMobileView] = useState<"tree" | "editor">("tree");
  const treeRefreshRef = useRef<(() => void) | null>(null);
  const dialogRef = useRef<HTMLElement | null>(null);
  const dialogOpenerRef = useRef<HTMLElement | null>(null);
  const pendingOpens = useRef<Set<string>>(new Set());
  const savingRef = useRef(false);

  // 用 ref 跟踪 openFiles 最新值，避免陈旧闭包问题
  const openFilesRef = useRef(openFiles);
  openFilesRef.current = openFiles;
  const editingFileRef = useRef(editingFile);
  editingFileRef.current = editingFile;

  const readOnly = isWorkspaceOccupied;
  const activeFile = openFiles.find((f) => f.path === activeFilePath) || null;
  const activeDialog = showNewFileDialog ? "create" : showRenameDialog ? "rename" : showDeleteConfirm ? "delete" : null;

  const closeActiveDialog = useCallback(() => {
    setShowNewFileDialog(null);
    setShowRenameDialog(null);
    setShowDeleteConfirm(null);
  }, []);

  useEffect(() => {
    if (!activeDialog) return;
    if (!dialogOpenerRef.current) {
      dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    }
    const dialog = dialogRef.current;
    const focusableSelector = 'button:not(:disabled), input:not(:disabled), [tabindex]:not([tabindex="-1"])';
    const focusFirst = () => dialog?.querySelector<HTMLElement>("input:not(:disabled), button:not(:disabled)")?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); closeActiveDialog(); return; }
      if (event.key !== "Tab" || !dialog) return;
      const focusable = [...dialog.querySelectorAll<HTMLElement>(focusableSelector)];
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    };
    requestAnimationFrame(focusFirst);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      dialogOpenerRef.current?.focus();
      dialogOpenerRef.current = null;
    };
  }, [activeDialog, closeActiveDialog]);

  // 打开文件
  const openFile = useCallback(
    async (path: string, name: string) => {
      // 如果已打开，切换到该标签
      const current = openFilesRef.current;
      const existing = current.find((f) => f.path === path);
      if (existing) {
        setActiveFilePath(path);
        setMobileView("editor");
        return;
      }

      // 防止同一文件的并发请求
      if (pendingOpens.current.has(path)) return;
      pendingOpens.current.add(path);

      try {
        const res = await request<FileContent>(
          `/api/projects/${projectId}/fs/read?path=${encodeURIComponent(path)}`
        );
        const lang = detectLanguage(name);
        const newFile: OpenFile = {
          path,
          name,
          content: res.content,
          originalContent: res.content,
          version: res.version,
          language: lang,
          isDirty: false,
          stat: res.stat,
        };

        // 超出上限且无可关闭的干净标签时拒绝打开
        if (openFilesRef.current.length >= MAX_OPEN_TABS) {
          const hasCleanClosable = openFilesRef.current.some(
            (f) => !f.isDirty && f.path !== path
          );
          if (!hasCleanClosable) {
            setError("标签数已达上限，请先保存或关闭某个标签");
            return;
          }
        }

        setOpenFiles((prev) => {
          // 超出上限时自动关闭最早未修改的标签
          let files = [...prev];
          if (files.length >= MAX_OPEN_TABS) {
            const oldestClean = files.find((f) => !f.isDirty && f.path !== path);
            if (oldestClean) {
              files = files.filter((f) => f.path !== oldestClean.path);
            }
          }
          return [...files, newFile];
        });
        setActiveFilePath(path);
        setMobileView("editor");
      } catch (err) {
        setError(err instanceof Error ? err.message : "无法打开文件");
      } finally {
        pendingOpens.current.delete(path);
      }
    },
    [projectId, request] // openFilesRef / pendingOpens 是 ref 不需要作为依赖
  );

  // 关闭标签
  const closeTab = useCallback(
    (path: string) => {
      setOpenFiles((prev) => prev.filter((f) => f.path !== path));
      setActiveFilePath((prevActive) => {
        if (prevActive !== path) return prevActive;
        const current = openFilesRef.current.filter((f) => f.path !== path);
        if (current.length > 0) {
          const idx = openFilesRef.current.findIndex((f) => f.path === path);
          const nextIdx = Math.min(idx, current.length - 1);
          return current[nextIdx]?.path || null;
        }
        return null;
      });
      if (editingFileRef.current === path) {
        setEditingFile(null);
      }
    },
    []
  );

  // 进入编辑模式
  const enterEditMode = useCallback(() => {
    const currentActive = openFilesRef.current.find(
      (f) => f.path === activeFilePath
    );
    if (currentActive && isEditableFile(currentActive.stat) && !readOnly) {
      setEditingFile(currentActive.path);
    }
  }, [activeFilePath, readOnly]);

  // 编辑器内容变更
  const handleEditorChange = useCallback(
    (content: string) => {
      const currentEditing = editingFileRef.current;
      if (!currentEditing) return;
      setOpenFiles((prev) =>
        prev.map((f) =>
          f.path === currentEditing
            ? { ...f, content, isDirty: content !== f.originalContent }
            : f
        )
      );
    },
    [] // editingFileRef 不需要作为依赖
  );

  // 保存文件
  const saveFile = useCallback(async () => {
    if (savingRef.current) return;
    const currentEditing = editingFileRef.current;
    if (!currentEditing) return;
    const file = openFilesRef.current.find((f) => f.path === currentEditing);
    if (!file) return;

    savingRef.current = true;
    setIsSaving(true);

    try {
      const result = await request<{ version: string }>(`/api/projects/${projectId}/fs/write`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: file.path, content: file.content, expectedVersion: file.version }),
      });
      setOpenFiles((prev) =>
        prev.map((f) =>
          f.path === currentEditing
            ? {
                ...f,
                originalContent: file.content,
                version: result.version,
                isDirty: f.content !== file.content,
              }
            : f
        )
      );
      if (openFilesRef.current.find((f) => f.path === currentEditing)?.content === file.content) {
        setEditingFile(null);
      }
    } catch (err) {
      const status = (err as Error & { status?: number }).status;
      if (status === 409) {
        setError(err instanceof Error ? err.message : "文件已被修改，请重新加载后再保存");
      } else {
        setError(err instanceof Error ? err.message : "保存失败");
      }
    } finally {
      savingRef.current = false;
      setIsSaving(false);
    }
  }, [projectId, request]);

  // 取消编辑
  const cancelEdit = useCallback(() => {
    const currentEditing = editingFileRef.current;
    if (!currentEditing) return;
    setOpenFiles((prev) =>
      prev.map((f) =>
        f.path === currentEditing
          ? { ...f, content: f.originalContent, isDirty: false }
          : f
      )
    );
    setEditingFile(null);
  }, []);

  // readOnly 变为 true 时自动退出编辑模式（恢复原始内容）
  useEffect(() => {
    if (readOnly && editingFileRef.current) {
      cancelEdit();
    }
  }, [readOnly, cancelEdit]);

  // 新建文件
  const handleCreateFile = useCallback((dirPath: string) => {
    dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setShowNewFileDialog({ dirPath, type: "file" });
    setNewFileName("");
  }, []);

  // 新建目录
  const handleCreateDir = useCallback((dirPath: string) => {
    dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setShowNewFileDialog({ dirPath, type: "dir" });
    setNewFileName("");
  }, []);

  // 提交新建
  // 校验文件名是否包含路径遍历字符
  const validateFileName = (name: string): string | null => {
    const trimmed = name.trim();
    if (!trimmed) return "文件名不能为空";
    if (trimmed.includes("/") || trimmed.includes("\\")) return "文件名不能包含路径分隔符";
    if (trimmed.includes("..")) return "文件名不能包含 ..";
    return null;
  };

  const submitNewFile = useCallback(async () => {
    if (!showNewFileDialog || !newFileName.trim()) return;
    const nameError = validateFileName(newFileName);
    if (nameError) {
      setError(nameError);
      return;
    }
    try {
      const path = showNewFileDialog.dirPath
        ? `${showNewFileDialog.dirPath}/${newFileName.trim()}`
        : newFileName.trim();
      if (showNewFileDialog.type === "file") {
        await request(`/api/projects/${projectId}/fs/write`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path, content: "", createOnly: true }),
        });
      } else {
        await request(`/api/projects/${projectId}/fs/mkdir`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path }),
        });
      }
      setShowNewFileDialog(null);
      treeRefreshRef.current?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : "创建失败");
    }
  }, [showNewFileDialog, newFileName, projectId, request]);

  // 重命名
  const handleRename = useCallback((path: string, name: string) => {
    dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setShowRenameDialog({ path, name });
    setRenameValue(name);
  }, []);

  const submitRename = useCallback(async () => {
    if (!showRenameDialog || !renameValue.trim()) return;
    const nameError = validateFileName(renameValue);
    if (nameError) {
      setError(nameError);
      return;
    }
    try {
      const dir = showRenameDialog.path.includes("/")
        ? showRenameDialog.path.substring(0, showRenameDialog.path.lastIndexOf("/"))
        : "";
      const newPath = dir ? `${dir}/${renameValue.trim()}` : renameValue.trim();
      await request(`/api/projects/${projectId}/fs/rename`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ oldPath: showRenameDialog.path, newPath }),
      });
      const oldPath = showRenameDialog.path;
      setOpenFiles((prev) =>
        prev.map((file) => {
          const nextPath = remapPath(file.path, oldPath, newPath);
          if (nextPath === file.path) return file;
          const name = baseName(nextPath);
          return {
            ...file,
            path: nextPath,
            name,
            stat: { ...file.stat, path: nextPath, name },
          };
        })
      );
      setActiveFilePath((path) => (path ? remapPath(path, oldPath, newPath) : null));
      setEditingFile((path) => (path ? remapPath(path, oldPath, newPath) : null));
      setShowRenameDialog(null);
      treeRefreshRef.current?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : "重命名失败");
    }
  }, [showRenameDialog, renameValue, projectId, request]);

  // 删除
  const handleDelete = useCallback((path: string, name: string, isDir: boolean) => {
    dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setShowDeleteConfirm({ path, name, isDir });
  }, []);

  const submitDelete = useCallback(async () => {
    if (!showDeleteConfirm) return;
    try {
      await request(
        `/api/projects/${projectId}/fs/remove?path=${encodeURIComponent(showDeleteConfirm.path)}`,
        { method: "DELETE" }
      );
      const removedPath = showDeleteConfirm.path;
      setOpenFiles((prev) => prev.filter((file) => !isPathAtOrBelow(file.path, removedPath)));
      setActiveFilePath((path) =>
        path && isPathAtOrBelow(path, removedPath) ? null : path
      );
      setEditingFile((path) =>
        path && isPathAtOrBelow(path, removedPath) ? null : path
      );
      setShowDeleteConfirm(null);
      treeRefreshRef.current?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : "删除失败");
    }
  }, [showDeleteConfirm, projectId, request]);

  // 自动清除错误
  useEffect(() => {
    if (!error) return;
    const timer = setTimeout(() => setError(null), 5000);
    return () => clearTimeout(timer);
  }, [error]);

  const isRemote = runner.startsWith("ssh-");

  return (
    <div className="files-panel">
      {/* 错误提示 */}
      {error && (
        <div className="files-error">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      {/* 移动端返回按钮 */}
      {mobileView === "editor" && (
        <button
          className="files-mobile-back"
          onClick={() => setMobileView("tree")}
        >
          ← 文件列表
        </button>
      )}

      <div className="files-content">
        {/* 文件树 */}
        <div className={`files-tree ${mobileView === "editor" ? "hidden-mobile" : ""}`}>
          <ProjectFileTree
            projectId={projectId}
            request={request}
            onFileSelect={openFile}
            onCreateFile={handleCreateFile}
            onCreateDir={handleCreateDir}
            onRename={handleRename}
            onDelete={handleDelete}
            readOnly={readOnly}
            refreshRef={treeRefreshRef}
          />
        </div>

        {/* 查看器/编辑器 */}
        <div id="file-content-panel" role="tabpanel" aria-label="文件内容" className={`files-editor ${mobileView === "tree" ? "hidden-mobile" : ""}`}>
          {/* 标签栏 */}
          <FileTabs
            openFiles={openFiles}
            activeFilePath={activeFilePath}
            onTabSelect={(path) => {
              setActiveFilePath(path);
              setMobileView("editor");
            }}
            onTabClose={closeTab}
          />

          {/* 文件内容区 */}
          {activeFile ? (
            editingFile === activeFile.path ? (
              <FileEditor
                content={activeFile.content}
                stat={activeFile.stat}
                isSaving={isSaving}
                onChange={handleEditorChange}
                onSave={saveFile}
                onCancel={cancelEdit}
                fontSize={fontSize}
              />
            ) : (
              <FileViewer
                key={activeFile.path}
                content={activeFile.content}
                stat={activeFile.stat}
                projectId={projectId}
                onEdit={enterEditMode}
                readOnly={readOnly}
                fontSize={fontSize}
                onIncreaseFont={increase}
                onDecreaseFont={decrease}
                canIncreaseFont={canIncrease}
                canDecreaseFont={canDecrease}
              />
            )
          ) : (
            <div className="files-empty">
              <FileIcon iconKey="folder" size={36} />
              <div className="files-empty-text">
                {isRemote ? "选择文件查看" : "选择文件查看或编辑"}
              </div>
              {readOnly && (
                <div className="files-empty-hint">
                  AI 运行中，文件编辑已锁定
                </div>
              )}
            </div>
          )}
        </div>
      </div>

      {/* 新建文件/目录对话框 */}
      {showNewFileDialog && (
        <div className="files-dialog-backdrop" onClick={closeActiveDialog}>
          <section ref={dialogRef} className="files-dialog" role="dialog" aria-modal="true" aria-labelledby="file-create-title" onClick={(e) => e.stopPropagation()}>
            <header><h3 id="file-create-title">{showNewFileDialog.type === "file" ? "新建文件" : "新建目录"}</h3><button type="button" className="files-dialog-close" title="关闭" aria-label="关闭" onClick={closeActiveDialog}>x</button></header>
            <input
              type="text"
              value={newFileName}
              onChange={(e) => setNewFileName(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && submitNewFile()}
              placeholder={
                showNewFileDialog.type === "file" ? "文件名（如 new-file.ts）" : "目录名"
              }
              autoFocus
            />
            <div className="files-dialog-actions">
              <button type="button" className="primary" onClick={submitNewFile}>
                创建
              </button>
              <button type="button" onClick={closeActiveDialog}>取消</button>
            </div>
          </section>
        </div>
      )}

      {/* 重命名对话框 */}
      {showRenameDialog && (
        <div className="files-dialog-backdrop" onClick={closeActiveDialog}>
          <section ref={dialogRef} className="files-dialog" role="dialog" aria-modal="true" aria-labelledby="file-rename-title" onClick={(e) => e.stopPropagation()}>
            <header><h3 id="file-rename-title">重命名</h3><button type="button" className="files-dialog-close" title="关闭" aria-label="关闭" onClick={closeActiveDialog}>x</button></header>
            <input
              type="text"
              value={renameValue}
              onChange={(e) => setRenameValue(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && submitRename()}
              autoFocus
            />
            <div className="files-dialog-actions">
              <button type="button" className="primary" onClick={submitRename}>
                确认
              </button>
              <button type="button" onClick={closeActiveDialog}>取消</button>
            </div>
          </section>
        </div>
      )}

      {/* 删除确认 */}
      {showDeleteConfirm && (
        <div className="files-dialog-backdrop" onClick={closeActiveDialog}>
          <section ref={dialogRef} className="files-dialog files-dialog-danger" role="dialog" aria-modal="true" aria-labelledby="file-delete-title" onClick={(e) => e.stopPropagation()}>
            <header><h3 id="file-delete-title">确认删除</h3><button type="button" className="files-dialog-close" title="关闭" aria-label="关闭" onClick={closeActiveDialog}>x</button></header>
            <p>
              确定要删除「{showDeleteConfirm.name}」
              {showDeleteConfirm.isDir ? " 及其所有内容" : ""}吗？此操作不可撤销。
            </p>
            <div className="files-dialog-actions">
              <button type="button" className="danger" onClick={submitDelete}>
                删除
              </button>
              <button type="button" onClick={closeActiveDialog}>取消</button>
            </div>
          </section>
        </div>
      )}
    </div>
  );
}
