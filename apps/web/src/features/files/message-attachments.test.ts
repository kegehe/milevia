import assert from "node:assert/strict";
import test from "node:test";
import { uploadMessageFiles } from "./MessageAttachments";

test("局域网 HTTP 缺少 crypto 或 randomUUID 时仍能上传附件", async () => {
  const original = Object.getOwnPropertyDescriptor(globalThis, "crypto");
  try {
    for (const cryptoValue of [undefined, {}]) {
      Object.defineProperty(globalThis, "crypto", { configurable: true, value: cryptoValue });
      const paths: string[] = [];
      const uploaded = await uploadMessageFiles([new File(["ok"], "lan.txt")], "p", async <T>(_url: string, init?: RequestInit) => {
        paths.push(JSON.parse(String(init?.body)).path);
        return {} as T;
      });
      assert.equal(uploaded.length, 1);
      assert.match(uploaded[0].path, /^\.milevia-attachments\/[a-zA-Z0-9-]+\/lan\.txt$/);
      assert.deepEqual(paths, [".milevia-attachments", uploaded[0].path.slice(0, -8), uploaded[0].path]);
    }
  } finally {
    if (original) Object.defineProperty(globalThis, "crypto", original);
    else Reflect.deleteProperty(globalThis, "crypto");
  }
});

test("附件部分失败时保留成功路径及其真实元数据", async () => {
  const files = [new File(["bad"], "bad.txt", { type: "text/plain" }), new File(["image"], "ok.png", { type: "image/png" })];
  const failures: string[] = [];
  const uploaded = await uploadMessageFiles(files, "project", async <T>(_url: string, init?: RequestInit) => {
    if (init?.method === "PUT" && String(init.body).includes("bad.txt")) throw new Error("写入失败");
    return {} as T;
  }, false, new AbortController().signal, (message) => failures.push(message));
  assert.equal(uploaded.length, 1);
  assert.match(uploaded[0].path, /^\.milevia-attachments\/[^/]+\/ok\.png$/);
  assert.equal(uploaded[0].size, 5);
  assert.equal(uploaded[0].mediaType, "image/png");
  assert.equal(failures.length, 1);
  assert.match(failures[0], /bad.txt/);
});

test("附件数量和合计限制在创建目录之前拒绝", async () => {
  const request = async <T>(): Promise<T> => { assert.fail("不得创建目录或写文件"); };
  await assert.rejects(uploadMessageFiles(Array.from({ length: 9 }, () => new File([], "f")), "p", request), /8/);
  await assert.rejects(uploadMessageFiles([{ size: 21 * 1024 * 1024 } as File], "p", request), /20 MiB/);
});

test("手机附件复用中继文件上限，取消后不再继续上传", async () => {
  const request = async <T>(_url: string, init?: RequestInit): Promise<T> => {
    assert.notEqual(init?.method, "PUT");
    return {} as T;
  };
  await assert.rejects(uploadMessageFiles([new File([new Uint8Array(256 * 1024 + 1)], "large.bin")], "p", request, true), /256 KiB/);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(uploadMessageFiles([new File(["x"], "small.txt")], "p", request, false, controller.signal), { name: "AbortError" });
});
