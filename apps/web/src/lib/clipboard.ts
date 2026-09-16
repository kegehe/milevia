// 剪贴板写入的统一入口。
//
// 优先用异步 Clipboard API；它在两种情况下不可用，都必须能退回旧方案：
//   1. 非安全上下文（手机端经局域网 http 访问时 `navigator.clipboard` 直接是 undefined）；
//   2. 权限策略拒绝（iframe / 部分浏览器设置）。
// 旧方案依赖当前调用仍处于用户手势中，所以只在异步 API 缺席或抛错后同步执行。
//
// 返回是否写入成功：调用方据此给出「已复制 / 复制失败」反馈，避免点了没反应。

function copyWithLegacyClipboard(content: string): boolean {
  const textarea = document.createElement("textarea");
  textarea.value = content;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.top = "-1000px";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  try {
    textarea.select();
    textarea.setSelectionRange(0, textarea.value.length);
    return document.execCommand("copy");
  } finally {
    textarea.remove();
  }
}

export async function copyToClipboard(content: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(content);
      return true;
    } catch {
      // 权限策略可能拒绝现代 API，而旧方案仍然可用。
    }
  }
  try {
    return copyWithLegacyClipboard(content);
  } catch {
    return false;
  }
}
