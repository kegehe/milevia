import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Tree,
  TreeItem,
  TreeItemIndex,
  UncontrolledTreeEnvironment,
} from "react-complex-tree";
import type { TreeEnvironmentRef } from "react-complex-tree";
import type { FileEntry, TreeResponse, SearchResponse } from "./file-model";
import { getFileIcon } from "./file-model";
import { FileIcon } from "./FileIcon";

// sessionStorage key：暂存要添加到对话的文件路径
const ADD_TO_CHAT_KEY = "milevia_add_file_to_chat";

// 树 id，用于向环境查询/修改视图状态（展开项、选中项、焦点项）
const TREE_ID = "file-tree";

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
  conversationId?: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onFileSelect: (path: string, name: string) => void;
  onCreateFile: (dirPath: string) => void;
  onCreateDir: (dirPath: string) => void;
  onRename: (path: string, name: string) => void;
  onDelete: (path: string, name: string, isDir: boolean) => void;
  onAddToChat?: (path: string) => void;
  readOnly: boolean;
  // 刷新文件树。传 dirPath 表示只重新拉取该目录的子项（删除/新建/重命名等
  // 定点更新用，保留其它目录的展开状态）；preferPath 是刷新后希望保持焦点的路径
  // （改名后的新路径）。都不传则做一次全量刷新。
  refreshRef?: React.MutableRefObject<((dirPath?: string, preferPath?: string) => void) | null>;
}

interface FileTreeItemData {
  name: string;
  path: string;
  isDir: boolean;
  icon: string;
  /**
   * 这个目录**读不到**（权限不足、或读到一半被删掉）。服务端在批量取树时只把它标出来、
   * 不让整棵树失败（见 control-server 的 readTree），所以界面必须说出来。
   *
   * 不说的话它和"空目录"长得一模一样 —— 用户会以为什么都没有，而真相是"没读到"。
   * 这正是本项目明令禁止的那种写法：把"读不到"写成"没有"。
   */
  unreadable?: boolean;
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

// 具名导出以便单测直接驱动它（与 git-model.ts 导出 CommitHistory 同一惯例）：
// 「重新列举目录时保留已加载子项」「清掉已消失条目」这两条不变量都是纯逻辑，
// 用真实 provider + 假 fetch 测比源码 grep 可靠。
export class FileTreeProvider {
  private items: Map<string, TreeItem<FileTreeItemData>> = new Map();
  private listeners: Set<(changedItemIds: TreeItemIndex[]) => void> = new Set();
  private fetchFn: (path: string) => Promise<FileEntry[]>;
  private onLoadingChange?: (loading: boolean) => void;
  private onError?: (message: string) => void;
  // 记录每个目录正在进行的请求，防止重复请求与旧请求覆盖新数据
  private pendingLoads = new Map<string, Promise<void>>();
  private pendingLoadIDs = new Map<string, number>();
  private nextLoadID = 0;
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
    const loadID = ++this.nextLoadID;
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
          const data: FileTreeItemData = {
            name: entry.name,
            path: entry.path,
            isDir: entry.isDir,
            icon: getFileIcon(entry),
            unreadable: entry.unreadable,
          };
          // 复用目录中仍然存在的条目，保留它已经加载过的子项。否则「刷新父目录」
          // 会把已展开的子目录内容清空（表现为展开却空白，且因为不再是展开动作、
          // onExpandItem 不会再触发，只能靠手动折叠再展开救回来）。
          const previous = this.items.get(String(index));
          const treeItem: TreeItem<FileTreeItemData> =
            previous && previous.isFolder === entry.isDir
              ? { ...previous, data, isFolder: entry.isDir }
              : {
                  index,
                  data,
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
        // 丢弃从根不可达的条目（被删除的文件/目录、重命名后失效的旧子树）。
        // 既避免 items 无限增长，也避免"删掉再用同名新建"时复用回旧的子项。
        //
        // 这里不能只在 parent 存在时清理：父目录如果在本次请求飞行途中被删掉并被
        // prune 移出 map，本次写进去的子项就成了"谁也不指向"的孤儿，之后不会再有人
        // 清理它们，等这个路径被重新创建时就会带着旧子项复活。孤儿不可能可达，
        // 直接清掉即可，下一次 loadChildren 会把它们重新拉回来。
        this.pruneUnreachable();
        // 同时通知 parent 和所有子项，让 UncontrolledTreeEnvironment 一次性写入 currentItems
        this.notifyChange([parentIndex, ...childIndices]);
      } catch (err) {
        if (this.dirEpochs.get(cacheKey) !== myEpoch) return;
        this.onError?.(err instanceof Error ? err.message : "加载目录失败");
      } finally {
        // 不应由过期请求清理后来请求；被 refreshAll 作废但没有后继请求的
        // Promise 则仍需在这里自行清理。
        if (this.pendingLoadIDs.get(cacheKey) === loadID) {
          this.pendingLoads.delete(cacheKey);
          this.pendingLoadIDs.delete(cacheKey);
        }
        // 仅在没有其他进行中请求时关闭 loading
        if (this.pendingLoads.size === 0) this.setLoading(false);
      }
    })();
    this.pendingLoads.set(cacheKey, promise);
    this.pendingLoadIDs.set(cacheKey, loadID);
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
   * 补拉「环境认为已展开、但缓存里还没有内容」的目录。
   *
   * 用在 provider 被重建（切换项目/会话）之后：环境的展开状态还在，但这些目录在新缓存里
   * 只是空壳（children = []），界面就成了"展开着却空白"，而因为不再是展开动作，
   * onExpandItem 不会补触发。
   *
   * 三条硬约束：
   * - **按深度升序、逐个 await**。深层目录的条目要等它父目录的列举结果落进缓存后才存在，
   *   并发跑或顺序反了都会被 loadChildren 里的孤儿清理丢掉，等于没补。
   * - **只补缓存里已有的目录条目**。查不到说明磁盘上已经删掉、或它的父目录没列出来
   *   （例如它的祖先被折叠着），这时补拉会白跑一趟甚至报 404。
   * - **已有内容的目录跳过**，否则每次展开都会把它重复拉一遍。
   */
  async restoreExpandedDirs(expandedPaths: TreeItemIndex[], skipPath?: string): Promise<void> {
    const ordered = [...new Set(expandedPaths.map(String))]
      .filter((path) => path && path !== skipPath)
      .sort((a, b) => a.split("/").length - b.split("/").length || a.localeCompare(b));
    for (const path of ordered) {
      const item = this.items.get(path);
      if (!item?.isFolder) continue;
      if ((item.children?.length ?? 0) > 0) continue;
      await this.loadChildren(path, path);
    }
  }

  /**
   * 从根开始标记可达条目，未标记的（已删除 / 改名后失效）从缓存里移除。
   * 只能清理已经被父目录列举过、但如今不再挂载的条目。
   */
  private pruneUnreachable() {
    const reachable = new Set<string>();
    const walk = (index: string) => {
      if (reachable.has(index)) return;
      reachable.add(index);
      const item = this.items.get(index);
      for (const child of item?.children ?? []) {
        walk(String(child));
      }
    };
    walk("root");
    for (const key of [...this.items.keys()]) {
      if (!reachable.has(key)) this.items.delete(key);
    }
  }

  /**
   * 全量刷新：清除所有已加载内容，配合组件层 key 重建 UncontrolledTreeEnvironment。
   * 已展开的子目录在用户再次展开时会重新拉取。
   */
  refreshAll() {
    // 不能清空 epoch。旧请求若为 1，而新请求在清空后也变为 1，就会重新
    // 获得写入资格。递增所有已知目录的 epoch 可同时作废仍在飞行的请求。
    for (const key of new Set([...this.dirEpochs.keys(), ...this.pendingLoads.keys()])) {
      this.dirEpochs.set(key, (this.dirEpochs.get(key) || 0) + 1);
    }
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

/**
 * 把一次取树的"完整性"折成一句话。
 *
 * 两个来源必须分开说：`truncated` 是**这次没取全**（条目超配额），
 * `skippedDirs` 是**按约定跳过的依赖目录**（node_modules 等，属预期行为）。
 * 合成一句"部分内容已隐藏"会让用户分不清"项目就是这样"还是"工具没给我看全"。
 * 两者都没有时返回 null —— 不摆一句"全部显示"占位。
 */
function treeSummaryText(res: TreeResponse): string | null {
  const parts: string[] = [];
  if (res.truncated) parts.push("条目太多，只显示了前面一部分");
  const skipped = Number(res.skippedDirs) || 0;
  if (skipped > 0) parts.push(`已隐藏 ${skipped} 个依赖目录`);
  return parts.length ? parts.join(" · ") : null;
}

export function ProjectFileTree({
  projectId,
  conversationId,
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
  /**
   * 这次取树"是不是全给了"的一句话说明（手机端一次多拿几层时才可能出现）。
   *
   * 它存在的理由：服务端会按忽略名单跳掉 node_modules 这类目录、也会在条目超配额时裁剪。
   * 这两件事**必须让用户看见** —— 否则他看到的就是一棵"少了几个目录"的树，
   * 而以为那就是项目全貌。跳依赖目录与"没取全"是两件事，措辞也分开。
   */
  const [treeSummary, setTreeSummary] = useState<string | null>(null);
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
  const environmentRef = useRef<TreeEnvironmentRef<FileTreeItemData, never> | null>(null);

  const showError = useCallback((message: string) => {
    setError(message);
  }, []);

  const fetchDir = useCallback(
    async (path: string): Promise<FileEntry[]> => {
      const params = new URLSearchParams();
      if (path) params.set("path", path);
		if (conversationId) params.set("conversationId", conversationId);
      const res = await request<TreeResponse>(
        `/api/projects/${projectId}/fs/tree?${params.toString()}`
      );
      // 只有根那一次带 depth（手机端），也就只有它知道"这次是不是取全了"。
      // 子目录是命中适配器缓存的切片，读它们的 truncated/skippedDirs 没有意义。
      if (!path) setTreeSummary(treeSummaryText(res));
      return res.entries || [];
    },
    [projectId, request, conversationId]
  );

  const provider = useMemo(
    () => new FileTreeProvider(fetchDir, setLoading, showError),
    [fetchDir, showError]
  );

  // 补拉「环境认为已展开、但缓存里还没内容」的目录，见 provider.restoreExpandedDirs。
  const restoreExpandedDirs = useCallback(
    async (skipPath?: string) => {
      const expanded = environmentRef.current?.viewState[TREE_ID]?.expandedItems ?? [];
      if (expanded.length === 0) return;
      await provider.restoreExpandedDirs(expanded, skipPath);
    },
    [provider]
  );

  // 初始加载根目录。provider 被重建（切换项目/会话）时同样走这里：新缓存里只有根目录，
  // 之前展开过的目录会变成空壳，所以根目录拉完再按展开状态补拉一次。
  useEffect(() => {
    void provider
      .loadChildren("", "root")
      .then(() => restoreExpandedDirs());
  }, [provider, restoreExpandedDirs]);

  /**
   * 收敛环境的焦点与选中项。
   *
   * 定点刷新不会重建环境，因此删除/改名之后 viewState 里可能残留指向已不存在条目的
   * focusedItem / selectedItems；而 react-complex-tree 的热键是拿它直接查
   * environment.items 的（回车 → onPrimaryAction、F2 → 重命名），命中环境里还留着的
   * 旧条目就会去打开 / 重命名一个已经被删掉的文件。这里把它们挪到仍然存在的条目上。
   *
   * 延时到宏任务：环境写入新条目是异步的（onDidChangeTreeData → getTreeItems →
   * writeItems），必须等它落地后再改视图状态，否则拿到的是还没更新的 items。
   */
  const settleEnvironmentSelection = useCallback(
    (dirPath: string, preferPath?: string) => {
      window.setTimeout(() => {
        const environment = environmentRef.current;
        const state = environment?.viewState[TREE_ID];
        if (!environment || !state) return;
        // 环境里的 items 只增不减，判存活要以数据源为准
        const alive = (id: TreeItemIndex) => provider.getItem(String(id)) !== undefined;
        const selected = state.selectedItems ?? [];
        const keptSelected = selected.filter(alive);
        if (keptSelected.length !== selected.length) {
          environment.selectItems(keptSelected, TREE_ID);
        }
        // 只在焦点确实指向已消失的条目时才动它；没有焦点时交给环境自己初始化。
        if (state.focusedItem === undefined || alive(state.focusedItem)) return;
        // 落点：改名后的新路径（改名时传进来）→ 仍被选中的条目 →
        // 刚刷新的目录里第一个条目 → 该目录本身
        const parentIndex = dirPath || "root";
        const fallback =
          (preferPath !== undefined && alive(preferPath) ? preferPath : undefined) ??
          keptSelected[0] ??
          provider.getItem(parentIndex)?.children?.[0] ??
          (dirPath || undefined);
        if (fallback !== undefined && environment.items[fallback] !== undefined) {
          environment.focusItem(fallback, TREE_ID, false);
        }
      }, 0);
    },
    [provider]
  );

  /**
   * 刷新文件树：
   * - 传 dirPath：只重新拉取该目录的子项。展开状态由 UncontrolledTreeEnvironment
   *   自己保管，只要不重建它，其余目录的展开状态与滚动位置都不会丢。
   *   preferPath 是刷新后希望保持焦点的路径（如改名后的新路径）。
   * - 不传：全量刷新（工具栏「刷新」），清缓存并重建环境，展开状态会重置。
   */
  const refreshTree = useCallback(
    (dirPath?: string, preferPath?: string) => {
      if (dirPath === undefined) {
        provider.refreshAll();
        setTreeKey((k) => k + 1);
        void provider.loadChildren("", "root", true);
        return;
      }
      void provider
        .refreshDir(dirPath, dirPath || "root")
        .then(() => settleEnvironmentSelection(dirPath, preferPath));
    },
    [provider, settleEnvironmentSelection]
  );

  // 暴露刷新方法给父组件
  useEffect(() => {
    if (refreshRef) {
      refreshRef.current = refreshTree;
    }
    return () => {
      if (refreshRef) refreshRef.current = null;
    };
  }, [provider, refreshRef, refreshTree]);

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
		if (conversationId) params.set("conversationId", conversationId);
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
  }, [searchQuery, projectId, request, onFileSelect, showError, conversationId]);

  // 刷新
  const handleRefresh = useCallback(() => {
    refreshTree();
  }, [refreshTree]);

  return (
    <div className="file-tree-container">
      <div className="file-tree-toolbar">        <div className="file-tree-search">
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

      {/* 取树时被隐藏/裁掉的部分要说出来。看不到这句的用户会以为这棵树就是项目全貌，
          而实际上 node_modules 这类目录被跳过了。 */}
      {treeSummary && <p className="file-tree-summary" role="status">{treeSummary}</p>}

      <div className="file-tree-body">
        <UncontrolledTreeEnvironment
          key={treeKey}
          ref={environmentRef}
          dataProvider={provider}
          // 文件侧栏较窄，显式保留清晰的层级缩进。
          renderDepthOffset={16}
          getItemTitle={(item) => item.data.name}
          viewState={{}}
          canRename={!readOnly}
          canReorderItems={false}
          canDropOnFolder={false}
          canDragAndDrop={false}
          onPrimaryAction={(item) => {
            // 环境里的 items 只增不减：被删掉的条目可能还留着旧引用，
            // 这里以数据源为准，避免回车打开一个已经不存在的文件。
            const entry = item?.data;
            if (!entry || entry.isDir || !provider.getItem(entry.path)) return;
            onFileSelect(entry.path, entry.name);
          }}
          onExpandItem={(item) => {
            // item 是 TreeItem<FileTreeItemData> 对象
            if (item.data.isDir) {
              const children = item.children || [];
              // 无子项时加载；已有子项时直接复用（刷新通过右键菜单）
              if (children.length === 0) {
                void provider
                  .loadChildren(item.data.path, String(item.index))
                  // 展开的目录里可能还记着更深的展开项（曾经展开过后折叠了祖先，
                  // 环境里仍留在 expandedItems 里），顺手把那些空壳一起补上。
                  .then(() => restoreExpandedDirs(item.data.path));
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
                {/* 读不到的目录必须说出来：不说它就和空目录长得一模一样。
                    文字而不是图标 —— 一个没有图例的小角标在手机上等于没写。
                    ⚠️ 解释只放在**这个角标**里，不要塞进 `getItemTitle`：
                    那个返回值同时被当成行内可见标签用，塞进去会变成
                    「locked（这个目录读不到）  读不到」——同一句话说两遍。 */}
                {item.data.unreadable && <span className="file-tree-item-unreadable" title="这个目录读不到">读不到</span>}
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
          <Tree treeId={TREE_ID} rootItem="root" treeLabel="文件浏览器" />
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
                refreshTree(contextMenu.path);
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
