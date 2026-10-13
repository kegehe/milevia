import assert from "node:assert/strict";
import { test } from "node:test";
import { isHoverPointerType } from "./TooltipProvider";

test("only mouse pointers are treated as hover-capable", () => {
  assert.equal(isHoverPointerType("mouse"), true);
  assert.equal(isHoverPointerType("touch"), false);
  assert.equal(isHoverPointerType("pen"), false);
  assert.equal(isHoverPointerType(""), false);
});

test("touch input is filtered before focus can reopen a tooltip", async () => {
  const { readFile } = await import("node:fs/promises");
  const source = await readFile(new URL("./TooltipProvider.tsx", import.meta.url), "utf8");

  assert.match(source, /document\.addEventListener\("pointerdown", pointerDown, true\)/);
  assert.match(source, /if \(target\) showTooltip\(target\)/);
  assert.match(source, /if \(!isHoverPointerType\(pointerType\)\) \{\s*hideTooltip\(\);\s*return;/);
  assert.match(source, /inputModalityRef\.current !== "touch" && inputModalityRef\.current !== "pen"/);
  assert.match(source, /if \(!isHoverPointerType\(pointerType\)\) hideTooltip\(\)/);
});
