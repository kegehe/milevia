import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { MOBILE_AGENTS, mobileAgent } from "./mobile-agent-catalog";

test("手机端设置页与远程控制页共用工具清单", async () => {
  const pages = await Promise.all(["SettingsPage", "MobileRemotePage"].map((name) =>
    readFile(new URL(`../pages/${name}.tsx`, import.meta.url), "utf8"),
  ));
  for (const page of pages) {
    assert.match(page, /import \{ MOBILE_AGENTS \} from "\.\.\/lib\/mobile-agent-catalog"/);
    assert.match(page, /MOBILE_AGENTS\.map\(/);
    assert.doesNotMatch(page, /(?:const|let)\s+MOBILE_AGENTS\s*[:=]/);
  }
});

// Go 侧是服务端目录的来源。除了 ID，还保护名称、厂商、权限档位及默认权限，
// 避免手机端仍能选中工具，却显示旧名称或提交服务端不支持的权限。
test("手机端工具清单与服务端目录一致", async (t) => {
  const catalog = await readFile(
    new URL("../../../control-server/internal/app/agent_catalog.go", import.meta.url),
    "utf8",
  ).catch((error: NodeJS.ErrnoException) => {
    if (error.code === "ENOENT") return undefined;
    throw error;
  });
  if (catalog === undefined) {
    t.skip("只装了 web 包的裁剪环境，没有服务端目录");
    return;
  }

  const entries = catalog.match(/var agentCatalogEntries = \[\]AgentCatalogEntry\{([\s\S]*?)^\}/m)?.[1];
  assert.ok(entries, "找不到 agentCatalogEntries");
  const serverAgents = [...entries.matchAll(/^\t\{\r?\n([\s\S]*?)^\t\},/gm)].map((match) => {
    const entry = match[1];
    const field = (name: string) => {
      const value = entry.match(new RegExp(`^\\s*${name}:\\s+"([^"]+)",`, "m"))?.[1];
      assert.ok(value, `目录条目缺少 ${name}：${entry}`);
      return value;
    };
    const modes = entry.match(/^\s*PermissionModes:\s+\[\]string\{([^}]+)\}/m)?.[1];
    assert.ok(modes, `目录条目缺少 PermissionModes：${entry}`);
    return {
      id: field("ID"),
      name: field("Name"),
      vendor: field("Vendor"),
      permissionModes: [...modes.matchAll(/"([a-z_]+)"/g)].map((mode) => mode[1]),
      defaultPermission: field("DefaultPermissionMode"),
    };
  });
  const serverIDs = [...entries.matchAll(/^\s*ID:\s+"([a-z0-9-]+)",/gm)].map((match) => match[1]);
  assert.ok(serverIDs.length >= 3, "从 agent_catalog.go 解析出的工具太少");
  assert.equal(serverAgents.length, serverIDs.length, "未能解析全部目录条目");

  const mobileAgents = MOBILE_AGENTS.map(({ id, name, vendor, permissionModes, defaultPermission }) =>
    ({ id, name, vendor, permissionModes, defaultPermission }),
  );
  assert.deepEqual(mobileAgents, serverAgents, "手机端工具信息或顺序与 agent_catalog.go 漂移");
});

test("手机端保留厂商的中文展示名", () => {
  assert.equal(mobileAgent("codebuddy")?.vendorLabel, "腾讯云");
  assert.equal(mobileAgent("qoder")?.vendorLabel, "阿里云");
});
