import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { MOBILE_AGENTS } from "./mobile-agent-catalog";

// 手机端的工具清单是**手写镜像**（服务端目录在 Go 侧）。这份镜像有两处：
// lib/mobile-agent-catalog.ts（设置页的默认工具下拉）与 pages/MobileRemotePage.tsx
// 里那份（远程控制页新建会话真正用的）。两处都不在编译期与 Go 目录关联，于是"目录加了
// 工具、手机端没跟"只会表现为"手机端少一个可选项"，没有任何测试会红。
//
// 这条用例把三个集合钉在一起：Go 目录的 ID、TSX 的 MobileAgentID 联合类型、lib 的清单。
// 双向比：多一个少一个都算漂移。解析不到时不静默通过（锚点自证）。
test("手机端工具清单与服务端目录 ID 集合一致", async () => {
  const catalog = await readFile(
    new URL("../../../control-server/internal/app/agent_catalog.go", import.meta.url),
    "utf8",
  ).catch(() => "");
  if (!catalog) return; // 只装了 web 包的裁剪环境，跳过

  const serverIDs = [...catalog.matchAll(/^\s*ID:\s+"([a-z0-9-]+)",/gm)].map((match) => match[1]);
  assert.ok(serverIDs.length >= 3, "从 agent_catalog.go 解析出的工具太少：" + JSON.stringify(serverIDs));

  const page = await readFile(new URL("../pages/MobileRemotePage.tsx", import.meta.url), "utf8");
  const union = page.match(/type MobileAgentID = ([^;]+);/)?.[1] ?? "";
  const pageIDs = [...union.matchAll(/"([a-z0-9-]+)"/g)].map((match) => match[1]);
  assert.ok(pageIDs.length >= 3, "抠不到 MobileRemotePage 的 MobileAgentID 联合类型：" + JSON.stringify(union));

  const sort = (ids: readonly string[]) => [...new Set(ids)].sort();
  assert.deepEqual(sort(pageIDs), sort(serverIDs), "MobileRemotePage 的 MobileAgentID 与 agent_catalog.go 漂移");
  assert.deepEqual(sort(MOBILE_AGENTS.map((agent) => agent.id)), sort(serverIDs), "mobile-agent-catalog.ts 与 agent_catalog.go 漂移");
});
