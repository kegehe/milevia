export type OAuthFlowResult = { ok: boolean; error?: string };
export type OAuthFlowStatus = { status?: string; error?: string };

export async function waitForOAuthFlowResult(
  readStatus: () => Promise<OAuthFlowStatus>,
  signal?: AbortSignal,
  intervalMs = 5000,
  // 后端 mcpOAuthFlowTTL = 10 分钟。前端等待必须覆盖整个有效期，否则用户在第 6 分钟
  // 完成浏览器授权时前端已经放弃、向导还会把刚建的 server 删掉。
  attempts = 120,
): Promise<OAuthFlowResult> {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    await new Promise<void>((resolve) => {
      if (signal?.aborted) { resolve(); return; }
      const finish = () => {
        clearTimeout(timer);
        signal?.removeEventListener("abort", finish);
        resolve();
      };
      const timer = setTimeout(finish, intervalMs);
      signal?.addEventListener("abort", finish, { once: true });
    });
    if (signal?.aborted) return { ok: false, error: "已取消授权。" };
    let status: OAuthFlowStatus;
    try {
      status = await readStatus();
    } catch {
      return { ok: false, error: signal?.aborted ? "已取消授权。" : "授权状态读取失败，请重新发起。" };
    }
    if (signal?.aborted) return { ok: false, error: "已取消授权。" };
    if (status.status === "done") return { ok: true };
    if (status.status === "error" || status.status === "canceled") {
      return { ok: false, error: status.error || "授权失败" };
    }
  }
  return { ok: false, error: "等待授权超时。" };
}
