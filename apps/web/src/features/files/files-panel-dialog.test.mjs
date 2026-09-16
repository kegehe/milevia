import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// FilesPanel 的四个弹窗（新建 / 重命名 / 删除确认 / 放弃未保存的更改，全部是内联 JSX）
// 共用同一段默认焦点与键鼠逻辑。默认焦点的选择顺序、data-autofocus 标记、以及卸载时的
// 焦点归还时序三者互相牵制，任何一处回退都会让「回车」变成「取消」或让弹窗关掉又被重新
// 打开，所以在这里做源码级回归。
const source = await readFile(new URL("./FilesPanel.tsx", import.meta.url), "utf8");

test("默认焦点顺序：data-autofocus → 输入框 → 按钮", () => {
  assert.match(source, /dialog\?\.querySelector<HTMLElement>\("\[data-autofocus\]:not\(:disabled\)"\)\s*\?\?/);
  assert.match(source, /dialog\?\.querySelector<HTMLElement>\("input:not\(:disabled\)"\)\s*\?\?/);
  assert.match(source, /dialog\?\.querySelector<HTMLElement>\("button:not\(:disabled\)"\)/);
  // 合并成一个 "input, button" 选择器会按文档序命中标题栏右上角的关闭按钮，
  // 默认焦点被抢走，回车就变成了取消（2026-09-11 修复的原始 bug）。
  assert.doesNotMatch(source, /querySelector<HTMLElement>\("input:not\(:disabled\), button:not\(:disabled\)"\)/);
});

test("删除确认弹窗的默认焦点是「删除」按钮", () => {
  assert.match(source, /<button type="button" className="danger" data-autofocus onClick=\{submitDelete\}>/);
});

test("弹窗卸载时的焦点归还延后一帧", () => {
  assert.match(
    source,
    /const opener = dialogOpenerRef\.current;\s*dialogOpenerRef\.current = null;\s*requestAnimationFrame\(\(\) => \{\s*if \(opener\?\.isConnected\) opener\.focus\(\);/
  );
  // 同步归还焦点会被回车那次 keydown 的默认动作再点一次触发按钮，
  // 表现是「回车提交成功后弹窗立刻又弹出来」。
  assert.doesNotMatch(source, /dialogOpenerRef\.current\?\.focus\(\);/);
});

test("删除提交防重复：连按回车 / 双击不会发两次请求", () => {
  assert.match(source, /const deleteInFlightRef = useRef\(false\);/);
  assert.match(source, /if \(!showDeleteConfirm \|\| deleteInFlightRef\.current\) return;/);
  assert.match(source, /finally \{\s*deleteInFlightRef\.current = false;\s*\}/);
});

test("「放弃未保存的更改」也纳入 activeDialog，默认焦点给「取消」", () => {
  // 纳入 activeDialog 才有 ESC 关闭与 Tab 焦点陷阱
  assert.match(source, /pendingDiscard \? "discard" : null/);
  assert.match(source, /<section ref=\{dialogRef\} className="files-dialog" role="dialog" aria-modal="true" aria-labelledby="discard-unsaved-title"/);
  // ESC / 点遮罩 = 取消放弃（未保存的编辑留着）
  assert.match(source, /const closeActiveDialog = useCallback\(\(\) => \{[\s\S]*?setPendingDiscard\(null\);[\s\S]*?\}, \[\]\);/);
  assert.match(source, /<div className="files-dialog-backdrop" onClick=\{closeActiveDialog\}/);
  // 默认焦点必须是安全的「取消」，不能被破坏性的「放弃更改」抢走
  assert.match(source, /<button type="button" data-autofocus onClick=\{closeActiveDialog\}>取消<\/button>/);
  assert.doesNotMatch(source, /data-autofocus[^>]*>放弃更改</);
});

test("三个提交回调都带上 workspace 参数依赖（不会用旧的 conversationId 发请求）", () => {
  // withWorkspace 必须是稳定的，否则写进依赖列表毫无意义
  assert.match(source, /const withWorkspace = useCallback\([\s\S]*?\[workspaceQuery\]\s*\);/);
  assert.match(source, /\}, \[showDeleteConfirm, projectId, request, withWorkspace\]\);/);
  assert.match(source, /\}, \[showNewFileDialog, newFileName, projectId, request, withWorkspace\]\);/);
  assert.match(source, /\}, \[showRenameDialog, renameValue, projectId, request, withWorkspace\]\);/);
});
