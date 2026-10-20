// 优化建议「分析引擎」选择卡（方案 A2 横排矮卡）的接线与规则顺序。
//
// 行为层（点击切换、键盘、验证中禁用）由真实页面承担；这里守的是
// 「图标判据有没有接对、靠顺序生效的规则有没有被挪乱」—— 用扫源码正则验
// 优先级只会自证，所以顺序类断言一律写成 indexOf 比较（与 insights-bulk.test.mjs
// 同一约定：读源码前先统一行尾，CRLF 工作区里写 \n 锚点会静默失配）。
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const normalize = (text) => text.replace(/\r\n/g, "\n");
const panel = normalize(await readFile(new URL("./InsightsPanel.tsx", import.meta.url), "utf8"));
const styles = normalize(await readFile(new URL("./insights.css", import.meta.url), "utf8"));

/** 断言"不该出现"之前先剥注释：注释里原样写出的选择器会把否定断言喂饱。 */
const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
const code = stripComments(panel);
const css = stripComments(styles);

test("图标判据只有一份：走 agentLogoKey + AgentLogo，不在这里自建白名单", () => {
  assert.match(code, /const logo = agentLogoKey\(id\);/);
  assert.match(code, /import \{ agentLogoKey \} from "\.\.\/\.\.\/lib\/cli-tools-view";/);
  assert.match(code, /import \{ AgentLogo \} from "\.\.\/\.\.\/components\/AgentLogo";/);
  // 页面里出现按 id 写死的图标分支 = 第二份判据（那正是 agent-registry 要消灭的写法）。
  assert.doesNotMatch(code, /id === "claude|id === "codex|id === "codebuddy/);
});

test("radio 语义留在 DOM：键盘与读屏不变，选中态样式由 .active 承担", () => {
  assert.match(code, /role="radiogroup"/);
  assert.match(code, /className="insights-agent-radio" name="insight-agent" value=\{id\}/);
  assert.match(code, /disabled=\{anyVerifying\}/);
  // 卡片类名必须由「选中」与「禁用」两个真实读数共同驱动，不许只挂一个。
  assert.match(code, /insights-agent-option\$\{agent === id \? " active" : ""\}\$\{anyVerifying \? " locked" : ""\}/);
});

test("白名单外的工具回落中性图形，不借已知工具的图标冒充身份", () => {
  assert.match(code, /insights-agent-glyph/);
  // 底色档位的诚实默认是中性（#f4f3f0）：品牌档必须显式按 logo key 挂上去。
  assert.match(css, /\.insights-agent-tile \{[\s\S]{0,300}background: #f4f3f0;/);
  assert.match(css, /\.insights-agent-tile\.logo-claude \{[\s\S]{0,120}#fdf1ec;/);
  assert.match(css, /\.insights-agent-tile\.logo-openai \{[\s\S]{0,120}#eaf6f2;/);
});

test("徽标类图标（CodeBuddy）牌位让位，禁用置灰走 filter", () => {
  assert.match(css, /\.insights-agent-tile:has\(img\[data-logo-badge\]\)/);
  // 官方图标是 <img> 不吃 color：整组禁用时的置灰必须自己做。
  assert.match(css, /\.insights-agent-option\.locked \.insights-agent-tile img \{ filter: grayscale\(1\);/);
});

test("同特异性靠顺序生效的规则：active 晚于 hover；locked.active 晚于 locked:hover", () => {
  // .insights-agent-option.active 与 :hover 同为 (0,2,0)，位置颠倒悬停就会盖掉选中态；
  // locked:hover 与 locked.active 同为 (0,3,0)，验证中悬停选中卡时后者必须赢。
  const hover = css.indexOf(".insights-agent-option:hover");
  const active = css.indexOf(".insights-agent-option.active");
  const lockedHover = css.indexOf(".insights-agent-option.locked:hover");
  const lockedActive = css.indexOf(".insights-agent-option.locked.active");
  assert.ok(hover > 0, "悬停规则还在");
  assert.ok(active > hover, "选中态必须晚于悬停规则");
  assert.ok(lockedHover > 0, "禁用悬停规则还在");
  assert.ok(lockedActive > lockedHover, "禁用选中态必须晚于禁用悬停规则");
});

test("旧的原生 radio 胶囊样式已清干净（不留会串味的死规则）", () => {
  assert.doesNotMatch(css, /insights-agent-options label|insights-agent-options input/);
});
