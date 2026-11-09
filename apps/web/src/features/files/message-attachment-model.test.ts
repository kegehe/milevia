import assert from "node:assert/strict";
import test from "node:test";
import { attachmentMessageBody, mergeMessageAttachments, normalizeAttachmentPath, readAttachmentDraft } from "./message-attachment-model";
import { pendingMessageContentKey } from "../remote/snapshot-pending-message-merge";

test("工作区附件的 Windows 路径、点路径与服务端历史使用相同匹配键", () => {
  const optimistic = { content: "", attachments: [{ path: " ./docs\\design.png " }, { path: "docs/design.png" }] };
  const persisted = { content: "", attachments: [{ path: "docs/design.png" }] };
  assert.equal(pendingMessageContentKey(optimistic), pendingMessageContentKey(persisted));
  assert.equal(normalizeAttachmentPath("docs/../outside"), "docs/../outside");
  assert.equal(normalizeAttachmentPath("/docs/image.png"), "/docs/image.png");
  assert.equal(normalizeAttachmentPath("\\\\server\\image.png"), "/server/image.png");
});

test("恢复失败或预约的附件时合并新草稿，超出发送上限也不静默丢文件", () => {
  const newer = Array.from({ length: 8 }, (_, i) => ({ path: `new-${i}.txt` }));
  const result = mergeMessageAttachments(newer, [{ path: "old.txt" }, { path: "./new-0.txt" }]);
  assert.equal(result.length, 9);
  assert.equal(result[8].path, "old.txt");
});

test("损坏的附件草稿不会进入界面，合法草稿保留元数据", () => {
  assert.deepEqual(readAttachmentDraft("not-json"), []);
  assert.deepEqual(readAttachmentDraft('{"path":"a"}'), []);
  assert.deepEqual(readAttachmentDraft('[null,{}, {"path":"a","size":-1}, {"path":"b","mediaType":4}]'), []);
  assert.deepEqual(readAttachmentDraft('[{"path":"./image.png","size":3,"mediaType":"image/png"}]'), [{ path: "image.png", size: 3, mediaType: "image/png" }]);
});

test("仅附件消息的显示与复制均包含附件路径", () => {
  assert.equal(attachmentMessageBody({ content: "", attachments: [{ path: "docs/design.png" }] }), "@docs/design.png");
  assert.equal(attachmentMessageBody({ content: "review", attachments: [{ path: "a" }] }), "review\n\n@a");
});
