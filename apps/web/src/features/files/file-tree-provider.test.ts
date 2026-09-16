import assert from "node:assert/strict";
import test from "node:test";
import { FileTreeProvider } from "./ProjectFileTree";
import type { FileEntry } from "./file-model";

// FileTreeProvider 的纯逻辑回归：不碰 React、不碰网络，直接驱动真实 provider。
// 这里守的两条不变量都是"看着像界面问题、实际出在数据层"：
//   1. 重新列举某个目录时，它下面**已加载**的子目录内容不能被清空
//      （清空 = 界面"展开着却空白"，而且因为不再是展开动作，onExpandItem 不会补触发）；
//   2. 已消失的条目要从缓存里清掉（否则"删掉再用同名新建"会带出旧子项）。

type Fs = Map<string, FileEntry>;

function entry(path: string, isDir: boolean): FileEntry {
  return { name: path.split("/").pop() ?? path, path, isDir };
}

/** 假文件系统：只按 path 前缀返回直接子项，可选按目录设置延迟（用来制造竞态）。 */
function makeFs(initial: string[], delayFor: (path: string) => number = () => 0) {
  const files: Fs = new Map();
  const put = (spec: string) => {
    const isDir = spec.endsWith("/");
    const path = isDir ? spec.slice(0, -1) : spec;
    files.set(path, entry(path, isDir));
  };
  initial.forEach(put);

  const listNow = (path: string): FileEntry[] => {
    const prefix = path ? `${path}/` : "";
    return [...files.values()]
      .filter((item) => item.path.startsWith(prefix) && !item.path.slice(prefix.length).includes("/"))
      .sort((a, b) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1));
  };

  const fetchDir = async (path: string): Promise<FileEntry[]> => {
    // 先取快照、再延迟返回：对应真实请求里"服务端已经读过目录，响应还在路上"的状态。
    // 反过来（延迟后再读）会让飞行中的请求返回最新内容，竞态用例就永远测不到东西。
    const snapshot = listNow(path);
    const delay = delayFor(path);
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    return snapshot;
  };

  const removeAtOrBelow = (path: string) => {
    for (const key of [...files.keys()]) {
      if (key === path || key.startsWith(`${path}/`)) files.delete(key);
    }
  };

  return { files, fetchDir, removeAtOrBelow };
}

/** "src/" 表示目录，"src/deep/c.txt" 表示文件。 */
const BASE = ["src/", "src/deep/", "src/deep/c.txt", "src/deep/d.txt"];

function childPaths(provider: FileTreeProvider, path: string) {
  return provider.getItem(path)?.children?.map(String);
}

test("重新列举父目录时，已加载的子目录内容不会被清空", async () => {
  const fs = makeFs(BASE);
  const provider = new FileTreeProvider(fs.fetchDir);
  await provider.loadChildren("", "root");
  await provider.loadChildren("src", "src");
  await provider.loadChildren("src/deep", "src/deep");
  assert.deepEqual(childPaths(provider, "src/deep"), ["src/deep/c.txt", "src/deep/d.txt"]);

  // 删掉一个文件后刷新它所在的目录：列表更新，另一个文件还在
  fs.removeAtOrBelow("src/deep/c.txt");
  await provider.refreshDir("src/deep", "src/deep");
  assert.deepEqual(childPaths(provider, "src/deep"), ["src/deep/d.txt"]);

  // 再刷新**上层**目录：src/deep 已加载的子项必须保留（这正是"展开着却空白"的根因）
  await provider.refreshDir("src", "src");
  assert.deepEqual(childPaths(provider, "src/deep"), ["src/deep/d.txt"]);
  assert.deepEqual(childPaths(provider, "src"), ["src/deep"]);
});

test("重新列举时清掉已消失的条目，同名重建不会带出旧子项", async () => {
  const fs = makeFs(BASE);
  const provider = new FileTreeProvider(fs.fetchDir);
  await provider.loadChildren("", "root");
  await provider.loadChildren("src", "src");
  await provider.loadChildren("src/deep", "src/deep");

  // 整个 src 被删掉
  fs.removeAtOrBelow("src");
  await provider.refreshDir("", "root");
  assert.equal(provider.getItem("src"), undefined);
  assert.equal(provider.getItem("src/deep"), undefined);
  assert.equal(provider.getItem("src/deep/c.txt"), undefined);

  // 同名重建：必须是干净的新目录。若没有 prune，会复用旧的 src/deep 条目并带回
  // 已经不存在的 src/deep/c.txt（幽灵子项）。
  fs.files.set("src", entry("src", true));
  fs.files.set("src/deep", entry("src/deep", true));
  fs.files.set("src/deep/n.txt", entry("src/deep/n.txt", false));
  await provider.refreshDir("", "root");
  await provider.refreshDir("src", "src");
  assert.deepEqual(childPaths(provider, "src"), ["src/deep"]);
  assert.deepEqual(childPaths(provider, "src/deep"), []);

  // 展开后应当只有新文件
  await provider.loadChildren("src/deep", "src/deep");
  assert.deepEqual(childPaths(provider, "src/deep"), ["src/deep/n.txt"]);
});

test("父目录在请求飞行途中被删掉，落地结果不会留下孤儿条目", async () => {
  const fs = makeFs(BASE, (path) => (path === "src/deep" ? 30 : 0));
  const provider = new FileTreeProvider(fs.fetchDir);
  await provider.loadChildren("", "root");
  await provider.loadChildren("src", "src");

  // 展开 src/deep 的请求还在飞，此时 src 整个被删掉并刷新根
  const deepLoad = provider.loadChildren("src/deep", "src/deep");
  fs.removeAtOrBelow("src");
  await provider.refreshDir("", "root");
  await deepLoad;

  // src/deep 的父目录已经被清掉，它自己写进来的子项也不可能可达 → 必须一并清掉，
  // 否则这些孤儿会一直躺在缓存里，等这条路径被重建时带着旧子项复活。
  assert.equal(provider.getItem("src/deep"), undefined);
  assert.equal(provider.getItem("src/deep/c.txt"), undefined);
  assert.equal(provider.getItem("src/deep/d.txt"), undefined);
});

test("全量刷新只保留根节点，其余条目在下次展开时重新拉取", async () => {
  const fs = makeFs(BASE);
  const provider = new FileTreeProvider(fs.fetchDir);
  await provider.loadChildren("", "root");
  await provider.loadChildren("src", "src");
  await provider.loadChildren("src/deep", "src/deep");

  provider.refreshAll();
  assert.equal(provider.getItem("src"), undefined);
  assert.deepEqual(childPaths(provider, "root"), []);

  await provider.loadChildren("", "root", true);
  assert.deepEqual(childPaths(provider, "root"), ["src"]);
});

// ─── provider 重建（切换项目/会话）后补拉已展开目录 ──────────────────────────
// 新缓存里只有根目录，环境却仍记着展开状态 → 那些目录是空壳，界面"展开着却空白"。

test("补拉已展开的空壳目录，必须按深度升序（否则深层目录会被丢掉）", async () => {
  const fs = makeFs(BASE);
  const rebuilt = new FileTreeProvider(fs.fetchDir);
  await rebuilt.loadChildren("", "root");
  // 刚重建时就是这个样子：根目录列出来了，src 只是个空壳（这正是"展开着却空白"的状态）
  assert.deepEqual(childPaths(rebuilt, "src"), []);

  // 环境里的展开项顺序是任意的，这里故意把深的放前面：
  // 先处理 src/deep 时它在缓存里还不存在（父目录没列出来）→ 跳过；
  // 处理完 src 之后 src/deep 才出现，所以必须靠"按深度升序 + 逐个 await"才能补上。
  await rebuilt.restoreExpandedDirs(["src/deep", "src"]);
  assert.deepEqual(childPaths(rebuilt, "src"), ["src/deep"]);
  assert.deepEqual(childPaths(rebuilt, "src/deep"), ["src/deep/c.txt", "src/deep/d.txt"]);
});

test("补拉只针对空壳目录：已有内容 / 缓存里没有的路径 / skipPath 都不发请求", async () => {
  const fs = makeFs(BASE);
  const fetches: string[] = [];
  const countingFetch = (path: string) => {
    fetches.push(path);
    return fs.fetchDir(path);
  };
  const provider = new FileTreeProvider(countingFetch);
  await provider.loadChildren("", "root");
  await provider.loadChildren("src", "src");
  await provider.loadChildren("src/deep", "src/deep");

  fetches.length = 0;
  await provider.restoreExpandedDirs(["", "root", "src", "src/deep", "src/ghost"], "src/deep");
  assert.deepEqual(fetches, []);
});

test("父目录没列出来的深层展开项会被跳过，不会白跑一趟（可能是已删目录）", async () => {
  const fs = makeFs(BASE);
  const fetches: string[] = [];
  const countingFetch = (path: string) => {
    fetches.push(path);
    return fs.fetchDir(path);
  };
  const provider = new FileTreeProvider(countingFetch);
  await provider.loadChildren("", "root");

  fetches.length = 0;
  // "src/deep" 只是环境里残留的展开项（祖先被折叠着），缓存里还没有它的条目
  await provider.restoreExpandedDirs(["src/deep"]);
  assert.deepEqual(fetches, []);
  assert.equal(provider.getItem("src/deep"), undefined);
});
