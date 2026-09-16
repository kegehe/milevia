// 真机验收脚本：对着本机跑起来的 control-server 走一遍模型选择接口。
// 只做 HTTP 调用，不发消息、不拉起 CLI（那部分由 Go 侧 stub runner 测试覆盖）。
const base = "http://127.0.0.1:18099";
const headers = { "X-Milevia-Session": "test-token-123", "Content-Type": "application/json" };

async function call(method, path, body) {
  const response = await fetch(base + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await response.text();
  let parsed = null;
  try { parsed = text ? JSON.parse(text) : null; } catch { parsed = text; }
  return { status: response.status, body: parsed };
}

const failures = [];
function check(label, condition, detail) {
  console.log(`${condition ? "OK  " : "FAIL"} ${label}${condition ? "" : " :: " + detail}`);
  if (!condition) failures.push(label);
}

const projectPath = process.argv[2];
if (!projectPath) throw new Error("usage: node verify-model-endpoints.mjs <projectPath>");

for (const agentId of ["claude-code", "codex"]) {
  // 每个 agent 用独立项目：一个项目只保留一个 is_current 会话，同一项目再次建会话会复用
  // 已有的那个（既有行为），那样测到的就不是本 agent 的会话了。
  const created = await call("POST", "/api/projects", { path: projectPath, name: `modelcheck-${agentId}` });
  check(`[${agentId}] 注册项目`, created.status === 200 || created.status === 201, JSON.stringify(created));
  const projectId = created.body?.id ?? created.body?.project?.id;
  check(`[${agentId}] 拿到 projectId`, Boolean(projectId), JSON.stringify(created.body));

  // ?new=true 才能拿到新会话：不带该参数时后端会复用项目当前的 is_current 会话
  // （前端"新会话"按钮走的也是 ?new=true）。
  const conversation = await call("POST", `/api/projects/${projectId}/conversations?new=true`, { agentId, permissionMode: agentId === "codex" ? "read_only" : "approval_required" });
  check(`[${agentId}] 建会话`, conversation.status === 200 || conversation.status === 201, JSON.stringify(conversation));
  const conversationId = conversation.body?.id;
  if (!conversationId) continue;
  check(`[${agentId}] 会话 agentId 正确`, conversation.body.agentId === agentId, JSON.stringify(conversation.body));
  check(`[${agentId}] 新会话 modelOverride 为空`, (conversation.body.modelOverride ?? "") === "", JSON.stringify(conversation.body));

  const before = await call("GET", `/api/conversations/${conversationId}/models`);
  check(`[${agentId}] 读模型目录`, before.status === 200, JSON.stringify(before));
  console.log(`     source=${before.body?.source} effective=${JSON.stringify(before.body?.effective)} 候选=${before.body?.models?.length} note=${JSON.stringify(before.body?.note)}`);
  console.log(`     前几项：${(before.body?.models ?? []).slice(0, 4).map((m) => m.id).join(", ")}`);
  check(`[${agentId}] 目录非空`, (before.body?.models?.length ?? 0) > 0, JSON.stringify(before.body));
  check(`[${agentId}] 允许自定义`, before.body?.customAllowed === true, JSON.stringify(before.body));
  check(`[${agentId}] 初始为跟随配置`, before.body?.source === "cli_default", JSON.stringify(before.body));

  const bad = await call("POST", `/api/conversations/${conversationId}/model`, { model: "opus; rm -rf /" });
  check(`[${agentId}] 非法模型名被拒`, bad.status === 400, JSON.stringify(bad));

  const chosen = before.body?.models?.[0]?.id ?? "opus";
  const set = await call("POST", `/api/conversations/${conversationId}/model`, { model: chosen });
  check(`[${agentId}] 设置模型 ${chosen}`, set.status === 200 && set.body?.modelOverride === chosen, JSON.stringify(set));

  const after = await call("GET", `/api/conversations/${conversationId}/models`);
  check(`[${agentId}] 目录反映选择`, after.body?.selected === chosen && after.body?.source === "override" && after.body?.effective === chosen, JSON.stringify(after.body));

  const cleared = await call("POST", `/api/conversations/${conversationId}/model`, { model: "" });
  check(`[${agentId}] 清空回跟随配置`, cleared.status === 200 && (cleared.body?.modelOverride ?? "") === "", JSON.stringify(cleared));
}

console.log(failures.length === 0 ? "\n全部通过" : `\n失败 ${failures.length} 项: ${failures.join(" | ")}`);
process.exit(failures.length === 0 ? 0 : 1);
