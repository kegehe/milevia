import assert from "node:assert/strict";
import test from "node:test";
import { formatModTime, validateFileName } from "./file-model";

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

// formatModTime 是文件信息条上「大小 · 修改时间」的时间那一半。
// 解析不了必须返回空串（调用方据此整段省略），绝不能让 "Invalid Date" 上屏。

test("formatModTime：解析不了的时间返回空串，由调用方省略", () => {
  assert.equal(formatModTime("not-a-date"), "");
  assert.equal(formatModTime(""), "");
});

test("formatModTime：当前时刻按「今天 HH:mm」读出", () => {
  // 用当前时刻本身构造，不受时区与跨午夜影响；经 ISO 字符串走一圈，
  // 正是调用链的真实路径（服务端给 ISO，formatModTime 再解析回来）。
  const label = formatModTime(new Date().toISOString());
  assert.match(label, /^今天 \d{2}:\d{2}$/);
});

test("formatModTime：往年时间带年份，且绝不落进「今天」", () => {
  // 本地构造固定时刻再转 ISO：往返解析后各分量不变，与运行环境的时区无关。
  const label = formatModTime(new Date(2019, 0, 15, 8, 5).toISOString());
  assert.equal(label, "2019年1月15日 08:05");
});
