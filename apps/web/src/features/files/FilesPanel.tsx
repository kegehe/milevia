import { useCallback, useEffect, useRef, useState } from "react";
import { ProjectFileTree } from "./ProjectFileTree";
import { FileViewer } from "./FileViewer";
import { FileEditor } from "./FileEditor";
import { FileTabs } from "./FileTabs";
import type { FileContent, FileInfo, OpenFile } from "./file-model";
import { detectLanguage, getDirPath, isEditableFile } from "./file-model";
import { getPreviewKind, isTextPreview } from "./source-language";
import { FileIcon } from "./FileIcon";
import { useCodeFontSize } from "./useCodeFontSize";
import type { NavigationGuard } from "../../components/ProjectLayout";

interface FilesPanelProps {
  projectId: string;
  conversationId?: string;
  runner: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  isWorkspaceOccupied: boolean;
  onAddToChat?: (path: string) => void;
  registerNavigationGuard: (guard: NavigationGuard | null) => void;
  // 进入面板时自动打开的文件（相对项目根的路径）。用于从其它页面深链跳到某个文件，
  // 例如优化建议卡片的「查看文件」。只在挂载后消费一次。
  initialPath?: string | null;
  // initialPath 被消费后回调，让持有方清掉它（避免下次进入又打开同一个文件）。
  onInitialPathConsumed?: () => void;
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
  conversationId,
  runner,
  request,
  isWorkspaceOccupied,
  onAddToChat,
  registerNavigationGuard,
  initialPath,
  onInitialPathConsumed,
}: FilesPanelProps) {
	const workspaceQuery = conversationId ? `conversationId=${encodeURIComponent(conversationId)}` : "";
	// 记忆化：它被三个提交回调写在依赖里，每次渲染都换新函数会让那三个回调白重建，
	// 也让「依赖列表」失去意义（submitDelete 曾经因此漏了它）。
	const withWorkspace = useCallback(
		(path: string) => `${path}${path.includes("?") ? "&" : "?"}${workspaceQuery}`,
		[workspaceQuery]
	);
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
  const [pendingDiscard, setPendingDiscard] = useState<{ files: OpenFile[]; proceed: () => void } | null>(null);
  const [mobileView, setMobileView] = useState<"tree" | "editor">("tree");
  // 刷新文件树：只重新拉取发生变化的目录，避免整棵树折叠、丢失滚动位置
  const treeRefreshRef = useRef<((dirPath?: string, preferPath?: string) => void) | null>(null);
  const dialogRef = useRef<HTMLElement | null>(null);
  const dialogOpenerRef = useRef<HTMLElement | null>(null);
  const pendingOpens = useRef<Set<string>>(new Set());
  const savingRef = useRef(false);
  // 删除确认的「删除」按钮是默认焦点（回车即确认），且弹窗在请求返回前不会关闭；
  // 双击、连按回车都会再触发一次 submitDelete，这里用 ref 做同步的防重复提交
  // （与上面的 savingRef 同一套路）。
  const deleteInFlightRef = useRef(false);

  // 用 ref 跟踪 openFiles 最新值，避免陈旧闭包问题
  const openFilesRef = useRef(openFiles);
  openFilesRef.current = openFiles;
  const editingFileRef = useRef(editingFile);
  editingFileRef.current = editingFile;

  const readOnly = isWorkspaceOccupied;
  const activeFile = openFiles.find((f) => f.path === activeFilePath) || null;
  const activeDialog = showNewFileDialog ? "create" : showRenameDialog ? "rename" : showDeleteConfirm ? "delete" : pendingDiscard ? "discard" : null;

  const requestDiscard = useCallback((files: OpenFile[], proceed: () => void) => {
    const dirtyFiles = files.filter((file) => file.isDirty);
    if (dirtyFiles.length === 0) {
      proceed();
      return;
    }
    setPendingDiscard({ files: dirtyFiles, proceed });
  }, []);

  const dirtyFiles = openFiles.filter((file) => file.isDirty);

  useEffect(() => {
    if (dirtyFiles.length === 0) {
      registerNavigationGuard(null);
      return;
    }
    registerNavigationGuard((proceed) => requestDiscard(openFilesRef.current, proceed));
    return () => registerNavigationGuard(null);
  }, [dirtyFiles.length, registerNavigationGuard, requestDiscard]);

  useEffect(() => {
    if (dirtyFiles.length === 0) return;
    const warnBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warnBeforeUnload);
    return () => window.removeEventListener("beforeunload", warnBeforeUnload);
  }, [dirtyFiles.length]);

  const closeActiveDialog = useCallback(() => {
    setShowNewFileDialog(null);
    setShowRenameDialog(null);
    setShowDeleteConfirm(null);
    // ESC / 点遮罩 = 取消放弃，用户的未保存编辑照旧留着
    setPendingDiscard(null);
  }, []);

  useEffect(() => {
    if (!activeDialog) return;
    if (!dialogOpenerRef.current) {
      dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    }
    const dialog = dialogRef.current;
    const focusableSelector = 'button:not(:disabled), input:not(:disabled), [tabindex]:not([tabindex="-1"])';
    // 默认焦点的优先级：显式标记 data-autofocus（删除确认里的「删除」按钮，回车即确认）
    // → 输入框（新建 / 重命名弹窗，回车即提交）→ 第一个可聚焦按钮。
    // 顺序不能省：右上角关闭按钮在 DOM 里排在输入框前面，若直接用 "input, button"
    // 会按文档序命中「x」，默认焦点被抢走，回车就变成了取消。
    const focusFirst = () => {
      const target =
        dialog?.querySelector<HTMLElement>("[data-autofocus]:not(:disabled)") ??
        dialog?.querySelector<HTMLElement>("input:not(:disabled)") ??
        dialog?.querySelector<HTMLElement>("button:not(:disabled)");
      target?.focus();
    };
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
      // 焦点归还必须延后一帧。回车按下时浏览器对该次 keydown 的默认动作（激活「当时
      // 获得焦点」的元素）发生在监听器与微任务之后：若在这里同步把焦点还给触发按钮，
      // 紧接着的默认动作就会再点它一次——表现为回车提交后弹窗刚关又被重新打开。
      const opener = dialogOpenerRef.current;
      dialogOpenerRef.current = null;
      requestAnimationFrame(() => {
        if (opener?.isConnected) opener.focus();
      });
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
		const stat = await request<FileInfo>(withWorkspace(`/api/projects/${projectId}/fs/stat?path=${encodeURIComponent(path)}`));
        const previewKind = getPreviewKind(name, stat.isText, stat.mimeType, stat.size);
        const res = isTextPreview(previewKind)
		  ? await request<FileContent>(withWorkspace(`/api/projects/${projectId}/fs/read?path=${encodeURIComponent(path)}`))
          : null;
        const lang = detectLanguage(name);
        const newFile: OpenFile = {
          path,
          name,
          content: res?.content ?? "",
          originalContent: res?.content ?? "",
          version: res?.version ?? "",
          language: lang,
          isDirty: false,
          stat: res?.stat ?? stat,
          previewKind,
          contentLoaded: Boolean(res),
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

  // 深链打开指定文件（如优化建议卡片的「查看文件」）。只消费一次：打开后通知持有方清掉，
  // 否则下次回到文件页会再次自动打开同一个文件。
  const initialPathConsumedRef = useRef<string | null>(null);
  useEffect(() => {
    const target = initialPath?.trim();
    if (!target || initialPathConsumedRef.current === target) return;
    initialPathConsumedRef.current = target;
    const name = target.split(/[\\/]/).pop() || target;
    void openFile(target, name);
    onInitialPathConsumed?.();
  }, [initialPath, openFile, onInitialPathConsumed]);

  // 关闭标签
  const closeTab = useCallback(
    (path: string) => {
      const file = openFilesRef.current.find((item) => item.path === path);
      if (!file) return;
      requestDiscard([file], () => {
        setOpenFiles((prev) => prev.filter((f) => f.path !== path));
        setActiveFilePath((prevActive) => {
          if (prevActive !== path) return prevActive;
          const current = openFilesRef.current.filter((f) => f.path !== path);
          if (current.length > 0) {
            const idx = openFilesRef.current.findIndex((f) => f.path === path);
            return current[Math.min(idx, current.length - 1)]?.path || null;
          }
          return null;
        });
        if (editingFileRef.current === path) setEditingFile(null);
      });
    },
    [requestDiscard]
  );

  // 关闭其他标签
  const closeOthers = useCallback(
    (path: string) => {
      const toClose = openFilesRef.current.filter((file) => file.path !== path);
      requestDiscard(toClose, () => {
        setOpenFiles((prev) => prev.filter((f) => f.path === path));
        setActiveFilePath(path);
        if (editingFileRef.current && editingFileRef.current !== path) setEditingFile(null);
      });
    },
    [requestDiscard]
  );

  // 关闭所有标签
  const closeAll = useCallback(() => {
    requestDiscard(openFilesRef.current, () => {
      setOpenFiles([]);
      setActiveFilePath(null);
      setEditingFile(null);
    });
  }, [requestDiscard]);

  // 关闭左侧标签
  const closeLeft = useCallback(
    (path: string) => {
      const current = openFilesRef.current;
      const idx = current.findIndex((f) => f.path === path);
      if (idx <= 0) return;
      const toClose = current.slice(0, idx);
      requestDiscard(toClose, () => {
        const closedPaths = new Set(toClose.map((file) => file.path));
        setOpenFiles((prev) => prev.filter((file) => !closedPaths.has(file.path)));
        if (editingFileRef.current && closedPaths.has(editingFileRef.current)) setEditingFile(null);
        setActiveFilePath((prevActive) => prevActive && closedPaths.has(prevActive) ? path : prevActive);
      });
    },
    [requestDiscard]
  );

  // 关闭右侧标签
  const closeRight = useCallback(
    (path: string) => {
      const current = openFilesRef.current;
      const idx = current.findIndex((f) => f.path === path);
      if (idx < 0 || idx >= current.length - 1) return;
      const toClose = current.slice(idx + 1);
      requestDiscard(toClose, () => {
        const closedPaths = new Set(toClose.map((file) => file.path));
        setOpenFiles((prev) => prev.filter((file) => !closedPaths.has(file.path)));
        if (editingFileRef.current && closedPaths.has(editingFileRef.current)) setEditingFile(null);
        setActiveFilePath((prevActive) => prevActive && closedPaths.has(prevActive) ? path : prevActive);
      });
    },
    [requestDiscard]
  );

  // 进入编辑模式
  const enterEditMode = useCallback(() => {
    const currentActive = openFilesRef.current.find(
      (f) => f.path === activeFilePath
    );
    if (currentActive && currentActive.contentLoaded && isEditableFile(currentActive.stat) && !readOnly) {
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
      const result = await request<{ version: string }>(withWorkspace(`/api/projects/${projectId}/fs/write`), {
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
  }, [projectId, request, withWorkspace]);

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
        await request(withWorkspace(`/api/projects/${projectId}/fs/write`), {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path, content: "", createOnly: true }),
        });
      } else {
        await request(withWorkspace(`/api/projects/${projectId}/fs/mkdir`), {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ path }),
        });
      }
      setShowNewFileDialog(null);
      treeRefreshRef.current?.(showNewFileDialog.dirPath);
    } catch (err) {
      setError(err instanceof Error ? err.message : "创建失败");
    }
  }, [showNewFileDialog, newFileName, projectId, request, withWorkspace]);

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
      await request(withWorkspace(`/api/projects/${projectId}/fs/rename`), {
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
      // 第二个参数：如果焦点原本在被改名的条目上，让它落到改名后的新路径上
      treeRefreshRef.current?.(getDirPath(newPath), newPath);
    } catch (err) {
      setError(err instanceof Error ? err.message : "重命名失败");
    }
  }, [showRenameDialog, renameValue, projectId, request, withWorkspace]);

  // 删除
  const handleDelete = useCallback((path: string, name: string, isDir: boolean) => {
    dialogOpenerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setShowDeleteConfirm({ path, name, isDir });
  }, []);

  const submitDelete = useCallback(async () => {
    if (!showDeleteConfirm || deleteInFlightRef.current) return;
    deleteInFlightRef.current = true;
    try {
      await request(
        withWorkspace(`/api/projects/${projectId}/fs/remove?path=${encodeURIComponent(showDeleteConfirm.path)}`),
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
      treeRefreshRef.current?.(getDirPath(removedPath));
    } catch (err) {
      setError(err instanceof Error ? err.message : "删除失败");
    } finally {
      deleteInFlightRef.current = false;
    }
  }, [showDeleteConfirm, projectId, request, withWorkspace]);

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
            conversationId={conversationId}
            request={request}
            onFileSelect={openFile}
            onCreateFile={handleCreateFile}
            onCreateDir={handleCreateDir}
            onRename={handleRename}
            onDelete={handleDelete}
            onAddToChat={onAddToChat}
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
            onCloseOthers={closeOthers}
            onCloseAll={closeAll}
            onCloseLeft={closeLeft}
            onCloseRight={closeRight}
          />

          {/* 文件内容区 */}
          {activeFile ? (
            editingFile === activeFile.path && activeFile.contentLoaded ? (
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
                previewKind={activeFile.previewKind}
                projectId={projectId}
                conversationId={conversationId}
                request={request}
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

      {/* 放弃未保存的更改（关标签 / 关闭全部 / 导航守卫都会走到这里） */}
      {pendingDiscard && (
        <div className="files-dialog-backdrop" onClick={closeActiveDialog} role="presentation">
          <section ref={dialogRef} className="files-dialog" role="dialog" aria-modal="true" aria-labelledby="discard-unsaved-title" onClick={(e) => e.stopPropagation()}>
            <header><h3 id="discard-unsaved-title">放弃未保存的更改？</h3></header>
            <p>以下文件有未保存的编辑：{pendingDiscard.files.map((file) => file.name).join("、")}。继续操作将丢失这些更改。</p>
            <div className="files-dialog-actions">
              {/* 默认焦点给「取消」而不是破坏性的「放弃更改」：这个弹窗可能由回车/导航触发，
                  误按回车不能直接丢掉用户没保存的编辑。 */}
              <button type="button" data-autofocus onClick={closeActiveDialog}>取消</button>
              <button type="button" className="primary danger" onClick={() => { const action = pendingDiscard.proceed; setPendingDiscard(null); action(); }}>放弃更改</button>
            </div>
          </section>
        </div>
      )}

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
              <button type="button" className="danger" data-autofocus onClick={submitDelete}>
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
