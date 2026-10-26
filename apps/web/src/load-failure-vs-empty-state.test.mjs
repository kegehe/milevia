import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

// 「读取失败」不能显示成「没有数据」回归。
//
// SSH 连接页、SSH Profile 下拉与通知中心都曾把请求失败吞进 catch，随后因为列表是
// 空数组而渲染「还没有远程连接」/「暂无未读通知」/ 空下拉。用户会以为本来就没有
// 内容，而不是服务或网络出了问题。这里固化三态（加载中 / 读取失败 / 确实为空）的
// 分支条件、优先级、重试入口、成功后的收尾，以及"旧快照为空 + 刷新失败"的退化路径。

const [sshPage, notificationCenter, styleCss, notificationCss] = await Promise.all([
  readFile(new URL("./pages/SSHManagerPage.tsx", import.meta.url), "utf8"),
  readFile(new URL("./components/NotificationCenter.tsx", import.meta.url), "utf8"),
  readFile(new URL("./style.css", import.meta.url), "utf8"),
  readFile(new URL("./notification.css", import.meta.url), "utf8"),
]);

test("SSH 连接页：读取失败单独成态，不落到空态", () => {
  assert.match(sshPage, /const \[loadError, setLoadError\] = useState\(""\);/);
  assert.match(sshPage, /const \[refreshError, setRefreshError\] = useState\(""\);/);

  // 从未成功加载 → loadError（整块错误态）；已加载过 → refreshError（保留旧列表，
  // 但上次快照为空时 body 仍要显示错误态，不能落回「还没有远程连接」）。
  assert.match(sshPage, /const message = cause instanceof Error && cause\.message \? cause\.message : "读取 SSH 连接失败";/);
  assert.match(sshPage, /if \(!connectionsLoadedRef\.current\) \{\s*setLoadError\(message\);\s*\} else \{/);
  assert.match(sshPage, /setRefreshError\(`连接列表刷新失败：\$\{message\}`\);/);
  assert.match(sshPage, /const loadFailureMessage = loadError \|\| refreshError;/);
  assert.match(sshPage, /const showLoadFailed = Boolean\(loadFailureMessage\) && connections\.length === 0;/);
  assert.match(sshPage, /: showLoadFailed \? <div className="ssh-empty-state is-error" role="alert">/);

  // 顺序即优先级：加载中 / 读取失败 都不能落到「还没有远程连接」。
  const loading = sshPage.indexOf("正在读取SSH连接");
  const failed = sshPage.indexOf(">读取 SSH 连接失败</h3>");
  const empty = sshPage.indexOf("<h3>还没有远程连接</h3>");
  assert.ok(loading >= 0, "加载中分支应存在");
  assert.ok(failed >= 0, "读取失败分支应存在");
  assert.ok(empty >= 0, "确实没有连接时仍要保留空态");
  assert.ok(loading < failed && failed < empty, "空态必须排在加载中与失败态之后");

  // 失败时给的是「重试」，不是「添加SSH连接」——不要引导用户重复添加。
  const failedBlock = sshPage.slice(failed, empty);
  assert.match(failedBlock, /onClick=\{retryLoad\}/);
  assert.doesNotMatch(failedBlock, /添加SSH连接/);

  // 重试必须清掉两种错误并真的重新发请求；请求结束后 loading 必须归位；
  // 成功后必须同时清掉 loadError 与 refreshError。
  assert.match(
    sshPage,
    /const retryLoad = useCallback\(\(\) => \{\s*setLoading\(true\);\s*setLoadError\(""\);\s*setRefreshError\(""\);\s*void loadConnections\(\);/,
  );
  assert.match(sshPage, /if \(mountedRef\.current && requestVersion === connectionsRequestVersion\.current\) setLoading\(false\);/);
  assert.match(sshPage, /connectionsLoadedRef\.current = true;\s*setLoadError\(""\);\s*setRefreshError\(""\);/);

  // 非空旧列表 + 刷新失败：保留列表，但必须有常驻横幅，且不能被表单交互的
  // clearError() 清掉（因此不能复用 localError）。
  assert.match(sshPage, /\{!localError && refreshError && connections\.length > 0 && <div className="ssh-error" role="status">/);

  // 工具栏计数同样不能把失败说成「0 个已保存连接」。
  assert.match(sshPage, /showLoadFailed \? "连接列表读取失败"/);

  // Profile 列表：加载中 / 读取失败 / 有值 三态分开，失败时可重试且不阻塞手工填写。
  assert.match(sshPage, /const \[profileLoadError, setProfileLoadError\] = useState\(""\);/);
  assert.match(sshPage, /const \[profilesLoading, setProfilesLoading\] = useState\(true\);/);
  assert.match(sshPage, /setProfilesLoading\(true\);/);
  assert.match(sshPage, /if \(mountedRef\.current && requestVersion === profilesRequestVersion\.current\) setProfilesLoading\(false\);/);
  assert.match(sshPage, /setProfileLoadError\(cause instanceof Error && cause\.message \? cause\.message : "读取 SSH Profile 失败"\);/);
  assert.match(sshPage, /profilesLoading \? <p className="ssh-form-notice"><span className="ssh-loading-indicator"><\/span>正在读取 SSH Profile…<\/p>/);
  assert.match(sshPage, /profileLoadError \? <div className="ssh-form-notice is-error" role="alert">/);
  assert.match(sshPage, /onClick=\{\(\) => void loadProfiles\(\)\}/);
});

test("通知中心：读取失败单独成态，不落到「暂无未读通知」", () => {
  assert.match(notificationCenter, /const \[loadError, setLoadError\] = useState\(""\);/);
  assert.match(notificationCenter, /const visibleLoadPendingRef = useRef\(false\);/);
  // 每次打开/重试都要标记"本次可见加载尚未成功"，后台刷新顶掉它后失败也才能报错。
  assert.match(notificationCenter, /if \(showLoading\) \{\s*visibleLoadPendingRef\.current = true;\s*setLoading\(true\);\s*setLoadError\(""\);/);
  // 只有"后台刷新 + 已有旧数据 + 本次可见加载已成功"才允许静默。
  assert.match(notificationCenter, /if \(!showLoading && loadedRef\.current && !visibleLoadPendingRef\.current\) return;/);
  assert.match(
    notificationCenter,
    /if \(!showLoading && loadedRef\.current && !visibleLoadPendingRef\.current\) return;[\s\S]*?setLoadError\(cause instanceof Error && cause\.message \? cause\.message : "读取通知失败"\);/,
  );
  // 读取操作真正失败（而非"确定为空"）才进入错误分支。
  assert.match(
    notificationCenter,
    /: loadError \? \(\s*<div className="notification-empty notification-load-failed" role="alert">/,
  );

  const loading = notificationCenter.indexOf("加载中...");
  const failed = notificationCenter.indexOf("通知列表读取失败");
  const empty = notificationCenter.indexOf("<span>暂无未读通知</span>");
  assert.ok(loading >= 0, "加载中分支应存在");
  assert.ok(failed >= 0, "读取失败分支应存在");
  assert.ok(empty >= 0, "确实没有未读通知时仍要保留空态");
  assert.ok(loading < failed && failed < empty, "空态必须排在加载中与失败态之后");

  const failedBlock = notificationCenter.slice(
    notificationCenter.indexOf('className="notification-empty notification-load-failed" role="alert"'),
    empty,
  );
  assert.match(failedBlock, /role="alert"/);
  assert.match(failedBlock, /loadList\(true\)/, "失败态应提供重试入口");

  // 请求结束必须归位 loading（必须在 finally 里，失败路径也要走到）；
  // 成功必须同时清错并结束"本次可见加载未成功"的标记。
  assert.match(notificationCenter, /\.finally\(\(\) => \{\s*if \(seq === listRequestSeq\.current\) setLoading\(false\);\s*\}\);/);
  assert.match(notificationCenter, /visibleLoadPendingRef\.current = false;\s*setNotifications\(list \|\| \[\]\);\s*loadedRef\.current = true;\s*setLoadError\(""\);/);

  // 忽略/全部忽略提交后必须用新快照作废在途响应，否则旧 GET 会把已忽略项写回。
  assert.match(notificationCenter, /setNotifications\(\(prev\) => prev\.filter\(\(n\) => n\.id !== id\)\);[\s\S]*?void loadList\(\);/);
  assert.match(notificationCenter, /setNotifications\(\[\]\);[\s\S]*?void loadList\(\);/);
});

test("两处失败态都有样式，与空态区分开且长错误不横向溢出", () => {
  assert.match(styleCss, /\.ssh-empty-state\.is-error/);
  assert.match(styleCss, /\.ssh-empty-state\.is-error p \{ overflow-wrap: anywhere; \}/);
  assert.match(styleCss, /\.ssh-form-notice\.is-error/);
  assert.match(styleCss, /\.ssh-error button\.ssh-error-retry/);
  assert.match(notificationCss, /\.notification-load-failed/);
  assert.match(notificationCss, /\.notification-load-failed small \{[^}]*overflow-wrap: anywhere;/);
  // 失败态与空态同权重着色，顺序决定谁生效：失败态必须写在空态之后。
  assert.ok(
    notificationCss.indexOf(".notification-load-failed {") > notificationCss.indexOf(".notification-empty {"),
    "notification-load-failed 必须位于 notification-empty 之后",
  );
});
