import assert from "node:assert/strict";
import test from "node:test";
import { validateFileName } from "./file-model";

// validateFileName 是新建文件 / 新建目录 / 重命名三个入口共用的名字校验。
// 它守的是"名字接在父目录后面会不会逃出父目录"，不是"名字长得像不像禁忌字符"：
// 前者的判据只有分隔符与整个名字就是 `.` / `..` 两条，后者会把正常文件名误拦。
//
// 这里锁死行为而不是源码字面量 —— 2026-09-29 修的原始 bug 正是把判据写成了
// `includes("..")`，子串匹配看起来"更安全"，实际把 `config..bak` 这类名字也挡了。

test("拒绝会让路径逃出父目录的写法", () => {
  // 空名字
  assert.ok(validateFileName(""));
  assert.ok(validateFileName("   "));
  // 分隔符：一段名字被当成多段路径（`../` 也从这条被挡掉）
  assert.ok(validateFileName("a/b"));
  assert.ok(validateFileName("a\\b"));
  assert.ok(validateFileName("../x"));
  assert.ok(validateFileName("..\\..\\x"));
  // 整个名字就是 . / .. ：`dir/..` 直接指到上级
  assert.ok(validateFileName("."));
  assert.ok(validateFileName(".."));
  assert.ok(validateFileName("  ..  "));
});

test("放过含连续点的正常文件名（这是被修掉的那类误拦）", () => {
  for (const name of [
    "v1..2.md",
    "config..bak",
    "a..b",
    "..foo", // 点开头但不是 `..` 本身，是普通文件
    "...",
    "..foo..bar.txt",
    "tail..",
    "备份..2026.tar",
  ]) {
    assert.equal(validateFileName(name), null, `应放行：${name}`);
  }
});

test("其余常见合法名字照旧通过", () => {
  for (const name of ["new-file.ts", ".env", "文档 说明.md", "a b.txt", "  padded.txt  "]) {
    assert.equal(validateFileName(name), null, `应放行：${name}`);
  }
});

test("错误文案是给用户看的，必须非空且指出原因", () => {
  assert.equal(validateFileName(""), "文件名不能为空");
  assert.match(String(validateFileName("a/b")), /分隔符/);
  assert.match(String(validateFileName("..")), /\.\./);
});
