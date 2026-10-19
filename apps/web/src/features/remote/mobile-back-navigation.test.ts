import assert from "node:assert/strict";
import { test } from "node:test";
import { resolveMobileBackIntent, type MobileBackIntent, type MobileBackState } from "./mobile-back-navigation";

const emptyState = (): MobileBackState => ({
  blocked: false,
  headerMenuOpen: false,
  shortcutConfirmOpen: false,
  taskEditorOpen: false,
  taskDeleteOpen: false,
  taskCreateOpen: false,
  newConversationOpen: false,
  unbindConfirmOpen: false,
  stopConfirmOpen: false,
  aliasEditorOpen: false,
  devicesOpen: false,
  scannerOpen: false,
  optimizationOpen: false,
  tasksOpen: false,
  messageActionsOpen: false,
  conversationHistoryOpen: false,
  pairingOpen: false,
  inMobileConversation: false,
  filesOpen: false,
  gitOpen: false,
});

const nativeOrder: Array<[keyof MobileBackState, MobileBackIntent]> = [
  ["blocked", "blocked"],
  ["headerMenuOpen", "close-header-menu"],
  ["shortcutConfirmOpen", "close-shortcut-confirm"],
  ["taskEditorOpen", "close-task-editor"],
  ["taskDeleteOpen", "close-task-delete"],
  ["taskCreateOpen", "close-task-create"],
  ["newConversationOpen", "close-new-conversation"],
  ["unbindConfirmOpen", "close-unbind-confirm"],
  ["stopConfirmOpen", "close-stop-confirm"],
  ["aliasEditorOpen", "close-alias-editor"],
  ["devicesOpen", "close-devices"],
  ["scannerOpen", "close-scanner"],
  ["optimizationOpen", "close-optimization"],
  ["tasksOpen", "close-tasks"],
  ["messageActionsOpen", "close-message-actions"],
  ["conversationHistoryOpen", "close-conversation-history"],
  ["pairingOpen", "close-pairing"],
];

test("原生返回按既定优先级关闭每一层", () => {
  for (let index = 0; index < nativeOrder.length; index += 1) {
    const state = emptyState();
    state.inMobileConversation = true;
    for (const [field] of nativeOrder.slice(index)) state[field] = true;
    const [, expected] = nativeOrder[index];
    assert.equal(resolveMobileBackIntent("native", state), expected);
  }
  assert.equal(resolveMobileBackIntent("native", { ...emptyState(), inMobileConversation: true, filesOpen: true }), "delegate-files-back");
  assert.equal(resolveMobileBackIntent("native", { ...emptyState(), inMobileConversation: true, gitOpen: true }), "delegate-git-back");
  assert.equal(resolveMobileBackIntent("native", { ...emptyState(), inMobileConversation: true }), "leave-conversation");
  assert.equal(resolveMobileBackIntent("native", emptyState()), "unhandled");
});

test("页内返回只处理它实际可触达的层级", () => {
  assert.equal(resolveMobileBackIntent("page", { ...emptyState(), optimizationOpen: true, inMobileConversation: true, filesOpen: true }), "close-optimization");
  assert.equal(resolveMobileBackIntent("page", { ...emptyState(), headerMenuOpen: true, pairingOpen: true, inMobileConversation: true, filesOpen: true }), "delegate-files-back");
  assert.equal(resolveMobileBackIntent("page", { ...emptyState(), inMobileConversation: true, filesOpen: true, gitOpen: true }), "delegate-files-back");
  assert.equal(resolveMobileBackIntent("page", { ...emptyState(), inMobileConversation: true, gitOpen: true }), "delegate-git-back");
  assert.equal(resolveMobileBackIntent("page", { ...emptyState(), inMobileConversation: true }), "leave-conversation");
  assert.equal(resolveMobileBackIntent("page", emptyState()), "unhandled");
});
