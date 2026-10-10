import { useEffect, useLayoutEffect, useRef, useState, useCallback } from "react";
import type { ReactNode } from "react";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import { api } from "../lib/api";
import { downloadFailureReason, downloadPhase, type UpdaterStatus } from "../features/updater/update-view";
import type { RecentConversation } from "../lib/types";
import "./tray-panel.css";

/**
 * 托盘品牌面板。
 *
 * 运行在独立无边框透明窗口 `tray-panel` 中，通过注入的
 * `window.__MILEVIA_TRAY_ACTIONS__` 桥调用 Rust command。
 *
 * 功能：
 * - 显示 Milevia（回到主窗口）
 * - 在线更新：每次面板打开自动检查一次；有新版可点击安装；也可手动再查
 * - 退出
 *
 * 注意：该窗口常驻复用——打开只是 show、失焦关闭只是 hide，组件不会重挂载；
 * 因此"每次打开自动检查"依赖 Rust 在打开面板时发来的 `tray://panel-opened` 事件。
 */

type InstallResult = {
  installed: boolean;
};

/** 级联子菜单弹出时，在窗口右侧额外预留的宽度（容纳浮层而不撑宽一级面板）。 */
const SUBMENU_EXTRA_W = 264;

/** 「更多会话…」二级子菜单的 DOM id（供按钮 aria-controls 指向）。 */
const MORE_SUBMENU_ID = "tray-more-sessions";

type TrayActions = {
  showMain: () => void;
  close: () => void;
  navigateMain?: (path: string) => void;
  quit: () => void;
  restart: () => void;
  resize?: (width: number, height: number) => void;
  resizeKeep?: (width: number, height: number) => void;
  resizeExpand?: (width: number, height: number, shift: number) => void;
  resizeUp?: (width: number, height: number, up: number) => void;
  // 更新相关（Tauri 注入；浏览器/测试环境缺失）
  getUpdaterStatus?: () => Promise<UpdaterStatus>;
  checkForUpdate?: () => Promise<UpdaterStatus>;
  installUpdate?: () => Promise<InstallResult>;
};

declare global {
  interface Window {
    __MILEVIA_TRAY_ACTIONS__?: Partial<TrayActions>;
  }
}

/** 更新行的派生阶段。 */
type UpdatePhase =
  | "idle" // 首次打开事件到达前 / 尚未执行过检查
  | "checking"
  | "downloading" // 后台静默预下载中：不可点，等它备好
  | "available" // 有新版但包没备好（预下载失败/未开始）：点击后现场下载安装
  | "ready" // 包已下载并验签完成：点击即安装
  | "upToDate"
  | "checkError"
  | "installError";

export function TrayPanel() {
  const rootRef = useRef<HTMLDivElement>(null);
  const [phase, setPhase] = useState<UpdatePhase>("idle");
  const [version, setVersion] = useState<string>(""); // 当前版本（从检查结果回填）
  const [available, setAvailable] = useState<string>(""); // 检测到的新版本
  const [message, setMessage] = useState<string>(""); // 失败/说明文案
  const [recentConversations, setRecentConversations] = useState<RecentConversation[]>([]); // 最近历史会话
  const [showMore, setShowMore] = useState(false); // “更多会话”是否展开
  const [panelSide, setPanelSide] = useState<"right" | "left">("right"); // 打开面板时按屏幕空间一次性决定的子菜单方向（hover 阶段不再改）
  const laidRef = useRef(false); // 本次面板是否已完成“一次性布局”（打开时让位一次，hover 不再移动）
  const subRef = useRef<HTMLElement | null>(null); // 二级子菜单根节点，用于测量高度以撑开窗口
  const moreBtnRef = useRef<HTMLButtonElement | null>(null); // 「更多会话…」触发器（收起时把焦点还给它）
  // 最近一次指针输入是不是"有 hover 的鼠标"（触屏/笔没有 hover）。
  // 用来区分"click 是鼠标点的（子菜单已由 hover 打开）"与"键盘/触屏点的（没有 hover 帮忙）"。
  // 默认 false 是刻意的：读屏/AT 合成的 click 往往不带任何指针事件，也不能带 hover 语义 ——
  // 默认 false 让这类 click 走"切换"，而不是被误判成鼠标点击。
  const hoverPointerRef = useRef(false);
  // 向上扩展让位仅执行一次（resizeUp 是“相对当前位置上移”，重复触发会累积上爬）。
  const growShiftedRef = useRef(false);

  const runRef = useRef(0); // 每代自增：辨别我们自己发起的检查 vs 过期/被他者覆盖的结果
  const pollTimer = useRef<number | undefined>(undefined); // 协作轮询的定时器句柄

  // 每次面板打开拉取最近历史会话（跨项目，附带项目名）。失败静默隐藏该区块。
  const loadRecentConversations = useCallback(() => {
    void api<RecentConversation[]>("/api/conversations/recent?limit=13")
      .then((list) => {
        setRecentConversations(Array.isArray(list) ? list : []);
      })
      .catch(() => setRecentConversations([]));
  }, []);

  // 点击会话：收起面板并唤起主窗口跳转到对应项目里的这条会话。
  const openConversation = useCallback((rc: RecentConversation) => {
    window.__MILEVIA_TRAY_ACTIONS__?.close?.();
    const navigate = window.__MILEVIA_TRAY_ACTIONS__?.navigateMain;
    if (navigate) navigate(`/projects/${rc.projectId}/conversations/${rc.id}`);
  }, []);

  // 托盘窗口与主窗口共用同一份 CSS bundle：给 <html> 加 tray-window 标记，
  // 让 tray-panel.css 里针对 html/body/#root 的透明背景与 overflow:hidden
  // 只在本窗口生效，避免主窗口项目总览等页面被全局 overflow:hidden 锁死滚动。
  // useLayoutEffect 保证标记在首帧绘制前加上（窗口即便立刻 show 也不闪实底）。
  useLayoutEffect(() => {
    document.documentElement.classList.add("tray-window");
    return () => document.documentElement.classList.remove("tray-window");
  }, []);

  // 卸载/换代时清理可能挂起的协作轮询定时器。
  useEffect(
    () => () => {
      if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    },
    [],
  );

  // 下载相位的低频续约（在 settleFrom 里经 ref 调用，避免两个 useCallback 互相依赖）。
  const downloadPollRef = useRef<(gen: number) => void>(() => {});

  // 把一个终态（complete / failed）落成面板文案。
  const settleFrom = useCallback((r: UpdaterStatus) => {
    setVersion(r.appVersion);
    const phase = downloadPhase(r);
    // 已备好的包优先于"最近一次检查失败"：包已经验签，能装就是能装
    // （Rust 侧失败时也刻意保住它，见 apply_check_failure）。
    if (r.update && phase === "ready") {
      setAvailable(r.update.version);
      setMessage(r.update.notes ?? "");
      setPhase("ready");
      return;
    }
    if (r.status === "failed") {
      setPhase("checkError");
      setMessage(r.error ?? "更新检查失败，请稍后重试");
      return;
    }
    if (r.update) {
      setAvailable(r.update.version);
      // 静默下载失败时把原因顶到副标题上（Rust 侧已按下载语境本地化）：这一行
      // 平时显示"更新内容"，但失败时用户更需要知道**为什么**没下成。
      setMessage(downloadFailureReason(r) ?? r.update.notes ?? "");
      // 后台静默预下载的相位决定这一行能干什么：下载中不可点，备好了才是"点击安装"。
      setPhase(phase === "downloading" ? "downloading" : "available");
      // ⚠️ 下载是**异步推进**的，而且后台下载刻意不发任何事件（静默）——面板若正好在这一相位
      // 打开，就会一直停在"正在下载…"、按钮也一直不可点，直到用户关掉再打开。
      // 低频续约到它离开这个相位为止（用 ref 间接调用，避免 settleFrom ↔ poll 的循环依赖）。
      if (phase === "downloading") downloadPollRef.current(runRef.current);
      return;
    }
    setPhase("upToDate");
  }, []);

  // 协作型终态等待：当我们自己发起的 check_for_update_now 返回的是共享态的
  // checking（意味着此刻有**另一并发**检查正持有 Rust 全局状态、将先去 post
  // 终态），就退而本地轮询 get_updater_status（不额外打网络）直到它落出 checking。
  // 轮询只在“自己不再是最新”时触发，通常一步即中；上限防御异常卡死。
  const pollToTerminal = useCallback(
    (gen: number) => {
      const get = window.__MILEVIA_TRAY_ACTIONS__?.getUpdaterStatus;
      if (!get) {
        // 无轮询通道：无法分辨他者终态，回到可重试的错误态（下次点击会再触发）。
        if (gen === runRef.current) {
          setPhase("checkError");
          setMessage("仍在检查中，请点击重试");
        }
        pollTimer.current = undefined;
        return;
      }
      Promise.resolve(get())
        .then((r): void => {
          if (gen !== runRef.current) return; // 已有新的一次检查接管，停止轮询
          pollTimer.current = undefined;
          if (r.status === "complete" || r.status === "failed") {
            settleFrom(r); // 他者已 posting 终态（结果与本次检查等价），采用之
            return;
          }
          // 仍是 checking：可能刚发起、网络很慢或 45s 超时临近；继续等一小段，
          // 但绑上限，避免检查阻塞时无限轮询。
          let attempts = 0;
          const tick = () => {
            if (gen !== runRef.current) return;
            attempts += 1;
            Promise.resolve(get())
              .then((next) => {
                if (gen !== runRef.current) return;
                if (next.status === "checking") {
                  if (attempts < 220) {
                    pollTimer.current = window.setTimeout(tick, 200); // ~45s 上限后放弃
                    return;
                  }
                  setPhase("checkError");
                  setMessage("仍在检查中，请点击重试");
                  return;
                }
                settleFrom(next);
              })
              .catch(() => {
                if (gen !== runRef.current) return;
                pollTimer.current = undefined;
                setPhase("checkError");
                setMessage("仍在检查中，请点击重试");
              });
          };
          pollTimer.current = window.setTimeout(tick, 200);
        })
        .catch(() => {
          if (gen !== runRef.current) return;
          pollTimer.current = undefined;
          setPhase("checkError");
          setMessage("仍在检查中，请点击重试");
        });
    },
    [settleFrom],
  );

  // 下载相位的续约轮询：低频问 get_updater_status（纯本地共享态，不打网络），
  // 直到它离开 downloading 为止 —— 然后交给 settleFrom 按新相位重画（ready / available / failed）。
  //
  // 为什么需要它：后台静默预下载**刻意不发任何事件**，面板若正好在下载期间打开就会一直
  // 停在"正在下载…"、按钮也不可点（用户只能关掉再打开）。间隔取 1 秒而不是 pollToTerminal
  // 那样的 200ms：下载以秒计，而面板一关轮询就随组件一起停（pollTimer 在卸载时清理）。
  // 上限 2 分钟只是兜底，超了就停在当前相位，重开面板会重新查一次。
  const pollDownloadPhase = useCallback(
    (gen: number) => {
      const get = window.__MILEVIA_TRAY_ACTIONS__?.getUpdaterStatus;
      if (!get) return; // 无轮询通道：保持现状（重开面板会重新查）
      let attempts = 0;
      const tick = () => {
        if (gen !== runRef.current) return; // 已有新的一次检查接管
        attempts += 1;
        Promise.resolve(get())
          .then((next: UpdaterStatus) => {
            if (gen !== runRef.current) return;
            if (downloadPhase(next) === "downloading") {
              if (attempts < 120) {
                pollTimer.current = window.setTimeout(tick, 1000);
                return;
              }
              // 到上限就**停住**，不要走 settleFrom：它在下载相位里会再触发一次续约，
              // 于是到达上限后立刻开新一轮，等于把上限变成了摆设。停在当前相位即可，
              // 重开面板会重新查一次。
              pollTimer.current = undefined;
              return;
            }
            pollTimer.current = undefined;
            settleFrom(next);
          })
          .catch(() => {
            // 读共享态失败不该把面板弄成错误态：停在"正在下载…"，重开面板会重新查。
            pollTimer.current = undefined;
          });
      };
      pollTimer.current = window.setTimeout(tick, 1000);
    },
    [settleFrom],
  );
  useEffect(() => {
    downloadPollRef.current = pollDownloadPhase;
  }, [pollDownloadPhase]);
  // 执行一次版本检查（打开自动 / 手动共用）。调用 Rust 的 check_for_update_now
  // （内部会把它设为共享态的最新一次，并返回本次或他子的终态）。
  // - 返回值非 checking：即为我们（最新）的终态，直接 settle。
  // - 返回值仍 checking：说明某并发（如主窗启动 prime）正在 post 终态 →
  //   交给 pollToTerminal 本地轮询收敛，不额外发起网络。
  const runCheck = useCallback(() => {
    if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    const check = window.__MILEVIA_TRAY_ACTIONS__?.checkForUpdate;
    if (!check) {
      setPhase("checkError");
      setMessage("当前环境不支持在线检查");
      return;
    }
    runRef.current += 1;
    const generation = runRef.current;
    setPhase("checking");
    setAvailable("");
    setMessage("");
    Promise.resolve(check())
      .then((r) => {
        if (generation !== runRef.current) return; // 已发新一轮检查，丢弃过期
        if (r.status === "checking") {
          pollToTerminal(generation); // 他者并发正在 post，协作轮询拿终态
          return;
        }
        settleFrom(r);
      })
      .catch((error: unknown) => {
        if (generation !== runRef.current) return;
        setPhase("checkError");
        setMessage(error instanceof Error ? error.message : "更新检查失败，请稍后重试");
      });
  }, [pollToTerminal, settleFrom]);

  // 每次面板打开自动检查一次：监听 Rust 在 open_tray_panel 里发出的
  // `tray://panel-opened`。组件只挂载一次，靠该事件跨次触发。
  useEffect(() => {
    let unlisten: UnlistenFn | undefined;
    let cancelled = false;
    void listen("tray://panel-opened", () => {
      if (!cancelled) {
        // 每次重新打开都重置展开态：方向 panelSide 重新按屏幕判定、一次性布局重做一次
        // （layout 由 recentConversations 变化后的 effect 触发）。收起不依赖 mouseleave，
        // 靠面板失焦自动隐藏来关闭。
        laidRef.current = false;
        growShiftedRef.current = false;
        setPanelSide("right");
        setShowMore(false);
        runCheck();
        void loadRecentConversations();
      }
    }).then((fn) => {
      if (cancelled) fn();
      else unlisten = fn;
    });
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [runCheck, loadRecentConversations]);

  // 安装更新：先隐藏面板，再调用 Rust 的 install_update，成功后 Rust 会重启应用。
  const applyUpdate = useCallback(() => {
    const install = window.__MILEVIA_TRAY_ACTIONS__?.installUpdate;
    const close = window.__MILEVIA_TRAY_ACTIONS__?.close;
    if (!install) return;
    // 结束可能挂起中的协作轮询：安装态与检查态的终态互斥，避免竞相写 state。
    runRef.current += 1;
    if (pollTimer.current !== undefined) window.clearTimeout(pollTimer.current);
    pollTimer.current = undefined;
    setPhase("checking");
    setMessage("");
    close?.();
    Promise.resolve(install()).catch((error: unknown) => {
      // 失败：面板已隐藏，下次打开会自动重查；此处置 installError，重开可由自动重查复位。
      setMessage(error instanceof Error ? error.message : "更新安装失败，请稍后重试");
      setPhase("installError");
      console.error("[tray] update install failed", error);
    });
  }, []);

  const reCheck = useCallback(() => {
    runCheck();
  }, [runCheck]);

  // 内容自适应：一级（.tray-panel）宽不受子菜单影响 —— **所以一级宽必须用 clientWidth 量**：
  // scrollWidth 会把子菜单（absolute 浮层）的溢出算进来（实测 169 → 326），
  // 拿它当"一级宽"再叠加预留宽就是双重计入（窗口宽出一截 + 随展开/收起来回变），
  // 拿它判左右还会把方向判错（见 decideSide / remeasure 里的 ⚠️）。
  // - 面板打开后尚未做一次性布局前：resize(contentWidth)，贴回鼠标锚点。
  // - 布局完成后（laidRef）：始终保持窗口宽 = 一级宽 + 子菜单预留宽，且仅 resizeKeep
  //   （不重新定位）→ 一级与窗口位置在 hover 阶段永不移动。
  // 子菜单（「更多会话…」的浮层）只在会话超过 3 条时才可能出现，它占的是**窗口**宽度的一截。
  // 托盘窗口是 transparent + always_on_top 的：白留的那截会吞掉点击（点它既到不了后面的
  // 窗口、也不会让面板失焦关闭）—— 所以只在它可能出现时才预留。
  //
  // ⚠️ 会话列表**还没回来时保守预留**（首次打开那一瞬间它是空的）：`layFinalSide` 每次打开
  // 只布局一次，若那时按"没有子菜单"收窄，晚到的第 4 条会把子菜单裁在窗口外 ——
  // 裁掉的代价比多留一截大得多。列表到了若 ≤3 条，remeasure 会用 resizeKeep 把那截收掉
  // （只改尺寸、不重新定位，所以不会抖）。
  const submenuReserve = recentConversations.length === 0 || recentConversations.length > 3;

  // 一次性布局：窗口宽度 = 一级宽 + 子菜单宽；左侧时用 margin 把一级钉在鼠标点、
  // 子菜单让到窗口左侧。此后 hover 只显隐，再不发 resize 移动。
  const remeasure = useCallback(() => {
    const el = rootRef.current;
    if (!el) return;
    const r = window.__MILEVIA_TRAY_ACTIONS__;
    if (!r) return;
    const baseH = el.scrollHeight;
    if (baseH <= 0) return;
    if (laidRef.current && r.resizeKeep) {
      // 布局完成后窗口宽恒为一级宽 + （子菜单可能出现时的）预留宽 —— 预留与否只看
      // submenuReserve，不看 hover：hover 阶段宽度必须稳定，不能一进一出就 resize。
      //
      // ⚠️ 宽度取 **clientWidth**（一级面板自身宽）而**不是 scrollWidth**：子菜单是 absolute
      // 浮层，展开时它的溢出**会**被算进 .tray-panel 的 scrollWidth（实测 169 → 326），
      // 于是 scrollWidth + 预留宽 = 双重计入，窗口会比需要宽出一整截 —— 多出来的透明窗口会
      // 吞掉点击，而且随展开/收起来回变（正是上面那句"hover 阶段宽度必须稳定"要挡的事）。
      // clientWidth 不受浮层溢出影响（展开/收起都是 169），两态请求的窗口宽因此完全一致。
      const extra = submenuReserve ? SUBMENU_EXTRA_W : 0;
      const w = el.clientWidth + extra;
      // 高度用 scrollHeight（那是"一级面板自己的内容装不装得下窗口"，浮层不计入）。
      // 面板底部锚定在鼠标点、向上展开：若子菜单比一级高，需要把窗口向上让位
      // （resizeUp，底部不动、额外高度向上生长）避免下越屏被裁。
      let h = baseH;
      let growUp = false;
      if (showMore && subRef.current) {
        const subNeed = subRef.current.getBoundingClientRect().bottom - el.getBoundingClientRect().top + 10;
        if (subNeed > h) {
          growUp = true;
          h = subNeed;
        }
      }
      // resizeUp 是“相对当前位置上移”，重复调用会累积上爬，故仅在首次让位时调用，
      // 后续用 resizeKeep 保持（不重新定位），hover 阶段稳定。
      if (showMore && growUp && r.resizeUp && !growShiftedRef.current) {
        r.resizeUp(w, h, h - baseH);
        growShiftedRef.current = true;
      } else if (!showMore && growShiftedRef.current && panelSide === "right" && r.resize) {
        // 收起子菜单：窗口要从"已向上让位"的位置缩回，而 resizeKeep 只改尺寸、窗口顶缘不动，
        // 会让一级面板悬在鼠标点上方。右侧面板可用 resize 按锚点重贴（左下角回到鼠标点，
        // 即打开时的原位）；左侧面板整体被 resizeExpand 左移让出子菜单区，重贴会把它推回右缘、
        // 再被 --left 的 margin 推右一截，故只在右侧重贴，左侧接受这点上浮余量。
        //
        // 兜底分支：实测当前内容高度下让位根本打不到（子菜单底缘需 410px，一级面板已有 429px），
        // 所以这条与上面那条 resizeUp 一样属于"内容再长一截才会活"的对称保险 —— 留着是为了
        // 不制造"能展开却收不回原位"的半个状态，别当成活代码去改断言。
        growShiftedRef.current = false;
        r.resize(w, h);
      } else {
        // 退出 hover 不重置让位状态：窗口没有“下移回滚”，重复进入时用 resizeKeep
        // 保持在已让位位置+完整高度，既不再次上移累积，也不截断。
        // 面板整体重开（panel-opened）时连同 laidRef 一并复位，允许重新让位。
        r.resizeKeep(w, h);
      }
    } else if (r.resize) {
      growShiftedRef.current = false;
      // 与 remeasure 同一口径：宽度用 clientWidth（此时子菜单必然收起、浮层不在 DOM 里，
      // 两者相等，但统一用 clientWidth 才不会在某天被浮层溢出悄悄改宽）。
      r.resize(el.clientWidth, baseH);
    }
    // 预留宽只在 submenuReserve 变化时改变（会话数跨过 3 条），它是 w 的一部分 ——
    // 所以必须进依赖：少了它，remeasure 会用创建时的旧值算宽度，列表晚到时收不掉那一截。
    // panelSide 进依赖：收起子菜单时要不要重贴窗口由它决定。
  }, [showMore, submenuReserve, panelSide]);
  useEffect(() => {
    const raf = requestAnimationFrame(remeasure);
    return () => cancelAnimationFrame(raf);
  }, [remeasure, phase, available, message, version, showMore, recentConversations]);

  // Esc：先收起已展开的「更多会话」，没有展开时才关面板（与常见的浮层/折叠语义一致）。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (showMore) {
        setShowMore(false);
        // 焦点若在折叠区里（Tab 进去过），收起会把那个节点 display:none 掉 → 焦点掉回 body、
        // 下次 Tab 从头开始。把焦点还给触发器，键盘用户才不丢位置。
        if (subRef.current?.contains(document.activeElement)) moreBtnRef.current?.focus();
        return;
      }
      window.__MILEVIA_TRAY_ACTIONS__?.close?.();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [showMore]);

  // —— 渲染 ——
  const showMain = () => window.__MILEVIA_TRAY_ACTIONS__?.showMain?.();

  // 「更多会话…」是个折叠触发器：鼠标移入即展开（hover 只对鼠标成立，触屏补发的
  // pointerenter 不算），键盘 Enter/Space 与触屏点按则切换展开态。
  // 展开态的读写都只走 setShowMore，所以收起路径（Esc / 再点一次）与展开路径同源。
  const openMoreOnHover = useCallback((event: React.PointerEvent<HTMLButtonElement>) => {
    if (event.pointerType !== "mouse") return;
    hoverPointerRef.current = true;
    setShowMore(true);
  }, []);
  const markHoverPointer = useCallback((event: React.PointerEvent<HTMLButtonElement>) => {
    hoverPointerRef.current = event.pointerType === "mouse";
  }, []);
  const toggleMore = useCallback((event: React.MouseEvent<HTMLButtonElement>) => {
    // 键盘（Enter/Space）激活的 click 其 detail 为 0，触屏/笔/AT 合成的 click 没有 hover ——
    // 这几类都必须"点一下即切换"。只有"鼠标点击"这一种不切换：指针已经在行上、hover 早已
    // 展开，把它翻成收起会像"点一下刚展开的菜单就没了"。
    const clickedByHoveringMouse = event.detail > 0 && hoverPointerRef.current;
    setShowMore((value) => (clickedByHoveringMouse ? true : !value));
  }, []);

  // 打开面板时按屏幕空间决定子菜单放右还是放左（右侧够就右，否则左）。
  // 注意用“一级内容真实宽度”（未含子菜单预留）来计算子菜单右缘，避免用已被扩宽的
  // 窗口宽度导致左右判定失真。取 clientWidth（浮层溢出不算在内），
  // 与 remeasure / layFinalSide 用的是同一个"一级宽"口径。
  const decideSide = useCallback(() => {
    const subW = SUBMENU_EXTRA_W + 16; // 子菜单宽 + 边距，留出余量
    const sx = window.screenX || 0;
    const mainW = rootRef.current?.clientWidth ?? 0;
    const avail = window.screen?.availWidth ?? sx + subW;
    const rightOK = sx + mainW + subW <= avail;
    const leftOK = sx - subW >= 0;
    if (rightOK) return "right";
    return leftOK ? "left" : "right";
  }, []);

  const layFinalSide = useCallback(() => {
    const r = window.__MILEVIA_TRAY_ACTIONS__;
    if (!r) return;
    const el = rootRef.current;
    if (!el) return;
    const extra = submenuReserve ? SUBMENU_EXTRA_W : 0;
    // 方向只在**有预留宽**（= 有子菜单可放）时才有意义。
    // 会话 ≤3 条时预留宽为 0，这里必须判成 right：left 会给一级加 .tray-root--left 的
    // margin-left:264px，而这条路径**不会**调 resizeExpand 让位（窗口宽度也没含预留宽），
    // 于是面板从 264px 处开始画、整个落到 ~171px 宽的窗口之外被裁掉 ——
    // 用户看到的是"右键点了托盘，什么都没出现"（真实可达：托盘图标在屏幕右侧 + 最近会话 ≤3 条）。
    const side = extra > 0 ? decideSide() : "right";
    setPanelSide(side);
    laidRef.current = true;
    const w = el.clientWidth + extra;
    const h = el.scrollHeight;
    if (side === "left" && r.resizeExpand) {
      r.resizeExpand(w, h, extra);
    } else if (r.resizeKeep) {
      r.resizeKeep(w, h);
    }
  }, [decideSide, submenuReserve]);

  // 等最近会话渲染完成且本次尚未布局时，执行一次性布局（每次打开面板只会做一次）。
  useEffect(() => {
    if (laidRef.current) return;
    const raf = requestAnimationFrame(layFinalSide);
    return () => cancelAnimationFrame(raf);
  }, [recentConversations, layFinalSide]);

  // 最近会话条目（主列与二级子菜单共用）。
  const renderRecentItem = (rc: RecentConversation) => (
    <button
      key={rc.id}
      type="button"
      className="tray-panel-item tray-panel-item--recent"
      onClick={() => openConversation(rc)}
      title={`${rc.title} - ${rc.projectName}`}
    >
      <span className="tray-recent-row">
        <span className="tray-recent-title">{rc.title || "未命名会话"}</span>
        <span className="tray-recent-project">{rc.projectName}</span>
      </span>
    </button>
  );

  // 检查中与后台预下载中都是"只读、不可点"，共用同一套淡色观感。
  const busy = phase === "checking" || phase === "downloading";

  const rowClass =
    "tray-panel-item tray-panel-item--update" +
    (busy ? " tray-panel-item--update-spin" : "") +
    (phase === "available" || phase === "ready"
      ? " tray-panel-item--update-new"
      : "") +
    (phase === "checkError" || phase === "installError"
      ? " tray-panel-item--update-error"
      : "");

  let row: ReactNode;
  switch (phase) {
    case "ready":
      row = (
        <button type="button" className={rowClass} onClick={applyUpdate}>
          <span className="tray-update-text">
            <span className="tray-update-label">新版本 v{available} 已就绪，点击安装</span>
            <span className="tray-update-sub">当前 v{version} · 更新包已下载完成</span>
          </span>
        </button>
      );
      break;
    case "downloading":
      row = (
        <button type="button" className={rowClass} disabled aria-busy="true">
          <span className="tray-update-text">
            <span className="tray-update-label">正在下载新版本 v{available}…</span>
          </span>
        </button>
      );
      break;
    case "available":
      row = (
        <button type="button" className={rowClass} onClick={applyUpdate}>
          <span className="tray-update-text">
            <span className="tray-update-label">发现新版本 v{available}，点击更新</span>
            <span className="tray-update-sub">
              当前 v{version}
              {message ? ` · ${message.trim().slice(0, 40)}` : ""}
            </span>
          </span>
        </button>
      );
      break;
    case "upToDate":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">已是最新版本</span>
            <span className="tray-update-sub">当前 v{version} · 点击检查更新</span>
          </span>
        </button>
      );
      break;
    case "checkError":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">检查更新失败，点击重试</span>
            {message && <span className="tray-update-sub">{message}</span>}
          </span>
        </button>
      );
      break;
    case "installError":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          <span className="tray-update-text">
            <span className="tray-update-label">更新失败，点击重试</span>
            {message && <span className="tray-update-sub">{message}</span>}
          </span>
        </button>
      );
      break;
    case "idle":
      row = (
        <button type="button" className={rowClass} onClick={reCheck}>
          检查更新
        </button>
      );
      break;
    default: // checking（含降级兜底）
      row = (
        <button
          type="button"
          className={rowClass}
          disabled
          aria-busy="true"
        >
          正在检查更新…
        </button>
      );
  }

  return (
    <div className={"tray-root" + (panelSide === "left" ? " tray-root--left" : "")}>
      <main className="tray-panel" ref={rootRef}>
        <header className="tray-panel-brand">
        <img className="tray-panel-mark" src="/milevia-mark.svg" alt="" />
        <span className="tray-panel-brand-name">
          <strong>Mile</strong>
          <em>via</em>
        </span>
      </header>

      {recentConversations.length > 0 && (
        <>
          <div className="tray-panel-section-label">最近会话</div>
          <div className="tray-panel-sessions">
            <nav className="tray-panel-sessions-list" aria-label="最近会话">
              {recentConversations.slice(0, 3).map(renderRecentItem)}
              {recentConversations.length > 3 && (
                <span className="tray-more-wrap">
                  <button
                    type="button"
                    ref={moreBtnRef}
                    className="tray-panel-item tray-panel-item--more"
                    // 折叠触发器：aria-expanded 让读屏念出"已展开/已收起"，避免"还有内容"全靠猜；
                    // aria-controls 指向被它展开的那部分（键盘 Enter/Space 与触屏点按都能切换）。
                    aria-expanded={showMore}
                    aria-controls={MORE_SUBMENU_ID}
                    onPointerEnter={openMoreOnHover}
                    onPointerDown={markHoverPointer}
                    onClick={toggleMore}
                  >
                    更多会话…
                  </button>
                  {/* 收起时保留节点、只置 hidden：aria-controls 指向的 id 必须始终存在，
                      否则折叠态下这条 IDREF 悬空。hidden 的 display:none 让它在布局上
                      与"不渲染"完全等价（不占尺寸、不进 Tab 序、读屏不读）。 */}
                  <nav
                    ref={subRef}
                    id={MORE_SUBMENU_ID}
                    hidden={!showMore}
                    className={
                      "tray-session-submenu" +
                      (panelSide === "left" ? " tray-session-submenu--left" : "")
                    }
                    aria-label="更多会话"
                  >
                    <div className="tray-submenu-title">更多会话</div>
                    <div className="tray-session-items">
                      {recentConversations.slice(3).map(renderRecentItem)}
                    </div>
                  </nav>
                </span>
              )}
            </nav>
          </div>
          <div className="tray-panel-sep" />
        </>
      )}

      <nav className="tray-panel-items" aria-label="Milevia 快捷操作">
        <button type="button" className="tray-panel-item" onClick={showMain}>
          显示 Milevia
        </button>

        <div className="tray-panel-sep" />

        {row}

        <div className="tray-panel-sep" />

        <button
          type="button"
          className="tray-panel-item"
          onClick={() => {
            // 先收起面板，再触发重启（避免面板残留/焦点竞争）
            window.__MILEVIA_TRAY_ACTIONS__?.close?.();
            window.__MILEVIA_TRAY_ACTIONS__?.restart?.();
          }}
        >
          重启应用
        </button>

        <button
          type="button"
          className="tray-panel-item tray-panel-item-danger"
          onClick={() => window.__MILEVIA_TRAY_ACTIONS__?.quit?.()}
        >
          退出
        </button>
      </nav>
      </main>
    </div>
  );
}
