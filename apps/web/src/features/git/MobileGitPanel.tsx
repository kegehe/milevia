/**
 * 手机端的 Git 视图壳：把适配器、错误落点、返回键层级收在一处，`GitWorkbench` 本身
 * 只多一个 `mobile` 开关（不另写一套界面 —— 差异全由那个开关与 CSS 承担）。
 *
 * 为什么要有这一层，而不是在页面里直接用 `GitWorkbench`：
 *
 *  1. **错误必须有就地落点。** `GitWorkbench` 把所有错误都塞进 `fail`，桌面端接的是
 *     页面级 `setError`。手机上**不能顺手接页面级那条** —— `MobileRemotePage` 里
 *     已经有三处记录了同一个问题（`.mobile-error` 在弹层遮罩之下、用户看不到）。
 *     这里自己接住并渲染在 Git 视图内部。
 *  2. **适配器持有"最近一次拿到的 stateToken"**，所以必须 `useMemo` 起来。
 *     每次渲染重建 = 每次写操作都要多换一次令牌（还没什么害处），
 *     但更重要的是那意味着"这个对象的身份"在组件与测试里都不可依赖。
 *  3. **返回键要问"里面还有没有上一层"**，而那一层只有 `GitWorkbench` 知道。
 *     它通过 ref 暴露 `showTopLevel()`，这里原样转发给页面。
 */
import { forwardRef, useImperativeHandle, useMemo, useRef, useState } from "react";

import { GitWorkbench, type GitWorkbenchHandle } from "./GitWorkbench";
import { createMobileGitRequest, STALE_STATE_NOTICE } from "./mobile-git-request";
import type { MobileRpcTransport } from "../remote/mobile-rpc";
import "../../git.css";

export interface MobileGitPanelHandle {
  /** 退出详情层（diff / 冲突解决）。返回 false 表示已在最外层，宿主该退整个视图。 */
  showTopLevel: () => boolean;
  /** 重新读一遍仓库状态（顶栏 ⋯ 菜单里的「刷新仓库状态」走它）。 */
  reload: () => void;
}

interface MobileGitPanelProps {
  projectId: string;
  conversationId?: string;
  /**
   * 中继发信器。它的**身份**同时编码了"哪台电脑 / 哪个项目 / 哪个工作区" ——
   * 页面在换其中任何一个时都会换一个新的，所以下面只拿它当依赖
   * （再看 projectId 是多余的，而那两处一旦不同步就会静默用错工作区）。
   */
  transport: MobileRpcTransport;
}

export const MobileGitPanel = forwardRef<MobileGitPanelHandle, MobileGitPanelProps>(function MobileGitPanel({ projectId, conversationId, transport }, ref) {
  const workbenchRef = useRef<GitWorkbenchHandle | null>(null);
  const [error, setError] = useState("");
  // 可恢复的提示（目前只有一种：服务端说"仓库状态已变化，已为你刷新"）。
  // 它与 error **分开**：那条提示不该把整屏盖成一个错误条 —— 用户要做的是确认后重试，
  // 不是"出事了"。
  const [notice, setNotice] = useState("");

  const adapter = useMemo(
    () =>
      createMobileGitRequest({
        transport,
        onStale: (message) => setNotice(message),
        // 用户重试成功之后那条绿色提示要自己撤掉，否则它会一直挂在那儿，
        // 而它说的"请确认后再提交"其实早就完成了。
        onRecovered: () => setNotice(""),
      }),
    [transport],
  );

  useImperativeHandle(ref, () => ({
    showTopLevel: () => workbenchRef.current?.showTopLevel() ?? false,
    reload: () => workbenchRef.current?.reload(),
  }), []);

  return <section className="mobile-git" aria-label="Git 工作台">
    {error !== "" && <div className="mobile-git-error" role="alert"><span>{error}</span><button type="button" onClick={() => setError("")} aria-label="关闭错误提示" title="关闭">×</button></div>}
    {notice !== "" && <div className="mobile-git-notice" role="status"><span>{notice}</span><button type="button" onClick={() => setNotice("")} aria-label="关闭提示" title="关闭">×</button></div>}
    <GitWorkbench
      ref={workbenchRef}
      projectID={projectId}
      conversationId={conversationId}
      request={adapter.request}
      // 「仓库状态已变化」那条**不进**错误条：它跑在下面的绿色提示里。
      // 适配器为了让调用方仍能判失败，必须把同一个错误抛出来，所以这里认出那句话并放行 ——
      // 否则用户会同时看到"已为你刷新"和一条红色的"…：Git state changed…"。
      fail={(message) => { if (message !== STALE_STATE_NOTICE) setError(message); }}
      active
      mobile
    />
  </section>;
});
