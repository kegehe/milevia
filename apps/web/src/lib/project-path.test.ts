import assert from "node:assert/strict";
import test from "node:test";

import { projectFileReference } from "./project-path";

test("认得项目内路径，含目录与行号后缀", () => {
  for (const testCase of [
    { input: "src/main.ts", path: "src/main.ts", line: 0 },
    { input: "README.md", path: "README.md", line: 0 },
    { input: "apps/web/src/App.tsx", path: "apps/web/src/App.tsx", line: 0 },
    { input: "  docs/40-方案.md  ", path: "docs/40-方案.md", line: 0 },
    // AI 回答里最常带的两种写法。
    { input: "src/main.ts:42", path: "src/main.ts", line: 42 },
    { input: "./src/main.ts", path: "src/main.ts", line: 0 },
    // 点开头的整档配置文件名是真文件，不能一刀切掉。
    { input: ".env", path: ".env", line: 0 },
    { input: ".gitignore", path: ".gitignore", line: 0 },
  ]) {
    const reference = projectFileReference(testCase.input);
    assert.equal(reference?.path, testCase.path, `path for ${JSON.stringify(testCase.input)}`);
    assert.equal(reference?.line, testCase.line, `line for ${JSON.stringify(testCase.input)}`);
  }
});

// 误判比漏判糟得多：把普通句子变成链接，点下去是一个"文件不存在"的报错。
// 这一组是本模块存在的理由，每一条都对应一种真实会出现的写法。
test("不把普通文本误判成路径", () => {
  for (const input of [
    "react",
    "node_modules",
    "pnpm dev",
    "npm run build",
    "/etc/passwd",
    "~/notes.md",
    "https://example.com/a.ts",
    "//example.com/a.ts",
    // 版本号是最常见的伪装者：有点、有字母数字，但扩展名不是字母开头。
    "v1.2.3",
    "1.5.0-beta",
    "2.0",
    "foo.123",
    // 文案里的半句话。
    "例如 src/main.ts",
    "见 apps/web。",
    "../outside.ts",
    "a\\b.ts",
    // 超长的一整段代码。
    `const x = ${"a".repeat(300)}.ts`,
    "",
    "   ",
  ]) {
    assert.equal(projectFileReference(input), null, `${JSON.stringify(input)} 不该被当成路径`);
  }
});

test("中文文件名与目录名照常认得", () => {
  const reference = projectFileReference("文档/说明.md:7");
  assert.equal(reference?.path, "文档/说明.md");
  assert.equal(reference?.line, 7);
});

// 行号只认 1–6 位：`a:b` 这种冒号结构不能被当成行号切掉 —— 切错了会去找一个
// 根本不存在的路径，而正确做法是整段判为不是路径。
test("不是数字后缀的冒号不当作行号", () => {
  assert.equal(projectFileReference("a:b"), null);
  assert.equal(projectFileReference("note:1234567"), null);
  assert.equal(projectFileReference("note:12"), null, "note 没有扩展名，本身就不是路径");
  assert.equal(projectFileReference("src/a.ts:7")?.path, "src/a.ts");
});
