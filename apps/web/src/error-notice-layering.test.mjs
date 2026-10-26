import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";

// 全局操作错误横幅（.error）曾被弹窗遮罩盖住：任务编辑、要求修改、保存定时任务
// 等弹窗里提交失败时，提示落在页面右上角却压在 .backdrop(30) 之下，几秒后消失，
// 看起来像按钮没反应。这里把「错误横幅必须高于所有全屏遮罩层」固化成断言：
// 既扫名字里带 backdrop 的层，也扫任何 position:fixed + inset:0 的层，
// 以后新增 overlay / sheet 之类换了名字的遮罩，或调整层级把横幅埋下去，会立刻失败。
const stylesDir = new URL("./", import.meta.url);

async function cssFiles() {
  const entries = await readdir(stylesDir, { recursive: true, withFileTypes: true });
  return entries
    .filter((entry) => entry.isFile() && entry.name.endsWith(".css"))
    .map((entry) => join(entry.parentPath, entry.name));
}

// 剥掉注释，避免注释里出现的 `.error {` / `backdrop` 字样被当成规则参与解析。
function ruleFor(source, selectorPattern) {
  const css = source.replace(/\/\*[\s\S]*?\*\//g, "");
  return css.match(new RegExp(`${selectorPattern}\\s*\\{[^}]*\\}`))?.[0] ?? null;
}

function zIndexIn(rule) {
  const zIndex = rule.match(/z-index:\s*(-?\d+)/);
  return zIndex ? Number(zIndex[1]) : null;
}

test("全局错误横幅压在所有全屏遮罩之上", async () => {
  const globalStyles = await readFile(new URL("./style.css", import.meta.url), "utf8");
  const errorRule = ruleFor(globalStyles, String.raw`\.error`);
  assert.ok(errorRule, ".error 规则应存在");
  // 只有定位元素参与 z-index 比较；改成 static 后这条回归会直接失效。
  assert.match(errorRule, /position:\s*fixed/, ".error 应保持 fixed 定位");
  const errorZIndex = zIndexIn(errorRule);
  assert.ok(errorZIndex !== null, ".error 应显式声明 z-index");

  const overlayZIndexes = [];
  for (const file of await cssFiles()) {
    // 同样先剥注释：注释里提到 backdrop 时不能被当成选择器参与扫描。
    const source = (await readFile(file, "utf8")).replace(/\/\*[\s\S]*?\*\//g, "");
    for (const match of source.matchAll(/([^{}]*)\{([^}]*)\}/g)) {
      const selector = match[1];
      const declarations = match[2];
      const isBackdrop = /backdrop/i.test(selector);
      // 全屏固定层：不管类名叫什么（overlay / sheet / drawer...），都会盖住横幅。
      const isFullScreen = /position:\s*fixed/.test(declarations) && /inset:\s*0(?:px)?\s*(?:;|$)/.test(declarations);
      if (!isBackdrop && !isFullScreen) continue;
      const zIndex = zIndexIn(declarations);
      if (zIndex !== null) overlayZIndexes.push({ file, zIndex, selector: selector.trim() });
    }
  }

  assert.ok(overlayZIndexes.length > 0, "应能扫描到弹窗遮罩层级");
  for (const overlay of overlayZIndexes) {
    assert.ok(
      errorZIndex > overlay.zIndex,
      `.error (z-index: ${errorZIndex}) 必须高于 ${overlay.file} 里的 ${overlay.selector} (z-index: ${overlay.zIndex})`,
    );
  }
});
