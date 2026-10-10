import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 管理页的几条硬要求（docs/42 §8.2、docs/43 §6）：
//   1. "正在读取 / 无法检测 / 真的没有"三档必须分开渲染（工具目录本身也是三态）；
//   2. "不可安装"的各档理由文案各不相同；
//   3. 判据全部来自服务端，页面不重判一遍。
// 这些都属于"合并了也不会报错，但用户会被支去做错事"的类型，所以用源码断言钉住。
//
// **断言前先剥注释**：注释里出现同一个字符串会让 doesNotMatch 空转，也会让 match
// 在"代码已经改坏、注释还留着"时假绿。
//
// ⚠️ 2026-09-23 这一轮把页面从"卡片流"改成了网格工具箱（每个工具一张等宽卡片），
// 同时把**判据搬到了 `lib/cli-tools-view.ts`**。所以本文件里凡是"判据"类的断言都改成了
// **两段式**：① 页面里不许再出现那份判据；② 它必须在模型里、且有真的调用去验它
// （`lib/cli-tools-view.test.ts`）。只写①会退化成"删掉了就算过"。

const raw = await readFile(new URL("./pages/CliToolsPage.tsx", import.meta.url), "utf8");
const css = await readFile(new URL("./pages/cli-tools.css", import.meta.url), "utf8");
const app = await readFile(new URL("./App.tsx", import.meta.url), "utf8");
const dashboard = await readFile(new URL("./pages/DashboardPage.tsx", import.meta.url), "utf8");
const model = await readFile(new URL("./lib/cli-diagnosis.ts", import.meta.url), "utf8");
const view = await readFile(new URL("./lib/cli-tools-view.ts", import.meta.url), "utf8");

function stripComments(source) {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

const code = stripComments(raw);
const viewCode = stripComments(view);
const modelCode = stripComments(model);

test("工具清单来自服务端目录，页面里不写死工具 ID", () => {
  // 页面遍历的是 registry 的目录（连 loaded / error 一起拿：只取 entries 的话，
  // "还没读到"与"读失败"都会渲染成"一个工具都没有"）。
  assert.match(code, /useAgentCatalogState\(\)/);
  assert.match(code, /catalog\.entries\.map\(\(entry\) => \{/);
  for (const forbidden of ['"claude-code"', '"codex"']) {
    assert.ok(!code.includes(forbidden), `页面里出现了写死的工具 ID ${forbidden}`);
  }
});

test("三档状态分开渲染，且「无法检测」不说成「未安装」", () => {
  assert.match(code, /viewState === "loading"/);
  // 通道失败：独立分支、独立文案，并明确否认"未安装"。
  assert.match(code, /viewState === "ready" && view && !view\.probeOk/);
  assert.match(code, /这不是"工具未安装"/);
  // 工具目录自己也是三态：读失败 / 还没读到 / 真的没有。
  assert.match(code, /catalog\.error/);
  assert.match(code, /!catalog\.loaded/);
  assert.match(code, /已读到工具目录，但里面没有工具/);
  // 「还没读到目录」这一档渲染的是**骨架卡**（2026-09-24 起）：那一刻连工具名都还没有，
  // 所以它是全页唯一还该用占位的地方（目录一到就画真卡片）。
  assert.match(code, /!catalog\.loaded[\s\S]{0,90}cli-tools-skeleton/);
  // 反面：不许把"读不到"直接渲染成工具的"未安装"徽标。
  assert.doesNotMatch(code, /probeError[^\n]*未安装/);
});

test("「正在读取」用骨架行，但它必须带一个可读的名字", () => {
  // 纯灰块对读屏和文本断言都是"什么都没有" —— 那就等于把"正在读"写成了"没有"。
  assert.match(code, /cli-tools-skeleton/);
  assert.match(code, /role="status" aria-label="正在读取这台机器上的工具"/);
  assert.match(css, /\.cli-tools-skeleton-card \{/);
});

test("「真的没有」的空态是卡片式且带下一步动作，不是一行灰字", () => {
  assert.match(code, /cli-tools-empty/);
  assert.match(code, /没有可管理的执行环境/);
  // 空态里必须有能点的下一步。
  const block = code.slice(code.indexOf("cli-tools-empty"), code.indexOf("cli-tools-empty") + 600);
  assert.match(block, /<button/, "空态必须给下一步动作");
});

test("能不能安装的判据只有一个来源：模型，而且页面不许再判一遍", () => {
  // ① 模型里那份必须是三件事都看（只看 runtime.installed 的话，Node 16 与 npm 缺失
  //    这两种情况都会亮出"安装"按钮，而服务端的闸门会拒）。
  assert.match(viewCode, /Boolean\(input\.item\?\.installSupported\)/);
  assert.match(viewCode, /Boolean\(input\.runtime\?\.npmVersion\)/);
  assert.match(viewCode, /input\.runtime\?\.meetsMinimumFor\?\.includes\(input\.entry\.id\)/);
  // ② 页面自己不许**判**这件事。判据的形态是 `meetsMinimumFor.includes(id)` 或
  //    `Boolean(runtime?.npmVersion)` 这种"拿它当闸门"的写法；页面只允许把服务端给的
  //    读数**显示**出来（"满足 2 / 3 个工具"、"npm 10.9.2"）。所以这里禁的是判断形态，
  //    而不是这两个字段名 —— 把字段名一律禁掉会让"显示服务端给的读数"也变成违规，
  //    那是另一种形态的错（页面被迫自己再算一遍）。
  assert.doesNotMatch(code, /meetsMinimumFor\?\.includes/, "页面里又判了一遍「够不够用」");
  assert.doesNotMatch(code, /Boolean\(runtime\?\.npmVersion\)/, "页面里又判了一遍「有没有 npm」");
  // 升级那两档更是判断，页面里一次都不该出现。
  // `preflight.installOk` 是 2026-10-09 补进这一列的：页面此前自己写了一遍
  // `preflight && !preflight.installOk`，而那正好是 `preflightNote` 自带守卫的取反 ——
  // 同一件事两处判，且守的那一处（模型）改了，页面这块会把话静默藏掉。
  for (const forbidden of ["autoUpdatable", "upgradeNeedsGrant", "preflight.upgradeOk", "preflight.installOk"]) {
    assert.ok(!code.includes(forbidden), `页面里又出现了判据字段 ${forbidden}（它只该在 lib/cli-tools-view.ts 里）`);
  }
  // ③ 页面确实通过模型算卡片。
  assert.match(code, /buildToolCard\(\{/);
});

test("四档「不可安装」的理由各不相同，且都来自模型（页面不自己拼）", () => {
  assert.match(viewCode, /item\.installBlockedReason/);
  assert.match(viewCode, /需要先安装 Node\.js 运行时/);
  assert.match(viewCode, /随包的 npm 不可用/);
  assert.match(viewCode, /请先升级运行时/);
  // 未授权那一档也是模型里的（不是让界面去认理由文案里有没有"授权"两个字）。
  assert.match(viewCode, /尚未授权在这台主机上安装/);
  assert.doesNotMatch(viewCode, /installBlockedReason\?\.(includes|match)/);
  assert.doesNotMatch(code, /installBlockedReason\?\.(includes|match)/);
});

test("不能应用内升级时给手动命令，不给出点了必失败的按钮", () => {
  // 判据在模型里，一处：`updateDecision`。
  assert.match(viewCode, /function updateDecision/);
  assert.match(viewCode, /if \(!item\.autoUpdatable\)/);
  assert.match(viewCode, /需在目标环境手动执行 \$\{entry\.commandName\} update/);
  // 两条提示互斥："平台不支持升级"与"授权还没给"是两回事，同时出现用户不知道该做哪个。
  assert.match(viewCode, /if \(item\.upgradeNeedsGrant\)/);
  // ⚠️ 这一条是这一轮补的：**预检必须进升级按钮的判据**。原先只看了 autoUpdatable 与
  //    upgradeNeedsGrant，于是"预检说升级也不行"与"升级按钮亮着"同屏，按钮必失败。
  assert.match(viewCode, /if \(preflight && !preflight\.upgradeOk\)/);
  assert.match(viewCode, /canUpdate: update\.primary\?\.kind === "update"/);
  // 反过来：页面上那个按钮只认模型给的 primary，不再自己看任何判据。
  assert.match(code, /card\.primary && <button/);
  assert.doesNotMatch(code, /hasUpdate/);
});

test("有任务在进行时不给操作入口", () => {
  // operation=running 时服务端会返回 409，界面不该亮出按钮。
  assert.match(viewCode, /item\?\.operation === "running"/);
  assert.match(viewCode, /该工具上已有任务在进行/);
});

test("未授权时不给安装/升级按钮，且授权入口只给一处", () => {
  // 跨端安装会在别人的机器上执行 npm 并落一整套运行时，未授权时服务端会 403。
  assert.match(code, /\{!view\?\.remoteInstallAllowed/);
  assert.match(code, /允许在此主机安装/);
  // 授权入口只给一处（运行依赖那一栏）：每个工具再各来一个同义按钮，
  // 未授权的机器上会出现 N+1 个"允许在此主机安装"。
  assert.equal((code.match(/允许在此主机安装/g) ?? []).length, 1);
});

test("安装与升级都要先确认，且确认框里点明目标环境", () => {
  assert.match(code, /pending && <div className="backdrop"/);
  assert.match(code, /目标环境：<b>\{environmentLabel\}<\/b>/);
  assert.match(code, /平台自己的工具链目录/);
  assert.match(code, /不需要管理员权限/);
  assert.match(code, /校验官方 SHA256/);
});

test("路由与首页入口都已接上", () => {
  assert.match(app, /import CliToolsPage from "\.\/pages\/CliToolsPage";/);
  assert.match(app, /<Route path="\/cli-tools" element=\{<CliToolsPage \/>\} \/>/);
  assert.match(dashboard, /navigate\("\/cli-tools"\)/);
  assert.match(dashboard, /<CliToolsIcon \/>/);
  assert.match(dashboard, /function CliToolsIcon\(\)/);
});

test("入口名只有一个来源：首页那两颗字与页面标题一模一样", () => {
  // 名字改过一次（「CLI 工具」→「AI 工具」→「Cli管理」），而两处容易只改一处。
  // 断言钉的是**两处相等**这件事本身，不钉具体名字 —— 下次改名只要两处一起动就仍然绿。
  assert.match(code, /<h1 className="cli-tools-title">([^<]+)<\/h1>/);
  const pageName = code.match(/<h1 className="cli-tools-title">([^<]+)<\/h1>/)[1];
  assert.match(dashboard, new RegExp(`<CliToolsIcon \\/><span>${pageName}</span>`),
    `首页入口写的名字与页面标题不一致（页面是「${pageName}」）`);
  assert.match(dashboard, new RegExp(`title="${pageName}"`), "入口的 title 没跟着改");
  // 反面：旧名字不许在入口上残留（`title` 或那两颗字任意一处）。
  for (const stale of ["AI 工具", "CLI 工具"]) {
    assert.ok(!dashboard.includes(`<span>${stale}</span>`), `入口还写着旧名字「${stale}」`);
    assert.ok(!dashboard.includes(`title="${stale}"`), `入口的 title 还写着旧名字「${stale}」`);
  }
  // 页面标题里也不许再出现旧名字。
  assert.doesNotMatch(code, /<h1[^>]*>[^<]*AI 工具/);
});

test("顶栏：返回是圆钮、标题带终端徽标、只有「重新检查」一颗动作", () => {
  // 2026-09-25 顶栏改版（docs/42 §27，方案 B「单行精致」）：行结构不动，只做层级。
  // 返回收成圆形图标钮（纯导航）：可读名靠 aria-label，不再是一颗文字按钮。
  assert.match(code, /className="cli-tools-back" aria-label="返回首页"/);
  // 标题前有终端徽标（纯装饰，可读名在 h1 —— 徽标不许携带可读名，免得读屏念两遍）。
  assert.match(code, /className="cli-tools-badge" aria-hidden="true"/);
  // 「重新检查」前的 ↻ 是纯装饰（aria-hidden），可读名只在按钮文字上。
  assert.match(code, /cli-tools-refresh-glyph" aria-hidden="true">↻/);
  // CSS 尺寸本身钉死（手感阈值教训：只断言"类名存在"挡不住数值漂移）。
  assert.match(css, /\.cli-tools-back \{[^}]*width: 38px; height: 38px;/);
  assert.match(css, /\.cli-tools-back \{[^}]*border-radius: 50%/);
  assert.match(css, /\.cli-tools-badge \{[^}]*width: 27px; height: 27px;/);
  assert.match(css, /\.cli-tools-title \{ margin: 0; font-size: 17\.5px;/);
  // 顶栏内的样式必须带 `.cli-tools-shell` 前缀：style.css 的 `.dashboard-bar > div`
  // 与 `.dashboard-bar > div > span`（(0,1,1)/(0,1,2)）会压掉无前缀的 (0,1,0) ——
  // 实测 env 轨道 gap 变 16px。2026-09-25 复查修正，这里钉住前缀本身。
  assert.match(css, /\.cli-tools-shell \.cli-tools-bar-actions \{[^}]*gap: 12px/);
  assert.match(css, /\.cli-tools-shell \.cli-tools-env \{[^}]*gap: 2px/);
  assert.ok(!css.includes("\n.cli-tools-env {"), "环境选择器的无前缀选择器还在（gap 会被 .dashboard-bar > div 压成 16px）");
  // 反面：「读数 今天 09:12」那一行已按用户要求删掉（文字与代码都不留，2026-09-25）。
  assert.ok(!code.includes("cli-tools-when"), "顶栏还在渲染读数时刻");
  assert.ok(!code.includes("readingMoment"), "页面又去算读数时刻了");
  assert.ok(!css.includes(".cli-tools-when"), "读数时刻的样式还留着");
});

test("运行依赖仪表行：徽标 + 大数字满足度 + 状态点，竖线分隔全部退场", () => {
  // 2026-09-25 依赖区改版（docs/42 §28，方案 C「仪表行」）。
  // 徽标是纯装饰，可读名在 b 里；「没装」档徽标转灰（data-tone），不冒充"装好了"的绿。
  assert.match(code, /className="cli-tools-dep-mark" aria-hidden="true"/);
  assert.match(code, /cli-tools-dep-mark" data-tone="missing" aria-hidden="true"/);
  // 满足度是这一栏存在的理由：大数字读数。页面只**显示**服务端的数（.length），
  // 不许出现判断形态（meetsMinimumFor.includes 已有别的断言禁着）。
  assert.match(code, /cli-tools-dep-meter">\s*<b>\{runtime\.meetsMinimumFor\?\.length \?\? 0\} \/ \{cards\.length\}<\/b>/);
  // 更新状态是读数不是动作：状态点走 data-tone 三档（ok 绿 / update 琥珀 / unknown 灰，
  // 后者 = 服务端没取到版本索引，latestVersion 为空）。**判据与文案在模型里**
  // （`runtimeDepLine`），页面只把它的两个返回值放上去 —— 2026-09-29 之前页面自己写着
  // `updateAvailable ? "update" : "ok"`，于是"读不到最新版本"被写成"已是最新"。
  assert.match(code, /cli-tools-dep-state" data-tone=\{depLine\?\.tone\}/);
  assert.match(code, /\{depLine\?\.text\}/);
  assert.doesNotMatch(code, /data-tone=\{runtime\.updateAvailable \? "update" : "ok"\}/);
  assert.match(css, /\.cli-tools-dep-state\[data-tone="update"\]/);
  assert.match(css, /\.cli-tools-dep-state\[data-tone="unknown"\]/);
  assert.match(css, /\.cli-tools-dep-mark\[data-tone="missing"\]/);
  // 反面：旧的竖线分隔与「依赖」小标签整体退场，不许残留。
  assert.ok(!code.includes("cli-tools-sep"), "竖线分隔符还在用");
  assert.ok(!code.includes("cli-tools-dep-tag"), "「依赖」小标签还在");
  assert.ok(!css.includes(".cli-tools-sep"), "竖线分隔符的 CSS 规则还在");
  assert.ok(!css.includes(".cli-tools-dep-tag"), "「依赖」小标签的 CSS 规则还在");
  // 授权入口仍在依赖栏下面单独一处（2026-09-24 修的那个 bug，不许回归）。
  assert.match(code, /\{!view\?\.remoteInstallAllowed && <p className="cli-tools-dep-grant">/);
  // 没装分支的原因文案与兜底不许丢（2026-09-25 复查补钉）。
  assert.match(code, /installBlockedReason \|\| "CLI 工具要通过 npm 安装，需要先装好它。"/);
  // 确认弹窗里写具体的下载源与托管位置（2026-09-25 复查修正：这两个事实此前全页零渲染，
  // view.ts 却一直带着字段 —— 「算了但没人读」）。只显示，不判。
  assert.match(code, /runtimeCatalog\?\.source \? <><code>\{runtimeCatalog\.source\}<\/code>/);
  assert.match(code, /runtime\?\.managedPath \? <>（<code>\{runtime\.managedPath\}<\/code>）<\/>/);
});

test("运行依赖那一行：读数与升级按钮同源，且授权门只挡按钮不挡读数（2026-10-09）", () => {
  // 修的是一个用户报得出的症状（未授权的 WSL 上）：那一行写着「可升级到 24.21.0」，
  // 按钮不渲染，而且**那一行一个字都不解释**。根因是判据分裂 —— 页面自己数三个条件
  // （installSupported && updateAvailable && remoteInstallAllowed），而那句读数只看
  // updateAvailable。所以这条用例钉的是**同源**：判据、文案、按钮三件事一起由模型给。
  //
  // ⚠️ 两段式（本文件的老规矩）：① 页面里不许再出现那份判据；② 它必须在模型里、
  // 且有真的调用去验它（lib/cli-tools-view.test.ts 五个状态逐条钉）。
  assert.match(code, /runtimeDepLine\(runtime, Boolean\(view\?\.remoteInstallAllowed\)\)/);
  assert.match(code, /const depAction = depLine\?\.action;/);
  assert.match(code, /\{depAction && <button/);
  assert.match(code, /\{depAction\.label\}/);
  // 动作种类**照模型给的发**，页面不自己写死一个 kind（否则将来依赖条长出第二种动作时，
  // TS 一声不响，页面照样发 install-runtime）。
  assert.match(code, /setPending\(\{ kind: depAction\.kind \}\)/);
  // 反面：页面里不许再有第二份口径。**判据按"页面不许读那个读数"来钉，不认写法的顺序与
  // 空格** —— 2026-10-09 独立复查指出：原先那条 `!code.includes("runtime.installSupported
  // && runtime.updateAvailable")` 只认那一种字面量与那一种先后，把条件换个顺序重写
  // （`runtime.updateAvailable && runtime.installSupported && …`）就整体漏过。
  // 现在 `updateAvailable` / `latestVersion` 都只该在模型里被读，页面一个都不许碰 ——
  // 无论它将来怎么排列组合那三个条件，只要再判一次就会在这里红。
  assert.ok(!code.includes("runtime.updateAvailable"), "页面又自己读 updateAvailable 了（那一行说什么、给不给按钮只能由模型判）");
  assert.ok(!code.includes("runtime.latestVersion"), "页面又自己读 latestVersion 了（按钮文案由模型给）");
  // ⚠️ 授权那一位**必须按 `view` 传**：漏传（或写成恒真的字面量）等于把"未授权"当成
  // "可以升"，于是按钮亮起来而服务端会 403（runtime_install.go:291 那道闸门，
  // 接口层自己拒）。上面那条正则把 `Boolean(view?.remoteInstallAllowed)` 钉死，
  // 写成 `true` 就失配 —— 不必再补一条同义的反面断言。
  //
  // 「没授权所以这颗按钮现在不给」只有一条措辞，两处（升级 / 重装）共用 —— 各写一句
  // 必然漂移成"同一件事两种说法"，而这一页的存在意义正是"别再让用户从噪音里挑信息"。
  assert.match(viewCode, /export const grantNeededReason = "需先授权在这台主机上安装";/);
  // 第二条同族缺陷（同一处修的）：授权门原先挡在**整段** npm 读数上
  // （`… && !runtime.npmVersion && view?.remoteInstallAllowed &&`），未授权时
  // "随包的 npm 不可用"这条机器事实跟着消失 —— 而它与授没授权无关。现在门只挡按钮。
  assert.match(code, /\{runtime\?\.installed && runtime\.installSupported && !runtime\.npmVersion &&\s*<p className="cli-tools-note">/);
  assert.ok(!code.includes("!runtime.npmVersion && view?.remoteInstallAllowed"), "那条 npm 读数又被授权门整段挡掉了");
  assert.match(code, /\{view\?\.remoteInstallAllowed\s*\?\s*<button className="secondary"/);
  assert.match(code, />重装<\/button>/);
  // 收回按钮时照那条缘由说清为什么，不写第二份措辞。
  assert.match(code, /\{grantNeededReason\}（见上）。/);
});

test("页脚那一整块已删，且没有事实因此失联", () => {
  // 2026-09-23 用户指出这三块是噪音：汇总那句、「这个页面是怎么判断的？」及其说明面板、
  // 「本次检查范围：Windows」。删它们的前提是**每一条都另有出口** —— 这个用例同时钉住
  // 那一半：不是"删干净就算过"，而是"删掉的东西在别处仍读得到"。
  for (const dead of ["cli-tools-foot", "cli-tools-scope", "cli-tools-explain"]) {
    assert.ok(!code.includes(dead), `页面里又长回了 ${dead}`);
    assert.ok(!css.includes(dead), `样式里还留着 .${dead}（没人命中的死规则）`);
  }
  assert.doesNotMatch(code, /tallyText|本次检查范围|这个页面是怎么判断的/);
  // 判据层里那对只服务页脚的函数也一并删了（"算了但没人读"本身就是缺陷）。
  assert.doesNotMatch(viewCode, /export function tallyText|export function tally\(/);

  // 出口一：环境名 —— 多环境时顶部的选择器，以及确认框里的「目标环境」。
  assert.match(code, /runnerLabel\(runner\)/);
  assert.match(code, /目标环境：<b>\{environmentLabel\}<\/b>/);
  // 出口二：「这次没查的部分」（runner 级那条的等价物是**逐工具**的 limitations）在抽屉里。
  assert.match(code, /openDiagnosis\.limitations\.length > 0/);
  assert.match(code, /openDiagnosis\.limitations\.map\(/);
  // 出口三：「上次检查」= 抽屉里每份报告的「诊断于 …」（报告是快照，时间不能丢）。
  assert.match(code, /diagnosisMetaLine\(openDiagnosis\)/);
  // 而「没事时每张卡自己写着已是最新」是那句汇总的替代品 —— 它必须在。
  assert.match(viewCode, /已是最新，不用管它/);
  // ⚠️ 唯一**不能**跟着一起删的那块仍然在首屏（全页的"我们不知道"）。
  assert.match(code, /这次没检查成功/);
});

test("执行环境标签只念机器名，页面里不再出现服务端的内部叫法", () => {
  // 服务端的 `name` 是 "Windows Local Runner" / "WSL Local Runner (Ubuntu)" 这种内部叫法，
  // 而这一页只需要回答"是哪台机器"。怎么念的判据在模型里（`runnerLabel`），页面只消费。
  assert.match(viewCode, /export function runnerLabel/);
  assert.match(code, /runnerLabel\(runner\)/);
  // 反面：页面里不许再直接读 `name`（那正是"标签里塞两个同义环境词"的来源）。
  assert.doesNotMatch(code, /runner\.name/, "页面里又出现了服务端的内部叫法");
  // 选择器每一项就是环境名本身，所以那套给 `<small>` 的两行布局必须跟着删掉 ——
  // 留着它就是一条没人命中的死规则。
  assert.doesNotMatch(css, /\.cli-tools-env-item small/);
});

test("先出页面、再填读数：卡片不等 /agents", () => {
  // 用户 2026-09-24 的原话：「应该是先出现页面然后再进行工具检查啊，而不是等等检查完成
  // 之后才出现页面」。判据层的落点是 `reading` 那一档 —— 它与"服务端没给这一行"
  // （`item === undefined`）是**两件不同的事**，合并就等于把"正在读"写成"读不到"。
  assert.match(viewCode, /if \(input\.reading\)/);
  assert.match(viewCode, /card\.currentText = "读取中…"/);
  assert.match(viewCode, /card\.statusText = "正在读取…"/);
  // 页面把"这台机器的读数还没到"整档交给它（含 idle / loading；error 走另一条分支）。
  assert.match(code, /const reading = viewState !== "ready"/);
  assert.match(code, /reading,/);
  // 反面：整页级的「正在读取执行环境…」不许回来 —— 它把页面按在 `/api/runners`
  // 回来为止（实测 1~2s），而执行环境名现在由顶栏选择器自己负责。
  assert.doesNotMatch(code, /正在读取执行环境…/);
  // 骨架只留给"连工具目录都还没到"那一档：那时没有名字可画，只能给占位。
  assert.match(code, /!catalog\.loaded[\s\S]{0,90}cli-tools-skeleton/);
});

test("样式：卡片右上角六档图标各不相同，且「没查成」与「未安装」一眼可分", () => {
  assert.match(css, /\.cli-tools-icon\[data-icon="ok"\]/);
  assert.match(css, /\.cli-tools-icon\[data-icon="update"\]/);
  assert.match(css, /\.cli-tools-icon\[data-icon="alert"\]/);
  assert.match(css, /\.cli-tools-icon\[data-icon="off"\]/);
  assert.match(css, /\.cli-tools-icon\[data-icon="unknown"\]/);
  assert.match(css, /\.cli-tools-icon\[data-icon="loading"\]/);
  // 虚线**只**出现在"没查成"那一档：图标是虚线圆，而「未安装」是实线灰。
  assert.match(css, /\.cli-tools-icon\[data-icon="unknown"\] \{ border-style: dashed/);
  assert.match(css, /\.cli-tools-icon\[data-icon="off"\] \{ border-color: #d4d9d5/);
  // 状态行同样四档分色，"没查成"那一档是虚线灰。
  assert.match(css, /\.cli-tools-status\[data-tone="ok"\]/);
  assert.match(css, /\.cli-tools-status\[data-tone="warn"\]/);
  assert.match(css, /\.cli-tools-status\[data-tone="bad"\]/);
  assert.match(css, /\.cli-tools-status\[data-tone="unknown"\][^}]*dashed/);
  // 动画必须配降级块，且降级块里要**显式列出同一个选择器**（本项目栽过三次）。
  assert.match(css, /@media \(prefers-reduced-motion: reduce\) \{\s*\.cli-tools-icon\[data-icon="loading"\] \{ animation: none; \}/);
  // 窄屏下动作按钮铺满。
  assert.match(css, /@media \(max-width: 680px\)[\s\S]*?\.cli-tools-card-actions \{ flex-direction: column;/);
});

// ── 故障诊断（docs/43） ────────────────────────────────────────────────────

test("诊断结论的判据只有一份，页面不自己判定", () => {
  assert.match(model, /export function diagnosisTone/);
  // 兜底方向必须是"没查成"（unknown），**不能是"没问题"** —— 这是这套文案里最要紧的一句。
  assert.match(model, /default:\s*return "unknown";/);
  assert.match(code, /from "\.\.\/lib\/cli-diagnosis"/);
  // 反过来：页面里不许再有一份结论词汇表（原来那张 DIAGNOSIS_LABELS 表已随首屏不再
  // 显示"结论徽标"而删除，本项目对"算了但没人读"的处置是删掉而不是留着当摆设）。
  // ⚠️ 这里必须用**剥掉注释**的源码：注释里提到这个名字（解释为什么删它）会让
  // doesNotMatch 空转 —— 本文件开头那条纪律就是为这种情况写的。
  assert.doesNotMatch(modelCode, /DIAGNOSIS_LABELS/);
  assert.doesNotMatch(code, /diagnosisBadge|diagnosisLabel/);
});

test("诊断自身三档分开渲染，且「没查成」不说成「没问题」", () => {
  assert.match(code, /diagnoseState === "loading"/);
  assert.match(code, /diagnoseState === "error"/);
  assert.match(code, /这次没检查成功/);
  // 失败那一档必须明确否认"没有问题"——否则用户会以为这台机器一切正常。
  assert.match(code, /这不代表"没有问题"/);
  // 而"没查成"那一句必须留在**首屏**（它是全页唯一必须留在一线的"我们不知道"）。
  const warnIndex = code.indexOf("这次没检查成功");
  const drawerIndex = code.indexOf("cli-tools-detail");
  assert.ok(warnIndex > 0 && drawerIndex > 0 && warnIndex < drawerIndex,
    "「这次没检查成功」必须渲染在详情抽屉之前（它不能跟着排障素材一起被收起来）");
  // 服务端说"这一轮没详查"的工具要被消费，而不是当成"没有问题"。
  assert.match(code, /skippedDiagnoses\[key\]/);
  assert.match(code, /skippedDiagnosisText\(diagnoseChannel\?\.ok \?\? true\)/);
  assert.match(model, /本轮没有详查/);
  assert.match(model, /本轮没有检测这个工具/);
  // 通道状态必须被消费：不消费它，通道坏掉时每个工具都会被盖章"没有可疑迹象"。
  assert.match(code, /setDiagnoseChannel\(\{ ok: result\.probeOk/);
  assert.match(code, /diagnoseChannel\?\.ok === false/);
});

test("检测与列表是两条路：诊断不进列表热路径", () => {
  // 诊断要跑多次子进程、扫目录、读审计，所以它是按需端点，有独立的状态与代际。
  assert.match(code, /\/diagnostics`/);
  assert.match(code, /\/diagnose`/);
  assert.match(code, /diagnoseGeneration/);
  // 进页面后自动跑一次（管理页真的被打开时才深查）。
  // force 由那个触发点从 ref 取用（手动刷新才为 true）—— 见下面那条缓存用例。
  assert.match(code, /void runDiagnostics\(selectedRunner, force\)/);
  // 「最新版本」同理：它是逐工具去问 registry，所以也有独立代际。
  assert.match(code, /latestGeneration/);
  // ⚠️ 但它是**并发**发的，不是串行（2026-09-24 改）：瓶颈是到 registry 的网络，
  //    实测每次 1.9~3.1s，串行就是三条相加（约 8s，冷启动时撞上过一条 14.6s 的，
  //    合计 22.7s）。并发之后只等最慢的那条，先回来的卡先出结果 ——
  //    这正是"读数状态挂在卡片上"的意义。**别把这条改回串行**。
  assert.match(code, /await Promise\.all\(agentIDs\.map\(/);
  assert.doesNotMatch(code, /for \(const agentID of agentIDs\)/);
});

test("只有手动动作才绕过服务端读数缓存", () => {
  // 服务端把读数缓存住了（见 agent_readings.go）：进入页面那条路**不带** force，
  // 否则"一段时间内只检测一次"就落空了。
  assert.match(code, /void loadView\(selectedRunner\);/);
  // 手动动作都带：顶栏「重新检查」、error 重试、通道坏掉后的重试、写操作之后的刷新。
  assert.match(code, /onClick=\{\(\) => void refresh\(true\)\}/);
  assert.match(code, /await refresh\(true\)/);
  assert.match(code, /void loadView\(selectedRunner, true\)/);
  assert.match(code, /void runDiagnostics\(selectedRunner, true\)/);
  // force 必须真的发到三个端点上去 —— 只在页面上留个标记、不发出去等于没做。
  assert.match(code, /withForce\(`\/api\/runners\/\$\{encodeURIComponent\(runnerID\)\}\/agents`, force\)/);
  assert.match(code, /withForce\(`\/api\/runners\/\$\{encodeURIComponent\(runnerID\)\}\/diagnostics`, force\)/);
  assert.match(code, /check-update`, force\)/);
});

test("修复动作全部来自服务端，页面不硬编码动作名、也不重排顺序", () => {
  // 症状上的按钮就是服务端下发的 remedies（连 label/detail 都是服务端给的）。
  assert.match(code, /issue\.remedies\.length > 0/);
  assert.match(code, /issue\.remedies\.map\(\(remedy\) => <button/);
  assert.match(code, /\{remedy\.label\}/);
  // 请求体只复述服务端给的 id。
  assert.match(code, /remedies: action\.remedies\.map\(\(remedy\) => remedy\.id\)/);
  // 反面：页面里不许出现任何一个修复动作 id，也不许自己排序。
  // （`install-runtime` 不算 —— 那是"安装 Node 运行时"这个既有动作的 kind，
  //   与同名 remedy 只是字面撞车。）
  for (const forbidden of ["reinstall", "rebuild-shim", "restore-backup", "cleanup-interrupted"]) {
    assert.ok(!code.includes(forbidden), `页面里出现了硬编码的修复动作 id：${forbidden}`);
  }
  assert.doesNotMatch(code, /remedies\.sort\(/);
  assert.doesNotMatch(code, /remedyOrder/);
});

test("修不了的症状不给按钮，而是说清只能手动处理", () => {
  // 给一个点了没反应的按钮比不给更坏。
  assert.match(code, /这个症状平台不能自动修/);
  assert.match(viewCode, /这个问题平台不能自动修/);
  // 确认框里逐步列出将要做什么（用户点的是"改我机器"的动作）。
  assert.match(code, /cli-tools-confirm-steps/);
  assert.match(code, /服务端只会执行当前诊断允许的动作/);
  // 但**不许宣称执行顺序**：界面拿到的是"症状里出现动作的次序"，真正执行的次序由
  // 服务端 remedyOrder 定（可能正好相反）。说一句自己做不到的承诺比不说更坏。
  assert.doesNotMatch(code, /按下面的顺序执行/);
  assert.match(code, /实际先后由服务端按依赖关系安排/);
});

test("修复后换上新诊断，而不是只弹一句「修复完成」", () => {
  // 服务端回填的是修复后重跑的诊断；直接换上去，用户才能当场看出症状有没有消失。
  assert.match(code, /setDiagnoses\(\(current\) => \(\{ \.\.\.current, \[key\]: result\.diagnosis \}\)\)/);
  assert.match(code, /describeRepair\(result\.applied\)/);
  // 成败**消费服务端算好的那个**（`RepairResult.success`），前端不自己重算一遍 ——
  // 同一条规则在两处各实现一遍，改一处另一处不跟随。
  assert.match(code, /if \(result\.success\) toast\.success\(message\)/);
  assert.match(code, /else toast\.error\(message\)/);
  assert.doesNotMatch(code, /describeRepair\(result\.applied\)\.ok/);
});

test("「修复完成」之外还要说清解决了什么，以及谁没执行", () => {
  // 只说"修复完成"证明不了任何事 —— 差集才是证据（docs/43 §6.3）。
  // ⚠️ 差集必须在**换掉旧诊断之前**算，换完就只剩新报告了。
  assert.match(code, /const note = resolvedSummary\(resolvedIssues\(diagnoses\[key\], result\.diagnosis\)\)/);
  assert.match(code, /setResolvedNotes\(/);
  // ⚠️ 它必须渲染在详情抽屉**之外**：修好之后批量诊断会把该工具判为"不必详查"，
  // diagnosis 被移除 —— 放在抽屉里就永远看不见了，而那一刻正是最该看见它的时候。
  assert.match(code, /\{resolvedNotes\[key\] && <p className="cli-tools-resolved"/);
  const resolvedLine = code.indexOf("cli-tools-resolved");
  const drawerStart = code.indexOf("cli-tools-detail");
  assert.ok(resolvedLine > 0 && drawerStart > 0 && resolvedLine < drawerStart,
    "「已解决 N 项」必须排在详情抽屉之前（否则它被抽屉的渲染条件挡住）");
  assert.match(model, /export function resolvedIssues/);
  assert.match(model, /export function resolvedSummary/);
  // ⚠️ 差集必须挡掉"修复后那份没查成"：否则它会把**所有**旧症状判成"已解决"。
  assert.match(model, /if \(!isConclusiveDiagnosis\(after\)\) return \[\];/);
  // 「没执行」不能被算成「执行失败」——判据在模型里，页面只消费。
  assert.match(model, /!step\.ok && !step\.skipped/);
  assert.match(model, /另有 \$\{skipped\.length\} 个动作没执行/);
  // 跳过的理由不能混进"做成了什么"那一栏（它也带 detail，但那是"为什么没执行"）。
  assert.match(model, /step\.detail && !step\.skipped/);
  assert.match(css, /\.cli-tools-resolved \{/);
  // 而这条提示**只对那一次修复有效**：一发起新动作就要先清掉，否则它会一直挂着，
  // 说一件早已被后续变更推翻的事实。
  assert.match(code, /setResolvedNotes\(\(current\) => \(\{ \.\.\.current, \[staleKey\]: "" \}\)\)/);
});

test("服务端算好的每个字段都有渲染路径上的消费点", () => {
  // 「一份没查成的报告也是零症状」⇒ 空态必须看结论，不能只说"没有发现任何症状"。
  assert.match(code, /diagnosisEmptyText\(openDiagnosis\)/);
  assert.match(model, /export function diagnosisEmptyText/);
  // 报告是**快照**：必须写出什么时候测的，否则旧读数会被当成此刻的事实。
  assert.match(code, /diagnosisMetaLine\(openDiagnosis\)/);
  assert.match(model, /诊断于 \$\{when\}/);
  // 上次失败也要有时间，否则"上周那次"与"刚才那次"分不出来。
  assert.match(code, /diagnoseMoment\(openDiagnosis\.lastFailure\.createdAt\)/);
  // 预检：判据在模型/诊断层，页面只渲染那一句 —— 而且它同时是**升级按钮**的闸门
  // （见 lib/cli-tools-view.ts 的 updateDecision），所以两个字段都真的被读。
  assert.match(code, /preflightNote\(openDiagnosis\.preflight\)/);
  assert.match(model, /export function preflightNote/);
  assert.match(model, /if \(!preflight \|\| preflight\.installOk\) return ""/);
  // 抽屉那块「预检」由**算好的那句话**决定出不出现（有话说才出现），页面不再对着
  // installOk 重判一次（2026-10-09）。等价性由模型那两条用例钉着：
  // `preflightNote(undefined) === ""`、`preflightNote({installOk: true, …}) === ""`。
  assert.match(code, /const openPreflightNote = openDiagnosis \? preflightNote\(openDiagnosis\.preflight\) : "";/);
  assert.match(code, /\{openPreflightNote && <div className="cli-tools-diagnosis-block">/);
  assert.match(code, /\{openPreflightNote\}<\/p>/);
  assert.match(viewCode, /!preflight\.upgradeOk/);
  // 通道状态与 runnerId 也要消费（前者决定 skipped 的说法，后者校验响应归属）。
  assert.match(code, /result\.runnerId !== runnerID/);
  // `meetsMinimumFor` 与 `updateAvailable` 也都要有渲染路径上的消费点（依赖条那一处）。
  // 前者由页面直接读；后者 2026-10-09 起改由模型读（`runtimeDepLine` 拿它与另外两道闸门
  // 一起决定那一行说什么、按钮给不给），页面只渲染结论 —— 所以断言落在模型里。
  assert.match(code, /runtime\.meetsMinimumFor\?\.length/);
  assert.match(viewCode, /runtime\.updateAvailable/);
  assert.match(viewCode, /!runtime\.latestVersion/);
  // 运行时的 `installBlockedReason` 现在有两处出口：没装那一分支，以及"装了但这个环境
  // 装不了运行时"时那句缘由（后者是 2026-10-09 补的 —— 此前装了之后它全页零渲染）。
  assert.match(code, /runtime\?\.installBlockedReason/);
  assert.match(viewCode, /runtime\.installBlockedReason/);
});

test("样式：诊断面板四档观感各不相同，且变体一律走 data-*", () => {
  // 变体一律走 data-*：页面不许再拼动态类名 —— 拼出来的类名一旦取值出乎意料，
  // 就是个没人样式化的死类（而 data-* 至少能被 grep 到）。
  assert.doesNotMatch(code, /cli-tools-diagnosis-\$\{/);
  assert.doesNotMatch(code, /cli-tools-severity-\$\{/);
  assert.doesNotMatch(code, /cli-tools-icon-\$\{/);
  assert.doesNotMatch(code, /cli-tools-card-\$\{/);
  // 说明那一句也有两档观感，走 data-tone（不拼类名）。
  assert.match(code, /className="cli-tools-note" data-tone=\{card\.noteTone\}/);
  assert.match(css, /\.cli-tools-note\[data-tone="warn"\]/);
  assert.match(code, /className="cli-tools-severity" data-severity=\{issue\.severity\}/);
  assert.match(css, /\.cli-tools-severity\[data-severity="blocker"\]/);
  assert.match(css, /\.cli-tools-severity\[data-severity="warning"\]/);
  // 上次失败的原文要能看完（限高可滚），不能因为长就被截掉。
  assert.match(css, /\.cli-tools-failure \{[^}]*overflow: auto/);
  // 窄屏下"所有位置"那张表横向可滚，别把页面撑破。
  assert.match(css, /@media \(max-width: 680px\)[\s\S]*?\.cli-tools-paths \{ display: block; overflow-x: auto; \}/);
});

// ── 授权入口的可见性 ───────────────────────────────────────────────────────

test("授权入口**不藏在「运行时装没装」的分支里**", () => {
  // 2026-09-24 修的缺陷：那个按钮原先嵌在 `runtime?.installed ? … : …` 的 else 分支里，
  // 于是只在"运行时装没装 = 没装"时才渲染。远端机器上 Node 通常早就装好了 ——
  // 后果是每张工具卡都写着「尚未授权…」，而**页面上没有任何地方能授权**，
  // 所有工具因此永远没有「安装」按钮。
  //
  // 钉的是**形状**：它的守卫是单独的 `!view?.remoteInstallAllowed` —— 这种写法结构上
  // 不可能再嵌回那个三元式里（那正是缺陷的成因）。
  // 「只给一处」由上面那条既有用例数按钮文案守着，这里不重复数。
  assert.match(code, /\{!view\?\.remoteInstallAllowed && <p className="cli-tools-dep-grant">/);
  // 反面：那个三元式的 else 分支里不许再出现授权按钮 —— 它现在只给"已授权"才亮的东西
  // （未授权时点「安装运行时」必然 403，不该亮）。2026-09-25 仪表行改版后按钮守卫从
  // `&& <>{…三元}` 收成了单行 `&& runtime?.installSupported &&`，钉的是新形状。
  assert.match(code, /\{view\?\.remoteInstallAllowed && runtime\?\.installSupported &&/);
  // 样式要在（没有样式的类名是个死类）。
  assert.match(css, /\.cli-tools-dep-grant \{/);
});

// ── 手动安装命令（默认折叠 + 一键复制）────────────────────────────────────

test("手动安装命令：默认折叠，命令来自模型，复制走统一入口", () => {
  // ① 默认折叠：原生 <details> **不带 open**（它就是"默认收起"的实现，
  //    加个 open 就变成默认展开了，而这一块是手册、不是这一屏的下一步）。
  assert.match(code, /<section className="cli-tools-body cli-tools-manual">/);
  assert.match(code, /<details>/);
  assert.doesNotMatch(code, /<details[^>]*\sopen/, "「手动安装命令」被改成默认展开了");
  assert.match(css, /\.cli-tools-manual > details > summary \{/);

  // 反面：那两句说明文字已按用户要求删掉（2026-09-25）—— 这一块只留标题与命令清单。
  assert.ok(!code.includes("cli-tools-manual-hint"), "折叠头里那句「平台装不了…」还在");
  assert.ok(!code.includes("装好之后点上面的"), "展开后那句「在 X 里执行…」还在");
  assert.ok(!css.includes(".cli-tools-manual-hint"), "说明文字的样式还留着（类名已成死类）");

  // ② 命令由模型拼，页面不拼命令（判据只有一处；模型那边有真的调用在验它）。
  assert.match(viewCode, /export function buildManualInstallRows/);
  assert.match(viewCode, /npm install -g \$\{entry\.npmPackage\}@latest/);
  assert.match(code, /buildManualInstallRows\(catalog\.entries\)/);
  // 页面里连包名都不该出现 —— 出现就意味着它又在拼命令了。
  // （注：确认框里那句 `将执行 npm install -g …` 是**说明动作**，不是拼命令，故只禁包名。）
  assert.doesNotMatch(code, /npmPackage/, "页面里自己拼了安装命令");

  // ③ 复制走 lib/clipboard 的统一入口，不另写一份（GitWorkbench / ProjectFileTree
  //    里那两份是重构前的重复品，别再长第三份）。
  assert.match(code, /import \{ copyToClipboard \} from "\.\.\/lib\/clipboard"/);
  assert.match(code, /await copyToClipboard\(command\)/);
  // 复制失败**不能**静默：用户会以为复制成功，粘出来是上一次的内容。
  assert.match(code, /无法复制命令/);
  // 反馈走 data-state（本页第 1 条约定：变体不拼类名）。
  assert.match(code, /className="secondary cli-tools-manual-copy"[\s\S]{0,80}data-state=\{manualCopy\[row\.id\]/);
  assert.match(css, /\.cli-tools-manual-copy\[data-state="copied"\]/);
});

test("手动安装命令：有意留在「探测不到」那道门之外", () => {
  // 通道探测不到（WSL 没起来 / SSH 连不上）恰恰是最需要自己上手装的时候，
  // 所以这一块**不能**跟着卡片区一起被 `view?.probeOk` 挡住。
  const sectionAt = code.indexOf('className="cli-tools-body cli-tools-manual"');
  assert.ok(sectionAt > 0, "找不到「手动安装命令」那一块");
  const guard = code.slice(Math.max(0, sectionAt - 120), sectionAt);
  assert.match(guard, /viewState === "ready" && manualRows\.length > 0/);
  assert.ok(!/probeOk/.test(guard), `它被 probeOk 挡住了：${guard}`);
});

test("详情抽屉不开场白；CodeBuddy 那句登录提示只给 CodeBuddy", () => {
  // 抽屉的素材本身就是排查用的，不用再念一遍（2026-09-26 用户点名删掉）。
  assert.doesNotMatch(code, /下面这些是给排查用的/);
  // 那句提示之前对**所有**工具无条件渲染，其它工具的登录面板里它是废话；
  // 但它不能整个删 —— CodeBuddy 走官方交互式授权，后端那条等价指引要等点过
  // 「发起登录」才经 loginInfo.message 出现（agent_login.go），这是事前唯一出口。
  assert.match(code, /loginAgent\.agentID === "codebuddy" && <p className="cli-tools-hintline">提示：CodeBuddy/);
});

// ── 卡片上的动作：主操作 + 需要登录时的「登录」+ 需要时的「详情」─────────────
//
// 背景（2026-10-08 用户报）：卡片上每张都挂一颗「详情」按钮，而抽屉里绝大多数素材
// （路径、证据、日志）在卡片上其实已经有了 —— 对健康卡那颗按钮没有信息量，还会在卡底
// 留下一条空动作条。处置：**健康卡不给「详情」**；需要登录的工具把「登录」摆到卡片上。
//
// 2026-10-09 补：删「详情」后又发现它删过头了 —— 抽屉的主入口只有顶部横幅，而横幅只
// 指向**第一张**命中的问题卡。于是"第 2 张要处理的卡"和 statusTone=unknown 的"没查成"
// 那一档再也没有入口，卡片文案却让用户"按详情里的证据手动处理"。现在：模型用
// `canOpenDetails` 单独说"这张卡的下一步就在抽屉里"（④ 修不了 / ⑤ 没查成），页面据此
// 补一颗「详情」；健康卡照旧不给。抽屉不再是登录的入口。

test("抽屉入口：横幅给唯一那张问题卡，卡片在需要时自己补一颗「详情」", () => {
  // 横幅仍是"当前这张问题卡"的主入口。
  assert.match(code, /setOpenAgent\(banner\.agentID\)/);
  assert.match(code, /看看是什么问题/);
  // 但横幅只指向**第一张**命中的问题卡（bannerFor 用 find）。对"第 2 张要处理的卡"与
  // statusTone=unknown 的"这次没检查成功"档，卡片必须自己留一颗入口 —— 否则卡片文案让
  // 用户"按「详情」里的证据手动处理"，抽屉却没有任何入口。判据由模型给（canOpenDetails）。
  assert.match(code, /\{card\.canOpenDetails && <button type="button" className="secondary"/);
  const detailsAt = code.indexOf("{card.canOpenDetails && <button");
  assert.ok(detailsAt > 0, "找不到卡片上的「详情」入口");
  assert.match(code.slice(detailsAt, detailsAt + 260), /setOpenAgent\(card\.id\)/);
  assert.match(viewCode, /canOpenDetails: false/);
  assert.match(viewCode, /card\.canOpenDetails = true/);
  // ⚠️ 动作区带 border-top 与浅底：三颗按钮（主操作 / 登录 / 详情）**都 absent** 时整条不能
  //    渲染，否则"已是最新、不需要登录、无需详情"那张最常见的健康卡会留下一条空条。
  assert.match(code, /\{\(card\.primary \|\| card\.canLogin \|\| card\.canOpenDetails\) && <div className="cli-tools-card-actions">/,
    "动作区没有做空档守卫，没有动作的卡片会留下一条空条");
});

test("需要登录的工具把「登录」摆在卡片上，判据来自模型", () => {
  // 页面只渲染模型给的 `canLogin`，不自己拼判据（supportsLogin / installed 都不许再出现）。
  assert.match(code, /\{card\.canLogin && <button type="button" className="secondary"/);
  assert.match(viewCode, /canLogin: Boolean\(entry\.supportsLogin\)/);
  assert.doesNotMatch(code, /supportsLogin/, "页面里出现了第二份「能不能登录」的判据");
  // 点开的是登录面板，且开面板前要把上一轮的登录态清掉（否则会带着上一台/上一个工具的结果）。
  const loginAt = code.indexOf("{card.canLogin && <button");
  assert.ok(loginAt > 0, "找不到卡片上的「登录」按钮");
  const block = code.slice(loginAt, loginAt + 500);
  assert.match(block, /setLoginAgent\(\{ agentID: card\.id, name: card\.name \}\)/);
  assert.match(block, /setLoggedIn\(false\)/);
  assert.match(block, /setLoginInfo\(null\)/);
});

test("抽屉里不再重复给「登录」（两个入口做同一件事）", () => {
  const drawerAt = code.indexOf("cli-tools-detail");
  const pendingAt = code.indexOf("cli-tools-confirm-title");
  assert.ok(drawerAt > 0 && pendingAt > drawerAt, "找不到详情抽屉或它后面的确认框");
  // 只取**抽屉那一块**（到确认框为止）—— 后面的登录面板里当然有「发起登录」这些字。
  const drawer = code.slice(drawerAt, pendingAt);
  assert.doesNotMatch(drawer, />登录</, "抽屉里又出现了「登录」—— 它现在只在卡片上");
  assert.doesNotMatch(drawer, /supportsLogin/);
});


// ── 长任务进行中：说清"在做什么、要多久、别再点" ────────────────────────────
//
// 背景（2026-09-26 用户报）：点「升级 Claude Code」后界面报的是超时失败，而服务端其实
// 升级成功了（审计里那条是 succeeded 2.1.266 → 2.1.282）。根因有两层，这里钉的是前端
// 那一层：装/升级是**数十秒到数分钟级**的操作（docs/42 §4.4），而原先点完「确认」的
// 界面是"弹窗关掉、按钮置灰、页面上什么都不说" —— docs/42 §1.1 把「长任务进度：要有
// 进行中与结果」列为待补项，这一组断言就是补上的那一条。
//
// 超时预算那一层在 `api-timeout-retry.test.mjs`（长任务表）。

test("进行中要说清在做什么，而不是只把按钮置灰", () => {
  // ① 有一档"正在跑"的状态，且它带得出**是哪一次操作**（光一个 busy 布尔说不出在忙什么），
  //    以及**跑在哪台机器上** —— 顶栏可以切执行环境，而操作还在原来那台上跑。
  assert.match(code, /const \[running, setRunning\] = useState<\{ action: PendingAction; runnerID: string; runnerText: string \} \| null>\(null\)/);
  assert.match(code, /setRunning\(\{ action, runnerID: selectedRunner, runnerText: environmentLabel \}\)/);
  assert.match(code, /setRunning\(null\)/);
  // 横幅用的环境名是**发起时捕获**的，不是现读选择框（后者在切环境后会说成新环境）。
  assert.match(code, /pendingRunningText\(running\.action, running\.runnerText\)/);
  // 卡片按钮只在"操作发生的那个环境 + 那张卡"上变文案。
  assert.match(code, /running\.runnerID === selectedRunner && "agentID" in running\.action && running\.action\.agentID === card\.id/);
  // ② 横幅消费它，并走 busy 那一档样式 —— **不能**用 bad：那是"出事了"，
  //    而这条说的是"正在做，请等"，画成一样会让用户在正常的升级里以为坏了。
  assert.match(code, /running && <div className="cli-tools-banner" data-tone="busy" role="status">/);
  assert.match(css, /\.cli-tools-banner\[data-tone="busy"\] \{/);
  assert.match(css, /\.cli-tools-banner\[data-tone="busy"\] \.cli-tools-banner-mark \{/);
  // ③ 三处说法来自同一份（各写一遍就会有一天只剩两处改了）。两个动作词都必须在。
  assert.match(code, /function pendingActionText\(action: PendingAction\)/);
  assert.match(code, /正在升级 \$\{agentDisplayName\(action\.agentID\)\}…/);
  assert.match(code, /正在安装 Node\.js 运行时…/);
  assert.match(code, /pendingActionText\(pending\)\.title/);
  // ④ "别再点"要说出来：这几种操作在服务端是串行闸门（beginAgentMaintenance），
  //    重复点击要么被拒（看起来像失败），要么一直转圈。
  assert.match(code, /期间请不要重复点击/);

  // ⑤ ⚠️ 横幅**不能**被 `view?.probeOk` 挡住：正在装的时候通道探测本来就可能没回来，
  //    那恰恰是最需要看见"在跑"的时候（同「手动安装命令」那块）。它挂在页头之后、
  //    cli-tools-body 之前，不在任何 probeOk 分支里。
  const bannerAt = code.indexOf('data-tone="busy" role="status"');
  assert.ok(bannerAt > 0, "找不到进行中横幅");
  const guard = code.slice(Math.max(0, bannerAt - 200), bannerAt);
  assert.ok(!/probeOk/.test(guard), `进行中横幅被 probeOk 挡住了：${guard}`);
});

test("升级成功后报的是**版本变化**，不是一句「已升级」", () => {
  // 用户点这个按钮就是为了让版本号动。只报"已升级"的话，卡片上的版本号万一还没刷新，
  // 他没法分辨"升成功了"与"根本没升" —— 而服务端本来就把两个版本号回给了我们
  // （app.go 的 writeJSON: previousVersion / currentVersion）。
  const updateAt = code.indexOf("/agents/${encodeURIComponent(action.agentID)}/update");
  assert.ok(updateAt > 0, "找不到升级那一档");
  const block = code.slice(updateAt, updateAt + 700);
  assert.match(block, /previousVersion/, "升级那一档没有再读服务端回的 previousVersion");
  assert.match(block, /currentVersion/, "升级那一档没有再读服务端回的 currentVersion");
  assert.match(block, /→ \$\{result\.currentVersion\}/, "没有把 旧版本 → 新版本 说给用户");
  // 服务端没回版本号时退回原话，不能显示 "undefined → undefined"。
  assert.match(block, /: `\$\{name\} 已升级`/);
  // 安装那一档同理：说有版本号就说。
  const installAt = code.indexOf("/agents/${encodeURIComponent(action.agentID)}/install");
  assert.match(code.slice(installAt, installAt + 700), /installed\?\.version/);
});

// 两处**接线**断言（模型侧已有断言，这里钉"页面真的接上了"）：
//   · 横幅按钮：模型给不给 `actionLabel` 是一回事，页面有没有按它渲染是另一回事
//     —— 无条件渲染就等于"没有可修动作也给一颗只能打开抽屉的按钮"（2026-09-29 修）；
//   · 授权链接：桌面端 `target="_blank"` 点不动（两个窗口都 Deny 新窗口），
//     必须走 `ExternalLink`（它 invoke 宿主的 `open_external`）。
// 判据都取自源码文本，改回修复前这两条会红。
test("横幅按钮按 actionLabel 渲染，授权链接走 ExternalLink", () => {
  assert.match(code, /\{banner\.actionLabel && <button className="primary"/, "横幅按钮又变回无条件渲染");
  assert.match(code, /import \{ ExternalLink \} from "\.\.\/components\/ExternalLink"/, "CLI 页没有引入 ExternalLink");
  assert.match(code, /<ExternalLink href=\{loginInfo\.authUrl\}/, "授权链接没走 ExternalLink");
  assert.doesNotMatch(code, /<a href=\{loginInfo\.authUrl\}/, "授权链接又变回裸 <a>");
});
