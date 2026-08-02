import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Tree,
  TreeItem,
  TreeItemIndex,
  UncontrolledTreeEnvironment,
} from "react-complex-tree";
import type { FileEntry, TreeResponse, SearchResponse } from "./file-model";
import { getFileIcon } from "./file-model";
import { FileIcon } from "./FileIcon";

// sessionStorage key：暂存要添加到对话的文件路径
const ADD_TO_CHAT_KEY = "milevia_add_file_to_chat";

async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to legacy method
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.append(textarea);
    textarea.select();
    const ok = document.execCommand("copy");
    textarea.remove();
    return ok;
  } catch {
    return false;
  }
}

// ─── 类型 ────────────────────────────────────────────────────────────────────

interface ProjectFileTreeProps {
  projectId: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onFileSelect: (path: string, name: string) => void;
  onCreateFile: (dirPath: string) => void;
  onCreateDir: (dirPath: string) => void;
  onRename: (path: string, name: string) => void;
  onDelete: (path: string, name: string, isDir: boolean) => void;
  onAddToChat?: (path: string) => void;
  readOnly: boolean;
  refreshRef?: React.MutableRefObject<(() => void) | null>;
}

interface FileTreeItemData {
  name: string;
  path: string;
  isDir: boolean;
  icon: string;
}

function TreeActionIcon({ name }: { name: "new-file" | "new-folder" | "refresh" }) {
  const paths = {
    "new-file": <><path d="M5 2.8h6.4L16 7.4v9.8a1.8 1.8 0 0 1-1.8 1.8H5A1.8 1.8 0 0 1 3.2 17.2V4.6A1.8 1.8 0 0 1 5 2.8Z" /><path d="M11.2 2.9v4.6h4.6M9.6 11v5M7.1 13.5h5" /></>,
    "new-folder": <><path d="M2.8 6.2A1.8 1.8 0 0 1 4.6 4.4h3l1.8 2h6.1a1.8 1.8 0 0 1 1.8 1.8v7.2a1.8 1.8 0 0 1-1.8 1.8H4.6a1.8 1.8 0 0 1-1.8-1.8V6.2Z" /><path d="M10 10v5M7.5 12.5h5" /></>,
    refresh: <><path d="M16.6 7.8A6.7 6.7 0 1 0 18 12" /><path d="M16.6 3.8v4h-4" /></>,
  };
  return <svg className="file-tree-action-icon" viewBox="0 0 20 20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.55" strokeLinecap="round" strokeLinejoin="round">{paths[name]}</svg>;
}

// ─── Provider ────────────────────────────────────────────────────────────────

class FileTreeProvider {
  private items: Map<string, TreeItem<FileTreeItemData>> = new Map();
  private listeners: Set<(changedItemIds: TreeItemIndex[]) => void> = new Set();
  private fetchFn: (path: string) => Promise<FileEntry[]>;
  private onLoadingChange?: (loading: boolean) => void;
  private onError?: (message: string) => void;
  // 记录每个目录正在进行的请求，防止重复请求与旧请求覆盖新数据
  private pendingLoads = new Map<string, Promise<void>>();
  // 按目录追踪的加载代次，仅丢弃同一目录的过期请求（避免跨目录干扰）
  private dirEpochs = new Map<string, number>();

  constructor(
    fetchFn: (path: string) => Promise<FileEntry[]>,
    onLoadingChange?: (loading: boolean) => void,
    onError?: (message: string) => void
  ) {
    this.fetchFn = fetchFn;
    this.onLoadingChange = onLoadingChange;
    this.onError = onError;
    this.items.set("root", {
      index: "root",
      data: { name: "/", path: "", isDir: true, icon: "folder" },
      isFolder: true,
      children: [],
    });
  }

  // TreeDataProvider 接口
  async getTreeItem(itemId: TreeItemIndex): Promise<TreeItem<FileTreeItemData>> {
    const item = this.items.get(String(itemId));
    if (item) return item;
    // 占位项：UncontrolledTreeEnvironment 在 onMissingItems 时会请求
    return {
      index: itemId,
      data: { name: String(itemId), path: String(itemId), isDir: false, icon: "file" },
      isFolder: false,
    };
  }

  async getTreeItems(itemIds: TreeItemIndex[]): Promise<TreeItem<FileTreeItemData>[]> {
    return Promise.all(itemIds.map((id) => this.getTreeItem(id)));
  }

  async onChangeItemChildren(itemId: TreeItemIndex, newChildren: TreeItemIndex[]): Promise<void> {
    const item = this.items.get(String(itemId));
    if (item) {
      item.children = newChildren;
    }
    this.notifyChange([itemId]);
  }

  onDidChangeTreeData(
    listener: (changedItemIds: TreeItemIndex[]) => void
  ): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  private notifyChange(changedItemIds: TreeItemIndex[] = []) {
    this.listeners.forEach((fn) => fn(changedItemIds));
  }

  private setLoading(loading: boolean) {
    this.onLoadingChange?.(loading);
  }

  /**
   * 加载目录子项。
   * - 同一目录的并发请求合并为一个（返回同一个 Promise）
   * - 强制刷新时丢弃旧请求结果（epoch 机制）
   */
  async loadChildren(parentPath: string, parentIndex: string, force = false): Promise<void> {
    const cacheKey = parentPath || "__root__";

    // 已有进行中的请求：强制刷新则跳过旧 Promise 重新发起，否则复用
    const existing = this.pendingLoads.get(cacheKey);
    if (existing && !force) return existing;

    const myEpoch = (this.dirEpochs.get(cacheKey) || 0) + 1;
    this.dirEpochs.set(cacheKey, myEpoch);
    const promise = (async () => {
      this.setLoading(true);
      try {
        const entries = await this.fetchFn(parentPath);
        // 过期请求：同一目录有更新的请求发起，丢弃本次结果
        if (this.dirEpochs.get(cacheKey) !== myEpoch) return;

        const childIndices: TreeItemIndex[] = [];
        for (const entry of entries) {
          const index = entry.path;
          const treeItem: TreeItem<FileTreeItemData> = {
            index,
            data: {
              name: entry.name,
              path: entry.path,
              isDir: entry.isDir,
              icon: getFileIcon(entry),
            },
            isFolder: entry.isDir,
            children: entry.isDir ? [] : undefined,
          };
          this.items.set(String(index), treeItem);
          childIndices.push(index);
        }

        const parent = this.items.get(String(parentIndex));
        if (parent) {
          parent.children = childIndices;
        }
        // 同时通知 parent 和所有子项，让 UncontrolledTreeEnvironment 一次性写入 currentItems
        this.notifyChange([parentIndex, ...childIndices]);
      } catch (err) {
        if (this.dirEpochs.get(cacheKey) !== myEpoch) return;
        this.onError?.(err instanceof Error ? err.message : "加载目录失败");
      } finally {
        // 仅当自己仍是该目录最新 epoch 时才从 pending 中移除
        if (this.dirEpochs.get(cacheKey) === myEpoch) {
          this.pendingLoads.delete(cacheKey);
        }
        // 仅在没有其他进行中请求时关闭 loading
        if (this.pendingLoads.size === 0) this.setLoading(false);
      }
    })();
    this.pendingLoads.set(cacheKey, promise);
    return promise;
  }

  /**
   * 刷新指定目录：保留其子项的展开状态，仅重新加载该目录内容。
   * 若 parentPath 为空则刷新根目录。
   */
  refreshDir(parentPath: string, parentIndex: string) {
    return this.loadChildren(parentPath, parentIndex, true);
  }

  /**
   * 全量刷新：清除所有已加载内容，配合组件层 key 重建 UncontrolledTreeEnvironment。
   * 已展开的子目录在用户再次展开时会重新拉取。
   */
  refreshAll() {
    this.pendingLoads.clear();
    this.dirEpochs.clear();
    const root = this.items.get("root");
    this.items.clear();
    if (root) {
      root.children = [];
      this.items.set("root", root);
    }
  }

  getItem(path: string): TreeItem<FileTreeItemData> | undefined {
    return this.items.get(path);
  }
}

// ─── 组件 ────────────────────────────────────────────────────────────────────

export function ProjectFileTree({
  projectId,
  request,
  onFileSelect,
  onCreateFile,
  onCreateDir,
  onRename,
  onDelete,
  onAddToChat,
  readOnly,
  refreshRef,
}: ProjectFileTreeProps) {
  const [searchQuery, setSearchQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // 全量刷新时递增，强制 UncontrolledTreeEnvironment 重建以清除内部 currentItems/viewState 残留
  const [treeKey, setTreeKey] = useState(0);
  const [contextMenu, setContextMenu] = useState<{
    x: number;
    y: number;
    path: string;
    name: string;
    isDir: boolean;
  } | null>(null);
  const [copiedPath, setCopiedPath] = useState<string | null>(null);

  const showError = useCallback((message: string) => {
    setError(message);
  }, []);

  const fetchDir = useCallback(
    async (path: string): Promise<FileEntry[]> => {
      const params = new URLSearchParams();
      if (path) params.set("path", path);
      const res = await request<TreeResponse>(
        `/api/projects/${projectId}/fs/tree?${params.toString()}`
      );
      return res.entries || [];
    },
    [projectId, request]
  );

  const provider = useMemo(
    () => new FileTreeProvider(fetchDir, setLoading, showError),
    [fetchDir, showError]
  );

  // 初始加载根目录
  useEffect(() => {
    void provider.loadChildren("", "root");
  }, [provider]);

  // 暴露刷新方法给父组件
  useEffect(() => {
    if (refreshRef) {
      refreshRef.current = () => {
        provider.refreshAll();
        setTreeKey((k) => k + 1);
        void provider.loadChildren("", "root", true);
      };
    }
    return () => {
      if (refreshRef) refreshRef.current = null;
    };
  }, [provider, refreshRef]);

  // 自动清除错误
  useEffect(() => {
    if (!error) return;
    const timer = setTimeout(() => setError(null), 5000);
    return () => clearTimeout(timer);
  }, [error]);

  // 点击外部关闭右键菜单
  useEffect(() => {
    if (!contextMenu) return;
    const close = () => setContextMenu(null);
    const closeOnContextMenu = (e: MouseEvent) => {
      e.preventDefault();
      setContextMenu(null);
    };
    document.addEventListener("click", close);
    document.addEventListener("contextmenu", closeOnContextMenu);
    return () => {
      document.removeEventListener("click", close);
      document.removeEventListener("contextmenu", closeOnContextMenu);
    };
  }, [contextMenu]);

  // 搜索
  const handleSearch = useCallback(async () => {
    const query = searchQuery.trim();
    if (!query) return;
    try {
      const params = new URLSearchParams({ query });
      const res = await request<SearchResponse>(
        `/api/projects/${projectId}/fs/search?${params.toString()}`
      );
      if (res.entries && res.entries.length > 0) {
        const first = res.entries[0];
        if (first) onFileSelect(first.path, first.name);
      } else {
        showError("未找到匹配的文件");
      }
    } catch (err) {
      showError(err instanceof Error ? err.message : "搜索失败");
    }
  }, [searchQuery, projectId, request, onFileSelect, showError]);

  // 刷新
  const handleRefresh = useCallback(() => {
    provider.refreshAll();
    setTreeKey((k) => k + 1);
    void provider.loadChildren("", "root", true);
  }, [provider]);

  return (
    <div className="file-tree-container">
      <div className="file-tree-toolbar">
        <div className="file-tree-search">
          <input
            type="text"
            placeholder="搜索文件..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && !loading && handleSearch()}
          />
        </div>
        {!readOnly && <>
          <button type="button" className="file-tree-action" onClick={() => onCreateFile("")} title="新建文件" aria-label="新建文件"><TreeActionIcon name="new-file" /></button>
          <button type="button" className="file-tree-action" onClick={() => onCreateDir("")} title="新建目录" aria-label="新建目录"><TreeActionIcon name="new-folder" /></button>
        </>}
        <button
          type="button"
          className="file-tree-refresh"
          onClick={handleRefresh}
          title="刷新"
          disabled={loading}
        >
          <TreeActionIcon name="refresh" />
        </button>
      </div>

      {error && (
        <div className="file-tree-error" role="alert">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      <div className="file-tree-body">
        <UncontrolledTreeEnvironment
          key={treeKey}
          dataProvider={provider}
          getItemTitle={(item) => item.data.name}
          viewState={{}}
          canRename={!readOnly}
          canReorderItems={false}
          canDropOnFolder={false}
          canDragAndDrop={false}
          onPrimaryAction={(item) => {
            // item 是 TreeItem<FileTreeItemData> 对象
            if (!item.data.isDir) {
              onFileSelect(item.data.path, item.data.name);
            }
          }}
          onExpandItem={(item) => {
            // item 是 TreeItem<FileTreeItemData> 对象
            if (item.data.isDir) {
              const children = item.children || [];
              // 无子项时加载；已有子项时直接复用（刷新通过右键菜单）
              if (children.length === 0) {
                void provider.loadChildren(item.data.path, String(item.index));
              }
            }
          }}
          renderItemTitle={({ item, title, context }) => {
            const isFocused = context.isSelected || context.isFocused;
            return (
              <div
                className={`file-tree-item ${isFocused ? "selected" : ""}`}
                onContextMenu={(e) => {
                  e.preventDefault();
                  e.stopPropagation();
                  setContextMenu({
                    x: e.clientX,
                    y: e.clientY,
                    path: item.data.path,
                    name: item.data.name,
                    isDir: item.data.isDir,
                  });
                }}
              >
                <span className="file-tree-item-icon">
                  <FileIcon iconKey={item.data.icon} expanded={item.data.isDir && context.isExpanded} size={16} />
                </span>
                <span className="file-tree-item-name">{title}</span>
              </div>
            );
          }}
          renderItemArrow={({ item, context }) => {
            if (!item.data.isDir) return <span className="file-tree-arrow-spacer" />;
            return (
              <span
                className={`file-tree-arrow ${context.isExpanded ? "expanded" : ""}`}
                {...context.arrowProps}
              >
                <svg viewBox="0 0 16 16" aria-hidden="true"><path d="m6 4 4 4-4 4" /></svg>
              </span>
            );
          }}
        >
          <Tree treeId="file-tree" rootItem="root" treeLabel="文件浏览器" />
        </UncontrolledTreeEnvironment>
      </div>

      {/* 右键菜单 */}
      {contextMenu && (
        <div
          className="file-tree-context-menu"
          style={{
            left: Math.min(contextMenu.x, window.innerWidth - 160),
            top: Math.min(contextMenu.y, window.innerHeight - 320),
          }}
        >
          {contextMenu.isDir && !readOnly && (
            <>
              <button
                onClick={() => {
                  onCreateFile(contextMenu.path);
                  setContextMenu(null);
                }}
              >
                新建文件
              </button>
              <button
                onClick={() => {
                  onCreateDir(contextMenu.path);
                  setContextMenu(null);
                }}
              >
                新建目录
              </button>
            </>
          )}
          {contextMenu.isDir && (
            <button
              onClick={() => {
                void provider.refreshDir(contextMenu.path, contextMenu.path || "root");
                setContextMenu(null);
              }}
            >
              刷新目录
            </button>
          )}
          {!contextMenu.isDir && (
            <button
              onClick={() => {
                onFileSelect(contextMenu.path, contextMenu.name);
                setContextMenu(null);
              }}
            >
              打开
            </button>
          )}
          {!readOnly && (
            <button
              onClick={() => {
                onRename(contextMenu.path, contextMenu.name);
                setContextMenu(null);
              }}
            >
              重命名
            </button>
          )}
          {!readOnly && (
            <button
              className="danger"
              onClick={() => {
                onDelete(contextMenu.path, contextMenu.name, contextMenu.isDir);
                setContextMenu(null);
              }}
            >
              删除
            </button>
          )}
          <div className="file-tree-context-menu-separator" />
          <button
            onClick={async () => {
              const ok = await copyToClipboard(contextMenu.path);
              if (!ok) {
                setContextMenu(null);
                return;
              }
              setCopiedPath(contextMenu.path);
              setTimeout(() => {
                setContextMenu(null);
                setCopiedPath(null);
              }, 1000);
            }}
          >
            {copiedPath === contextMenu.path ? "已复制路径" : "复制路径"}
          </button>
          {!contextMenu.isDir && (
            <button
              onClick={() => {
                if (onAddToChat) {
                  onAddToChat(contextMenu.path);
                } else {
                  sessionStorage.setItem(ADD_TO_CHAT_KEY, contextMenu.path);
                  window.location.href = `/projects/${projectId}/conversations?addFile=true`;
                }
                setContextMenu(null);
              }}
            >
              添加到对话
            </button>
          )}
        </div>
      )}
    </div>
  );
}
