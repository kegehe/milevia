// ── 向后兼容：从 @milevia/sdk 重导出运行时 API ────────────────────────────
// 所有实现已迁移到 packages/sdk/src/index.ts，
// 此处保持原有导出签名，现有代码无需修改。

export type { DesktopRuntimeConfig } from "@milevia/sdk";
export {
  apiURL,
  createWebSocket,
  getDesktopRuntime,
  getPlatform,
  isDesktop,
  isWeb,
  sessionHeaders,
} from "@milevia/sdk";
