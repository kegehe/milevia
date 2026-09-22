import assert from "node:assert/strict";
import test from "node:test";

import {
  createMobileFsRequest,
  MobileFsError,
  type MobileRpcReply,
  type MobileOpenPayload,
} from "./mobile-fs-request";
import type { FileInfo } from "./file-model";

// 一个记账用的桩：记录每次发出了什么 op / params，并按脚本回话。
// 断言里既看"返回了什么"，也看"打了几枪" —— 缓存类的改动最容易只验证返回值，
// 结果把"每次都重新请求"当成功（这正是适配器要省掉的那几次往返）。
function makeTransport(script: (op: string, params: unknown) => MobileRpcReply | undefined) {
  const calls: Array<{ op: string; params: unknown }> = [];
  const transport = async (op: string, params: unknown): Promise<MobileRpcReply> => {
    calls.push({ op, params });
    const reply = script(op, params);
    if (!reply) throw new Error(`stub has no reply for ${op} ${JSON.stringify(params)}`);
    return reply;
  };
  return { transport, calls };
}

const ok = (data: unknown): MobileRpcReply => ({ ok: true, status: 200, data });

function fileInfo(overrides: Partial<FileInfo> = {}): FileInfo {
  return {
    name: "main.ts",
    path: "src/main.ts",
    isDir: false,
    size: 12,
    modTime: "2026-09-21T00:00:00Z",
    mode: "0644",
    isText: true,
    mimeType: "application/typescript",
    ...overrides,
  };
}

function openPayload(overrides: Partial<MobileOpenPayload> = {}): MobileOpenPayload {
  return {
    stat: fileInfo(),
    content: "export {}\n",
    version: "v1",
    bytes: 10,
    editable: true,
    ...overrides,
  };
}

const PROJECT = "/api/projects/p1";

// ─── 路径映射 ───────────────────────────────────────────────────────────────

test("每个桌面端点都映射到对应的 op，方法写错要当场报错", async () => {
  for (const testCase of [
    { route: "tree", method: "GET", op: "fs.tree", hasBody: false },
    { route: "stat", method: "GET", op: "fs.open", hasBody: false },
    { route: "read", method: "GET", op: "fs.open", hasBody: false },
    { route: "search", method: "GET", op: "fs.search", hasBody: false },
    { route: "sqlite/tables", method: "GET", op: "fs.sqlite.tables", hasBody: false },
    { route: "sqlite/schema", method: "GET", op: "fs.sqlite.schema", hasBody: false },
    { route: "sqlite/rows", method: "GET", op: "fs.sqlite.rows", hasBody: false },
    { route: "write", method: "PUT", op: "fs.write", hasBody: true },
    { route: "mkdir", method: "POST", op: "fs.mkdir", hasBody: true },
    { route: "rename", method: "POST", op: "fs.rename", hasBody: true },
    { route: "remove", method: "DELETE", op: "fs.remove", hasBody: false },
  ] as const) {
    const { transport, calls } = makeTransport((op) =>
      op === "fs.open" ? ok(openPayload()) : ok({}),
    );
    const adapter = createMobileFsRequest({ transport });
    const init = testCase.hasBody
      ? { method: testCase.method, body: JSON.stringify({ path: "a/b.ts", content: "x" }) }
      : { method: testCase.method };
    await adapter.request(`${PROJECT}/fs/${testCase.route}?path=a%2Fb.ts`, init);
    assert.deepEqual(calls.map((call) => call.op), [testCase.op], `${testCase.route} 的 op 不对`);

    // 方法写错是接线错误：静默按表里的方法发出去，会让一个本该报错的调用
    // 看起来"成功了"，直到用户发现数据没变。
    const wrong = makeTransport(() => ok({}));
    const wrongAdapter = createMobileFsRequest({ transport: wrong.transport });
    await assert.rejects(
      () => wrongAdapter.request(`${PROJECT}/fs/${testCase.route}`, { method: "GET" === testCase.method ? "POST" : "GET" }),
      (error: unknown) => error instanceof MobileFsError && error.code === "wiring",
      `${testCase.route} 的方法写错没有被拦下`,
    );
    assert.equal(wrong.calls.length, 0, "接线错误不应该真的发出去");
  }
});

test("未支持与未映射的端点要说清原因，不能静默", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileFsRequest({ transport });

  // /fs/raw 与 /fs/download 要的是"浏览器直接去取字节"（<img src> / <a download>），
  // 那条路带不了令牌，手机也到不了电脑的本机端口。
  for (const route of ["raw", "download", "download-ticket"]) {
    await assert.rejects(
      () => adapter.request(`${PROJECT}/fs/${route}?path=a.png`),
      (error: unknown) => error instanceof MobileFsError && error.code === "unsupported",
      `${route} 应当明确说"不支持"，而不是抛一个看不懂的错`,
    );
  }
  await assert.rejects(
    () => adapter.request(`${PROJECT}/fs/not-a-real-route`),
    (error: unknown) => error instanceof MobileFsError && error.code === "wiring",
  );
  assert.equal(calls.length, 0);
});

// conversationId 由页面在构造适配器时定死（文件视图是绑工作区的）。
// 它从查询里被剔掉而不是原样透传成 params —— 否则会出现在不该出现的地方
// （比如作为 fs.write 的请求体字段）。
test("查询里的 conversationId 被剔掉，不混进 params", async () => {
  const { transport, calls } = makeTransport(() => ok({ entries: [] }));
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/tree?conversationId=c9&depth=3`);
  assert.deepEqual(calls[0]?.params, { depth: "3" });

  const write = makeTransport(() => ok({ status: "ok", version: "v2" }));
  const writeAdapter = createMobileFsRequest({ transport: write.transport });
  await writeAdapter.request(`${PROJECT}/fs/write?conversationId=c9`, {
    method: "PUT",
    body: JSON.stringify({ path: "a.ts", content: "x" }),
  });
  assert.deepEqual(write.calls[0]?.params, { path: "a.ts", content: "x" });
});

test("请求体不是 JSON 对象时当场报错", async () => {
  const { transport, calls } = makeTransport(() => ok({}));
  const adapter = createMobileFsRequest({ transport });
  await assert.rejects(
    () => adapter.request(`${PROJECT}/fs/write`, { method: "PUT", body: "not json" }),
    (error: unknown) => error instanceof MobileFsError && error.code === "wiring",
  );
  assert.equal(calls.length, 0);
});

// ─── 业务失败 ───────────────────────────────────────────────────────────────

// FilesPanel 靠 `status === 409` 走"文件已被修改，请重新加载"的分支，
// 所以状态码必须一路带到抛出的错误上；error 里那句中文是服务端写的，原样传。
test("业务失败抛出带状态码的错误，文案原样保留", async () => {
  const { transport } = makeTransport(() => ({
    ok: false,
    status: 409,
    error: "文件已被修改，请重新加载后再保存",
  }));
  const adapter = createMobileFsRequest({ transport });
  await assert.rejects(
    () => adapter.request(`${PROJECT}/fs/write`, { method: "PUT", body: JSON.stringify({ path: "a.ts" }) }),
    (error: unknown) => {
      assert.ok(error instanceof MobileFsError);
      assert.equal(error.status, 409);
      assert.equal(error.code, "operation_failed");
      assert.equal(error.message, "文件已被修改，请重新加载后再保存");
      return true;
    },
  );
});

// ─── 内容闸门 ───────────────────────────────────────────────────────────────

// 内容被省略时**绝不能**把 "" 交出去：查看器会渲染出一个空文件，用户以为文件是空的。
// 这是"宁可说看不了，也不能假装能看"那条纪律在适配器这一层的落点。
test("内容被省略时抛结构化错误并带上原始载荷", async () => {
  const payload = openPayload({
    content: null,
    version: "",
    editable: false,
    omittedReason: "too_large",
    readOnlyReason: "file_too_large",
    stat: fileInfo({ name: "big.ts", size: 5 * 1024 * 1024, path: "big.ts" }),
  });
  for (const route of ["read", "stat"]) {
    const { transport } = makeTransport(() => ok(payload));
    const adapter = createMobileFsRequest({ transport });
    if (route === "stat") {
      // stat 只取元信息，被省略内容时它仍然必须能答 —— 调用方要靠它渲染元信息卡。
      const stat = await adapter.request<FileInfo>(`${PROJECT}/fs/stat?path=big.ts`);
      assert.equal(stat.size, 5 * 1024 * 1024);
      continue;
    }
    await assert.rejects(
      () => adapter.request(`${PROJECT}/fs/read?path=big.ts`),
      (error: unknown) => {
        assert.ok(error instanceof MobileFsError);
        assert.equal(error.code, "content_omitted");
        // 载荷要带出来，后续查看器才能据此渲染元信息卡而不是空白。
        assert.equal(error.payload?.stat.name, "big.ts");
        // 文案要能解释"为什么看不了"，不是一句技术错误。
        assert.match(error.message, /太大/);
        assert.match(error.message, /请在电脑上查看/);
        return true;
      },
    );
  }
});

test("二进制文件被省略时的说明要说它是二进制，不能说成文件太大", async () => {
  const payload = openPayload({
    content: null,
    version: "",
    editable: false,
    omittedReason: "binary",
    readOnlyReason: "binary_file",
    stat: fileInfo({ name: "app.exe", mimeType: "application/octet-stream" }),
  });
  const { transport } = makeTransport(() => ok(payload));
  const adapter = createMobileFsRequest({ transport });
  await assert.rejects(
    () => adapter.request(`${PROJECT}/fs/read?path=app.exe`),
    (error: unknown) => {
      assert.ok(error instanceof MobileFsError);
      // 说成"太大"是给了用户一个改不掉的理由 —— 他没法把 exe 弄小。
      assert.doesNotMatch(error.message, /太大/);
      assert.match(error.message, /不是文本文件/);
      return true;
    },
  );
});

// ─── 缓存 ───────────────────────────────────────────────────────────────────

// stat 与 read 都映射到 fs.open。没有这一层，打开一个文件就是两次往返
// （手机到电脑每次几百毫秒到一秒），白等一半。
test("同一文件的 stat 与 read 只发一次请求", async () => {
  const { transport, calls } = makeTransport(() => ok(openPayload()));
  const adapter = createMobileFsRequest({ transport });
  const stat = await adapter.request<FileInfo>(`${PROJECT}/fs/stat?path=src%2Fmain.ts`);
  assert.equal(stat.path, "src/main.ts");
  const content = await adapter.request<{ content: string; version: string }>(`${PROJECT}/fs/read?path=src%2Fmain.ts`);
  assert.equal(content.content, "export {}\n");
  assert.equal(calls.length, 1, "同一个文件不该发两次请求");

  // read 交出去的形状必须是 FileContent（查看器/编辑器按它取值）。
  // `editable` 是手机端多出来的一格：服务端已经判过"能不能改"，界面必须收得到
  // （见 FileContent.editable）。除此之外不许有别的键冒出来 —— `encoding: undefined`
  // 那种"键在但值是 undefined"会让两端的 `in` 判断得到相反结果。
  assert.deepEqual(Object.keys(content).sort(), ["content", "editable", "stat", "version"]);
});

// 只读原因只在服务端真的判了 false 时才出现：可以编辑的文件不该带一个空的
// `readOnlyReason`，否则界面得再判一次"这个空串算不算有原因"。
test("可编辑的文件不带 readOnlyReason", async () => {
  const { transport } = makeTransport(() => ok(openPayload()));
  const adapter = createMobileFsRequest({ transport });
  const content = await adapter.request<Record<string, unknown>>(`${PROJECT}/fs/read?path=src%2Fmain.ts`);
  assert.equal("readOnlyReason" in content, false);
});

// 一次 fs.tree 带 depth 拿好几层，摊平成「目录 → 直接子项」之后，
// 展开子目录应当**零往返**命中缓存 —— 这是手机端"展开不卡"的全部依据。
test("一次带深度的取树之后，展开子目录不再发请求", async () => {
  const tree = {
    entries: [
      {
        name: "src",
        path: "src",
        isDir: true,
        children: [
          { name: "lib", path: "src/lib", isDir: true, children: [] },
          { name: "main.ts", path: "src/main.ts", isDir: false },
        ],
      },
      { name: "README.md", path: "README.md", isDir: false },
    ],
    truncated: false,
    skippedDirs: 2,
  };
  const { transport, calls } = makeTransport(() => ok(tree));
  const adapter = createMobileFsRequest({ transport });

  const root = await adapter.request<typeof tree>(`${PROJECT}/fs/tree`);
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0]?.params, { depth: "3" }, "根目录取树要带默认深度");
  assert.equal(root.skippedDirs, 2, "跳过依赖目录的数要带出来，界面才说得清");

  // 展开 src：命中缓存，不再往返。
  const src = await adapter.request<typeof tree>(`${PROJECT}/fs/tree?path=src`);
  assert.equal(calls.length, 1, "展开子目录不该再发请求");
  assert.deepEqual(src.entries.map((entry) => entry.name), ["lib", "main.ts"]);
  // 摊平之后 children 已经没用了，留着会让每次读缓存都重新遍历一遍深树。
  assert.equal(src.entries[0]?.children, undefined, "摊平后不该留下 children");

  // 再展开一层：同样命中。
  const lib = await adapter.request<typeof tree>(`${PROJECT}/fs/tree?path=src%2Flib`);
  assert.equal(calls.length, 1);
  assert.deepEqual(lib.entries, []);
});

test("缓存未命中时才真的发请求，且带 depth", async () => {
  const { transport, calls } = makeTransport(() => ok({ entries: [] }));
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/tree?path=other`);
  assert.deepEqual(calls[0]?.params, { depth: "3", path: "other" });
});

// ─── 失效范围 ───────────────────────────────────────────────────────────────

// 一律清空会让上一次展开的子树全丢，用户每删一个文件都要重新点回原位置。
// 所以每个写操作只丢"列表真的变了"的那些目录。
test("写文件只丢该文件的内容缓存与它父目录的树", async () => {
  const { transport, calls } = makeTransport((op) => {
    if (op === "fs.tree") return ok({ entries: [{ name: "main.ts", path: "src/main.ts", isDir: false }] });
    if (op === "fs.open") return ok(openPayload());
    return ok({ status: "ok", version: "v2" });
  });
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/tree?path=src`);
  await adapter.request(`${PROJECT}/fs/read?path=src%2Fmain.ts`);
  const before = calls.length;

  await adapter.request(`${PROJECT}/fs/write`, {
    method: "PUT",
    body: JSON.stringify({ path: "src/main.ts", content: "changed", expectedVersion: "v1" }),
  });

  // 父目录的列表被丢掉了 ⇒ 再取一次树要重新发请求。
  await adapter.request(`${PROJECT}/fs/tree?path=src`);
  assert.equal(calls.length, before + 2, "写完之后父目录的树应当重新取");
  // 内容缓存也要丢：否则重新打开这个文件会看到写之前的旧内容。
  await adapter.request(`${PROJECT}/fs/read?path=src%2Fmain.ts`);
  assert.equal(calls.length, before + 3);
});

test("删目录要丢掉它自己与它下面所有目录的树缓存", async () => {
  const tree = {
    entries: [
      {
        name: "src",
        path: "src",
        isDir: true,
        children: [{ name: "lib", path: "src/lib", isDir: true, children: [] }],
      },
    ],
  };
  const { transport, calls } = makeTransport((op) => (op === "fs.tree" ? ok(tree) : ok({ status: "ok" })));
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/tree`);
  const primed = calls.length;

  await adapter.request(`${PROJECT}/fs/remove?path=src%2Flib`, { method: "DELETE" });

  // src 的列表变了（lib 没了），src/lib 也不存在了 ⇒ 两个都要重新取。
  await adapter.request(`${PROJECT}/fs/tree?path=src%2Flib`);
  assert.equal(calls.length, primed + 2, "被删掉的目录不能还命中缓存");
});

// 点「刷新」必须真的重新拿 —— 否则 AI 在电脑上改完文件、用户点刷新，
// 适配器会把缓存里的旧内容还回去。
test("invalidateAll 之后连根目录的树也要重新取", async () => {
  const { transport, calls } = makeTransport(() => ok({ entries: [] }));
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/tree`);
  await adapter.request(`${PROJECT}/fs/tree`);
  assert.equal(calls.length, 1);
  adapter.invalidateAll();
  await adapter.request(`${PROJECT}/fs/tree`);
  assert.equal(calls.length, 2);
});

// 搜索与 SQLite 分页都是"每次都要新数据"的调用，不能进缓存：
// 缓存住搜索会让用户在文件改名之后搜不到它。
test("搜索与 sqlite 查询不进缓存", async () => {
  const { transport, calls } = makeTransport(() => ok({ entries: [] }));
  const adapter = createMobileFsRequest({ transport });
  await adapter.request(`${PROJECT}/fs/search?query=main`);
  await adapter.request(`${PROJECT}/fs/search?query=main`);
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[0]?.params, { query: "main" });
});

// ─── 图片字节 ───────────────────────────────────────────────────────────────

// 手机端不能再拼 /fs/raw 交给 <img src>（浏览器导航，带不了令牌，也到不了电脑的
// 本机端口）。字节只能从中继取回来，由调用方转成 object URL。
test("resolveMedia 从载荷里取出图片字节", async () => {
  const { transport, calls } = makeTransport(() =>
    ok(openPayload({ content: "aGVsbG8=", encoding: "base64", stat: fileInfo({ name: "shot.png", path: "shot.png", mimeType: "image/png" }) })),
  );
  const adapter = createMobileFsRequest({ transport });
  const resolution = await adapter.resolveMedia("shot.png");
  assert.equal(resolution.kind, "ready");
  assert.equal(resolution.kind === "ready" ? resolution.base64 : "", "aGVsbG8=");
  assert.equal(resolution.kind === "ready" ? resolution.mimeType : "", "image/png");
  // 再解析同一个图片要走缓存，不再往返。
  await adapter.resolveMedia("shot.png");
  assert.equal(calls.length, 1);
});

// 图片超过内联上限时要说清"为什么看不到"，不能只说"加载失败" ——
// 后者会让用户以为文件坏了或网络有问题。
test("resolveMedia 对超限图片给出可解释的原因", async () => {
  const { transport } = makeTransport(() =>
    ok(
      openPayload({
        content: null,
        version: "",
        editable: false,
        omittedReason: "binary",
        readOnlyReason: "binary_file",
        stat: fileInfo({ name: "big.png", path: "big.png", size: 3 * 1024 * 1024, mimeType: "image/png" }),
      }),
    ),
  );
  const adapter = createMobileFsRequest({ transport });
  const resolution = await adapter.resolveMedia("big.png");
  assert.equal(resolution.kind, "unavailable");
  assert.match(resolution.kind === "unavailable" ? resolution.message : "", /big\.png/);
});

test("resolveMedia 遇到传输失败不抛异常，回一句可显示的说明", async () => {
  const transport = async (): Promise<MobileRpcReply> => {
    throw new Error("电脑当前离线，请等电脑上线后再试");
  };
  const adapter = createMobileFsRequest({ transport });
  const resolution = await adapter.resolveMedia("shot.png");
  assert.equal(resolution.kind, "unavailable");
  assert.equal(resolution.kind === "unavailable" ? resolution.message : "", "电脑当前离线，请等电脑上线后再试");
});

test("非图片文件交给 resolveMedia 时要说它不是图片", async () => {
  const { transport } = makeTransport(() => ok(openPayload()));
  const adapter = createMobileFsRequest({ transport });
  const resolution = await adapter.resolveMedia("src/main.ts");
  assert.equal(resolution.kind, "unavailable");
  assert.match(resolution.kind === "unavailable" ? resolution.message : "", /不是图片/);
});

// ─── 服务端的「能不能改」必须原样透传 ───────────────────────────────────────

// 服务端有一条**桌面端不存在**的只读带：内容给全了（≤320 KiB），但它大到发不回去
// （>256 KiB），所以 `editable: false`。客户端不认这个字段就会亮出「编辑」，
// 用户改完按保存才失败 —— 而那正是这条字段存在的全部理由。
test("服务端说不能编辑时，editable 与原因都要原样交给界面", async () => {
  const { transport } = makeTransport(() =>
    ok(openPayload({ editable: false, readOnlyReason: "file_too_large" })),
  );
  const adapter = createMobileFsRequest({ transport });
  const content = await adapter.request<{
    content: string;
    editable?: boolean;
    readOnlyReason?: string;
  }>(`${PROJECT}/fs/read?path=big.ts`);
  assert.equal(content.content, "export {}\n");
  assert.equal(content.editable, false);
  assert.equal(content.readOnlyReason, "file_too_large");
});

test("服务端说可以编辑时同样透传，不留一层本地改判", async () => {
  const { transport } = makeTransport(() => ok(openPayload({ editable: true })));
  const adapter = createMobileFsRequest({ transport });
  const content = await adapter.request<{ editable?: boolean }>(`${PROJECT}/fs/read?path=main.ts`);
  assert.equal(content.editable, true);
});

// ─── 内容缓存必须有上限 ─────────────────────────────────────────────────────

// 每条内容最多 320 KiB。没有上限时这份缓存活到页面销毁为止 —— 手机端翻一遍目录
// 就是几十兆，而它换来的是"省一次往返"这么点收益。这里连开 13 个文件，第 1 个必须
// 已经被挤出去（第 13 个是界限内最新的那个，仍然命中缓存）。
test("内容缓存有条目上限，最早打开的那条会被挤掉", async () => {
  const { transport, calls } = makeTransport((_op, params) =>
    ok(openPayload({ version: String((params as { path: string }).path) })),
  );
  const adapter = createMobileFsRequest({ transport });
  for (let index = 1; index <= 13; index += 1) {
    await adapter.request(`${PROJECT}/fs/read?path=file-${index}.ts`);
  }
  assert.equal(calls.length, 13);

  // 最新那条仍在缓存里：重复读不会再发请求。
  await adapter.request(`${PROJECT}/fs/read?path=file-13.ts`);
  assert.equal(calls.length, 13, "刚读过的文件不该被自己挤掉");

  // 最早那条应该已经被挤出去了，再读要重新问一次。
  await adapter.request(`${PROJECT}/fs/read?path=file-1.ts`);
  assert.equal(calls.length, 14, "超出上限后最早打开的那条必须被逐出");
});
