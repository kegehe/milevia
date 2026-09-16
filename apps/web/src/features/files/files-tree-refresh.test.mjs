import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 删除 / 新建 / 重命名之后，文件树只能「定点刷新」受影响的那个目录。
// 一旦退回成「清缓存 + 重建 UncontrolledTreeEnvironment」，展开状态会随环境一起被
// 重置，整棵树折叠起来——用户就得重新一层层点回原来的位置才能继续删。
// 这里的回归点互相牵制：定点刷新 → 不重建环境 → 环境里会残留指向已删除条目的
// focusedItem/selectedItems → 必须收敛它们，否则回车/F2 会去操作已经不存在的文件。
const [fileTree, filesPanel] = await Promise.all([
  readFile(new URL("./ProjectFileTree.tsx", import.meta.url), "utf8"),
  readFile(new URL("./FilesPanel.tsx", import.meta.url), "utf8"),
]);

test("刷新入口接受可选目录：传目录走定点刷新，不传才全量重建", () => {
  assert.match(fileTree, /refreshRef\?: React\.MutableRefObject<\(\(dirPath\?: string, preferPath\?: string\) => void\) \| null>;/);
  // 定点分支：只重拉该目录，绝不 refreshAll / 不递增 treeKey
  assert.match(
    fileTree,
    /void provider\s*\.refreshDir\(dirPath, dirPath \|\| "root"\)\s*\.then\(\(\) => settleEnvironmentSelection\(dirPath, preferPath\)\);/
  );
  // 全量分支只应该在 dirPath === undefined 时可达
  const refreshTree = fileTree.match(/const refreshTree = useCallback\(\s*\(dirPath\?: string, preferPath\?: string\) => \{[\s\S]*?\n  \);/)?.[0] ?? "";
  assert.match(refreshTree, /if \(dirPath === undefined\) \{[\s\S]*?setTreeKey\(\(k\) => k \+ 1\);/);
  const targeted = refreshTree.slice(refreshTree.indexOf("refreshDir"));
  assert.doesNotMatch(targeted, /setTreeKey/);
});

test("重新列举目录时保留已加载的子项，并清掉已消失的条目", () => {
  // 复用同一条目对象，子目录已加载的 children 才不会被清空（展开却空白的根因）
  assert.match(fileTree, /const previous = this\.items\.get\(String\(index\)\);[\s\S]*?previous && previous\.isFolder === entry\.isDir\s*\?\s*\{ \.\.\.previous, data, isFolder: entry\.isDir \}/);
  assert.match(fileTree, /private pruneUnreachable\(\) \{/);
  // prune 必须在 if (parent) 之外无条件执行：父目录在请求飞行途中被删掉并被清理时，
  // 本次写进去的子项就是孤儿，关在 if 里就永远清不掉，等路径被重建时会带着旧子项复活。
  const between = fileTree.slice(fileTree.indexOf("if (parent) {"), fileTree.indexOf("this.pruneUnreachable()"));
  const stripped = between.replace(/\/\/[^\n]*/g, "");
  assert.match(stripped, /if \(parent\) \{\s*parent\.children = childIndices;\s*\}\s*$/);
});

test("定点刷新后收敛环境里指向已消失条目的焦点与选中项", () => {
  assert.match(fileTree, /const settleEnvironmentSelection = useCallback\(/);
  assert.match(fileTree, /const alive = \(id: TreeItemIndex\) => provider\.getItem\(String\(id\)\) !== undefined;/);
  // 焦点只在确实失效时才动，且落点必须已存在于环境里，否则 focusItem 会拿到 undefined
  assert.match(fileTree, /if \(state\.focusedItem === undefined \|\| alive\(state\.focusedItem\)\) return;/);
  // 改名后优先把焦点还给新路径，而不是随便落到父目录的第一项
  assert.match(fileTree, /preferPath !== undefined && alive\(preferPath\) \? preferPath : undefined/);
  assert.match(fileTree, /if \(fallback !== undefined && environment\.items\[fallback\] !== undefined\) \{\s*environment\.focusItem\(fallback, TREE_ID, false\);/);
  assert.match(fileTree, /const environmentRef = useRef<TreeEnvironmentRef<FileTreeItemData, never> \| null>\(null\);/);
  assert.match(fileTree, /ref=\{environmentRef\}/);
});

test("provider 重建后按深度升序补拉已展开的空壳目录", () => {
  // 组件侧：根目录拉完补一次；用户展开某个目录后再补一次（残留的更深展开项）
  assert.match(fileTree, /const restoreExpandedDirs = useCallback\(/);
  assert.match(fileTree, /\.then\(\(\) => restoreExpandedDirs\(\)\);/);
  assert.match(fileTree, /\.then\(\(\) => restoreExpandedDirs\(item\.data\.path\)\);/);
  assert.match(fileTree, /const expanded = environmentRef\.current\?\.viewState\[TREE_ID\]\?\.expandedItems \?\? \[\];/);
  // provider 侧：必须排序 + 逐个 await + 只补缓存里已有的目录条目
  assert.match(fileTree, /async restoreExpandedDirs\(expandedPaths: TreeItemIndex\[\], skipPath\?: string\): Promise<void> \{/);
  assert.match(fileTree, /\.sort\(\(a, b\) => a\.split\("\/"\)\.length - b\.split\("\/"\)\.length \|\| a\.localeCompare\(b\)\);/);
  assert.match(fileTree, /if \(!item\?\.isFolder\) continue;/);
  assert.match(fileTree, /if \(\(item\.children\?\.length \?\? 0\) > 0\) continue;/);
  assert.match(fileTree, /await this\.loadChildren\(path, path\);/);
  // 并发（Promise.all）会丢掉深层目录，禁止
  assert.doesNotMatch(fileTree, /restoreExpandedDirs[\s\S]{0,400}?Promise\.all/);
});

test("环境的 items 只增不减，回车必须按数据源校验条目仍然存在", () => {
  assert.match(fileTree, /if \(!entry \|\| entry\.isDir \|\| !provider\.getItem\(entry\.path\)\) return;/);
});

test("删除 / 重命名 / 新建都只刷新受影响目录", () => {
  assert.match(filesPanel, /const treeRefreshRef = useRef<\(\(dirPath\?: string, preferPath\?: string\) => void\) \| null>\(null\);/);
  // 复用 file-model 里已有的 getDirPath，别再手写一份
  assert.match(filesPanel, /import \{[^}]*\bgetDirPath\b[^}]*\} from "\.\/file-model";/);
  assert.match(filesPanel, /setShowDeleteConfirm\(null\);\s*treeRefreshRef\.current\?\.\(getDirPath\(removedPath\)\);/);
  assert.match(filesPanel, /setShowRenameDialog\(null\);[\s\S]{0,200}?treeRefreshRef\.current\?\.\(getDirPath\(newPath\), newPath\);/);
  assert.match(filesPanel, /setShowNewFileDialog\(null\);\s*treeRefreshRef\.current\?\.\(showNewFileDialog\.dirPath\);/);
  // 不留「不带参数 = 整树重建」的旧调用
  assert.doesNotMatch(filesPanel, /treeRefreshRef\.current\?\.\(\s*\);/);
});
