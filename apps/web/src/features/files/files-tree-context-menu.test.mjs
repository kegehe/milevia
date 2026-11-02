import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 右键菜单必须覆盖**整条**文件行，而不只是文字那一截。
//
// react-complex-tree 每行的结构是：
//   <div data-rct-item-container><箭头/><button data-rct-item-id>标题</button></div>
// 我们渲染的 .file-tree-item 只是按钮里的一段内容，宽度只到文字末尾 —— 按钮的剩余宽度、
// 展开箭头、行的内边距与层级缩进全在它外面。所以 handler 一旦挂在标题上，右键落在
// 文字右边一点点、行尾空白或箭头处就完全没有反应（用户报的就是这个）。
// 修法是把 handler 挂到整棵树容器上，再按被点中的行反查条目。
const fileTree = await readFile(new URL("./ProjectFileTree.tsx", import.meta.url), "utf8");

test("右键菜单挂在树容器上，按被点中的整行反查条目", () => {
  const body = fileTree.slice(fileTree.indexOf('className="file-tree-body"'));
  const handler = body.slice(0, body.indexOf("<UncontrolledTreeEnvironment"));
  assert.match(handler, /onContextMenu=/);
  // 整行 = [data-rct-item-container]，含箭头、内边距与缩进
  assert.match(handler, /closest\("\[data-rct-item-container\]"\)/);
  // 行的条目 id 挂在里层的按钮上，再以数据源为准取条目（环境里的条目只增不减）
  assert.match(handler, /querySelector\("\[data-rct-item-id\]"\)/);
  assert.match(handler, /provider\.getItem\(id\)\?\.data/);
});

test("标题元素不再自带右键 handler", () => {
  const title = fileTree.slice(fileTree.indexOf("renderItemTitle="), fileTree.indexOf("renderItemArrow="));
  assert.doesNotMatch(title, /onContextMenu/);
});
