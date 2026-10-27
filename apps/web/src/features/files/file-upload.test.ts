import assert from "node:assert/strict";
import test from "node:test";
import { droppedFiles, FILE_UPLOAD_LIMIT, MOBILE_FILE_UPLOAD_LIMIT, uploadFiles } from "./file-upload";

test("上传按原始字节编码，保留文件名和目录，使用 createOnly 并逐个完成", async () => {
  const bytes = Uint8Array.from({ length: 100_000 }, (_, index) => index % 256);
  const files = [new File([bytes], "图片.png"), new File(["%PDF-1.7\0\xff"], "report.pdf"), new File([], "empty.bin")];
  const progress: number[] = [];
  let inFlight = false;
  const result = await uploadFiles({
    files, dirPath: "assets", endpoint: "/write?conversationId=c1", signal: new AbortController().signal,
    onProgress: (completed) => progress.push(completed),
    request: async <T>(endpoint: string, init?: RequestInit) => {
      assert.equal(inFlight, false);
      inFlight = true;
      await Promise.resolve();
      assert.equal(endpoint, "/write?conversationId=c1");
      assert.equal(init?.method, "PUT");
      const body = JSON.parse(String(init?.body));
      const index = progress.length;
      assert.equal(body.path, `assets/${files[index].name}`);
      assert.equal(body.encoding, "base64");
      assert.equal(body.createOnly, true);
      assert.deepEqual(Buffer.from(body.content, "base64"), Buffer.from(await files[index].arrayBuffer()));
      inFlight = false;
      return {} as T;
    },
  });
  assert.deepEqual(result, { uploaded: files.map((file) => `assets/${file.name}`), failures: [] });
  assert.deepEqual(progress, [1, 2, 3]);
});

test("同名或写入失败不阻止后续文件，返回逐文件失败原因", async () => {
  const result = await uploadFiles({
    files: [new File(["old"], "same.txt"), new File(["new"], "next.bin")],
    dirPath: "", endpoint: "/write", signal: new AbortController().signal,
    request: async <T>(_path: string, init?: RequestInit) => {
      if (JSON.parse(String(init?.body)).path === "same.txt") throw new Error("文件已存在");
      return {} as T;
    },
  });
  assert.deepEqual(result, { uploaded: ["next.bin"], failures: [{ name: "same.txt", message: "文件已存在" }] });
});

test("桌面和手机的大小上限在读取内容和发送请求前检查，非法文件名不会写入", async () => {
  for (const mobile of [false, true]) {
    const file = { name: "large.pdf", size: (mobile ? MOBILE_FILE_UPLOAD_LIMIT : FILE_UPLOAD_LIMIT) + 1,
      arrayBuffer: () => { throw new Error("不应读取超限文件"); } } as unknown as File;
    const result = await uploadFiles({
      files: [file, new File([], "../escape"), new File([], "..")],
      dirPath: "", mobile, endpoint: "/write", signal: new AbortController().signal,
      request: async () => { assert.fail("不应写入无效文件"); },
    });
    assert.equal(result.uploaded.length, 0);
    assert.equal(result.failures.length, 3);
    assert.match(result.failures[0].message, mobile ? /256 KiB/ : /10 MiB/);
  }
});

test("切换工作区时中止批次，读取结束后不再发送写入", async () => {
  const controller = new AbortController();
  const file = { name: "old.bin", size: 1, arrayBuffer: async () => {
    controller.abort();
    return new Uint8Array([255]).buffer;
  } } as File;
  await assert.rejects(uploadFiles({
    files: [file, new File([], "next.bin")], dirPath: "", endpoint: "/write", signal: controller.signal,
    request: async () => { assert.fail("旧工作区的批次不应继续写入"); },
  }), { name: "AbortError" });
});

test("系统拖入过滤目录，但保留普通文件和零字节文件", () => {
  const file = new File([], "empty.bin");
  const result = droppedFiles({ items: [
    { kind: "string", getAsFile: () => null },
    { kind: "file", webkitGetAsEntry: () => ({ isDirectory: true }), getAsFile: () => new File([], "folder") },
    { kind: "file", getAsFile: () => file },
  ] } as unknown as DataTransfer);
  assert.deepEqual(result, { files: [file], hasDirectories: true });
  assert.deepEqual(droppedFiles({ files: [file] } as unknown as DataTransfer), { files: [file], hasDirectories: false });
});

test("已发送的请求即使忽略取消信号并成功返回，取消批次后也不再发送下一个文件", async () => {
  const controller = new AbortController();
  const writes: string[] = [];
  await assert.rejects(uploadFiles({
    files: [new File(["first"], "first.bin"), new File(["second"], "second.bin")],
    dirPath: "assets", endpoint: "/write", signal: controller.signal,
    request: async <T>(_path: string, init?: RequestInit) => {
      writes.push(JSON.parse(String(init?.body)).path);
      // 手机适配器不支持取消已发出的 RPC；批次仍必须停止后续文件。
      controller.abort();
      return {} as T;
    },
  }), { name: "AbortError" });
  assert.deepEqual(writes, ["assets/first.bin"]);
});
