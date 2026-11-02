import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

import { commitPushMenuHint, commitPushMenuLabel, type CommitPushTarget } from "./git-model";

/** 菜单宽度（px），定位时按它算左右边界。 */
const MENU_WIDTH = 260;
/** 下方至少要留这么多空间，否则翻到上面去展开。 */
const MENU_SPACE = 96;

/**
 * 提交键右边的下拉（**仅桌面端**）。
 *
 * 形态：**主键 + 箭头**的分裂按钮（与会话页的发送键同一套做法：
 * .composer-send-wrap + .composer-send-more）。为什么不做成两个并排的按钮：
 * 「提交」与「提交并推送」不是两个平级动作，而是同一个动作的两种结果 ——
 * 并排摆会让用户以为要先挑一个，分裂按钮才表达得出"主键是提交，箭头是它的另一种结果"。
 *
 * 为什么弹层走 portal + fixed：提交面板在 .git-changes-sidebar 里，
 * 那个容器 overflow: auto —— 绝对定位的菜单会被它裁掉，还跟着侧栏一起滚。
 * 挂到 body 上、用触发键的屏幕坐标定位，两个问题都没有
 * （与 .model-menu / ContextMenu 同一套做法）。
 *
 * 为什么不用原生 <select>：本项目手机端明令不用原生 <select>，
 * 桌面端也要跟"不用原生控件"这条一致，所以自己画。
 */
export function CommitPushMenu({ target, disabledReason, buttonDisabled = false, onPick }: {
  target: CommitPushTarget;
  /**
   * 空串 = 可以推。非空 = 为什么不能。
   *
   * 这个理由要**显示在菜单里**而不是只放在 title 上：禁用的按钮不派发鼠标事件，
   * title 根本不会弹出（提交面板那条 72 字提示也是这么处理的）。
   */
  disabledReason: string;
  /** 整个工作台在忙（提交键那排一起变灰）。 */
  buttonDisabled?: boolean;
  onPick: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState<{ left: number; top?: number; bottom?: number } | null>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const disabled = disabledReason !== "";

  /** 贴着触发键展开：右边对齐触发键，下方放得下就往下，否则往上。 */
  const place = () => {
    const rect = triggerRef.current?.getBoundingClientRect();
    if (!rect) return;
    const left = Math.min(Math.max(12, rect.right - MENU_WIDTH), Math.max(12, window.innerWidth - MENU_WIDTH - 12));
    if (window.innerHeight - rect.bottom < MENU_SPACE && rect.top > window.innerHeight - rect.bottom) {
      setPosition({ left, bottom: Math.max(12, window.innerHeight - rect.top + 8) });
      return;
    }
    setPosition({ left, top: Math.min(rect.bottom + 8, Math.max(12, window.innerHeight - MENU_SPACE)) });
  };

  const toggle = () => {
    if (open) {
      setOpen(false);
      return;
    }
    place();
    setOpen(true);
  };

  // 点外面 / Escape 收摊。滚动也收：菜单是 fixed 的，页面一动它就留在原地，
  // 而触发键已经不在那儿了（会话页的模型菜单也是这么处理的）。
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      const node = event.target as Node | null;
      if (node && (menuRef.current?.contains(node) || triggerRef.current?.contains(node))) return;
      setOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    const onScroll = () => setOpen(false);
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    document.addEventListener("scroll", onScroll, true);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("scroll", onScroll, true);
    };
  }, [open]);

  const label = commitPushMenuLabel(target);
  const hint = disabled ? disabledReason : commitPushMenuHint(target);

  return <>
    <button
      type="button"
      ref={triggerRef}
      className="git-commit-submit-caret"
      aria-haspopup="menu"
      aria-expanded={open}
      aria-label="提交与推送方式"
      title="提交与推送方式"
      disabled={buttonDisabled}
      onClick={toggle}
    ><ChevronDownIcon /></button>
    {open && createPortal(
      <div ref={menuRef} className="git-commit-menu" role="menu" aria-label="提交与推送方式" style={position ?? undefined}>
        <button
          type="button"
          className="git-commit-menu-item"
          role="menuitem"
          disabled={disabled}
          onClick={() => {
            setOpen(false);
            if (!disabled) onPick();
          }}
        >
          <span className="git-commit-menu-icon"><PushIcon /></span>
          <span className="git-commit-menu-text">
            <b>{label}</b>
            <small>{hint}</small>
          </span>
        </button>
      </div>,
      document.body,
    )}
  </>;
}

function ChevronDownIcon() {
  return <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M6 9.5 12 15.5 18 9.5" /></svg>;
}

/** 上箭头：把提交推上去。 */
function PushIcon() {
  return <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M8 13V3" /><path d="M4.5 6.5 8 3l3.5 3.5" /><path d="M3 13.5h10" /></svg>;
}
