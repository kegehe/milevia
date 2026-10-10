// 对话页把"哪个弹窗开着"放在 URL search 里（history / new / usage / execution / config，
// 外加文件页带过来的 addFile）。开、关这些参数的**唯一判据**在这里。
//
// 为什么不直接调 React Router 的 setSearchParams：
//
//   1) 它按**相对路径**解析，基准是调用方闭包里的 location（useNavigate 用渲染期的
//      pathname 拼相对地址）。而这里的调用常常发生在 await 之后 —— loadConversation、
//      newConversation、恢复归档会话的分支 —— 闭包还停在旧会话上，于是算出来的地址把
//      用户推回上一个会话，并把它当时 search 里的弹窗参数一并复活。
//      实测（真浏览器 hook pushState 并抓调用栈）：点「创建会话」后 URL 先 replace 到
//      新会话，88ms 后又被 push 回旧会话 + `?new=true` —— 新会话弹窗重新打开、选择被
//      重置，用户以为没建成，于是反复点，攒出一堆空会话。
//
//   2) 要关的参数本来就不在时，它照样发一次导航，凭空多一条历史记录。上一条的放大器：
//      那次多余的导航正是按旧 location 算出来的。
//
// 所以这里只做**纯计算**：给定"当前 location"与要开/关的参数，返回目标 URL；null 表示
// 不需要导航（已经关着 / 已经开着同一个值）。导航由调用方发（开＝push、关＝replace）。

export type DialogParamLocation = { pathname: string; search: string };

/**
 * 计算开关某个弹窗参数后的目标 URL。
 *
 * @param current 调用时刻**最新**的 location（不是闭包里的旧值）
 * @param name    参数名，例如 "usage"、"execution"
 * @param value   要设成的值；传 null 表示关闭该参数
 * @returns 目标绝对 URL；不需要导航时返回 null
 */
export function dialogParamURL(current: DialogParamLocation, name: string, value: string | null): string | null {
  const params = new URLSearchParams(current.search);
  if (value === null) {
    if (!params.has(name)) return null;
    params.delete(name);
  } else {
    if (params.get(name) === value) return null;
    params.set(name, value);
  }
  const query = params.toString();
  return `${current.pathname}${query ? `?${query}` : ""}`;
}
