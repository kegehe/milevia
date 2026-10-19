export type MobileBackEntry = "native" | "page";

export type MobileBackIntent =
  | "blocked"
  | "close-header-menu"
  | "close-shortcut-confirm"
  | "close-task-editor"
  | "close-task-delete"
  | "close-task-create"
  | "close-new-conversation"
  | "close-unbind-confirm"
  | "close-stop-confirm"
  | "close-alias-editor"
  | "close-devices"
  | "close-scanner"
  | "close-optimization"
  | "close-tasks"
  | "close-message-actions"
  | "close-conversation-history"
  | "close-pairing"
  | "delegate-files-back"
  | "delegate-git-back"
  | "leave-conversation"
  | "unhandled";

export interface MobileBackState {
  blocked: boolean;
  headerMenuOpen: boolean;
  shortcutConfirmOpen: boolean;
  taskEditorOpen: boolean;
  taskDeleteOpen: boolean;
  taskCreateOpen: boolean;
  newConversationOpen: boolean;
  unbindConfirmOpen: boolean;
  stopConfirmOpen: boolean;
  aliasEditorOpen: boolean;
  devicesOpen: boolean;
  scannerOpen: boolean;
  optimizationOpen: boolean;
  tasksOpen: boolean;
  messageActionsOpen: boolean;
  conversationHistoryOpen: boolean;
  pairingOpen: boolean;
  inMobileConversation: boolean;
  filesOpen: boolean;
  gitOpen: boolean;
}

export function resolveMobileBackIntent(entry: MobileBackEntry, state: MobileBackState): MobileBackIntent {
  if (entry === "native") {
    if (state.blocked) return "blocked";
    if (state.headerMenuOpen) return "close-header-menu";
    if (state.shortcutConfirmOpen) return "close-shortcut-confirm";
    if (state.taskEditorOpen) return "close-task-editor";
    if (state.taskDeleteOpen) return "close-task-delete";
    if (state.taskCreateOpen) return "close-task-create";
    if (state.newConversationOpen) return "close-new-conversation";
    if (state.unbindConfirmOpen) return "close-unbind-confirm";
    if (state.stopConfirmOpen) return "close-stop-confirm";
    if (state.aliasEditorOpen) return "close-alias-editor";
    if (state.devicesOpen) return "close-devices";
    if (state.scannerOpen) return "close-scanner";
    if (state.optimizationOpen) return "close-optimization";
    if (state.tasksOpen) return "close-tasks";
    if (state.messageActionsOpen) return "close-message-actions";
    if (state.conversationHistoryOpen) return "close-conversation-history";
    if (state.pairingOpen) return "close-pairing";
  } else if (state.optimizationOpen) {
    return "close-optimization";
  }

  if (state.inMobileConversation && state.filesOpen) return "delegate-files-back";
  if (state.inMobileConversation && state.gitOpen) return "delegate-git-back";
  if (state.inMobileConversation) return "leave-conversation";
  return "unhandled";
}
