import { useState } from "react";
import type { Ask, AskQuestion, ConversationModels, PermissionMode } from "../../lib/types";
import { permissionCopy } from "../../lib/agent-registry";

export function MobileAskCard({ ask, busy, onAnswer, onSkip }: {
  ask: Ask;
  busy: boolean;
  onAnswer: (answers: Record<string, string>, notes: string) => void;
  onSkip: () => void;
}) {
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [other, setOther] = useState<Record<string, string>>({});
  const [notes, setNotes] = useState("");
  const choose = (question: AskQuestion, label: string) => {
    setDraft((current) => {
      if (!question.multiSelect) return { ...current, [question.id]: current[question.id] === label ? "" : label };
      const parts = (current[question.id] || "").split("、").filter(Boolean);
      const next = parts.includes(label) ? parts.filter((part) => part !== label) : [...parts, label];
      return { ...current, [question.id]: next.join("、") };
    });
  };
  return <section className="mobile-ask" aria-label="需要你回答">
    <header><b>需要你回答</b><small>模型在等这个答案，答完这一轮才会继续</small></header>
    {ask.questions.map((question) => <div className="mobile-ask-question" key={question.id}>
      <strong>{question.header ? `${question.header} · ` : ""}{question.question}</strong>
      {question.options && question.options.length > 0 && <div className="mobile-ask-options" role={question.multiSelect ? "group" : "radiogroup"} aria-label={question.question}>
        {question.options.map((option) => {
          const selected = (draft[question.id] || "").split("、").includes(option.label);
          return <button type="button" key={option.label} aria-pressed={selected} className={selected ? "selected" : ""} disabled={busy} onClick={() => choose(question, option.label)}>
            <b>{option.label}</b>
            {option.description && <small>{option.description}</small>}
          </button>;
        })}
      </div>}
      {question.kind === "text" && <textarea value={draft[question.id] || ""} disabled={busy} rows={2} placeholder={question.placeholder || "输入你的回答"} aria-label={question.question} onChange={(event) => setDraft((current) => ({ ...current, [question.id]: event.target.value }))} />}
      {question.kind === "number" && <input value={draft[question.id] || ""} disabled={busy} inputMode="decimal" placeholder={question.placeholder || "输入数字"} aria-label={question.question} onChange={(event) => setDraft((current) => ({ ...current, [question.id]: event.target.value }))} />}
      {question.kind !== "text" && question.kind !== "number" && <input value={other[question.id] || ""} disabled={busy} placeholder="其他（直接输入，优先于上面的选项）" aria-label={`${question.question}的其他回答`} onChange={(event) => setOther((current) => ({ ...current, [question.id]: event.target.value }))} />}
    </div>)}
    <label className="mobile-ask-notes">补充说明
      <textarea value={notes} disabled={busy} rows={2} onChange={(event) => setNotes(event.target.value)} />
    </label>
    <footer>
      <button type="button" disabled={busy} onClick={onSkip}>跳过</button>
      <button type="button" className="primary" disabled={busy} onClick={() => {
        const answers: Record<string, string> = {};
        for (const question of ask.questions) {
          const value = (other[question.id] || "").trim() || (draft[question.id] || "").trim();
          if (value) answers[question.id] = value;
        }
        onAnswer(answers, notes.trim());
      }}>提交回答</button>
    </footer>
  </section>;
}

export function MobileApprovalCard({ detail, busy, onDecide }: {
  detail: string;
  busy: boolean;
  onDecide: (decision: "allow" | "deny") => void;
}) {
  return <section className="mobile-approval" aria-label="等待工具确认">
    <header><b>等待工具确认</b><small>这条命令会在电脑上的项目目录里执行</small></header>
    <p>{detail}</p>
    <footer>
      <button type="button" disabled={busy} onClick={() => onDecide("deny")}>拒绝</button>
      <button type="button" className="primary" disabled={busy} onClick={() => onDecide("allow")}>{busy ? "处理中" : "允许执行"}</button>
    </footer>
  </section>;
}

export function MobileModelSheet({ models, busy, onClose, onSelect }: {
  models: ConversationModels | null;
  busy: boolean;
  onClose: () => void;
  onSelect: (model: string) => void;
}) {
  const [custom, setCustom] = useState("");
  const options = models?.models || [];
  const current = models?.selected || models?.effective || "";
  return <div className="mobile-sheet-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-model-title" onClick={(event) => { if (event.target === event.currentTarget && !busy) onClose(); }}>
    <section className="mobile-sheet">
      <header><h2 id="mobile-model-title">切换模型</h2><button type="button" onClick={onClose} disabled={busy} aria-label="关闭">×</button></header>
      <p className="mobile-sheet-note">{models?.note || "切换后下一条消息生效。会话正在运行时不能换。"}</p>
      {current && <p className="mobile-sheet-current">当前模型：<b>{current}</b></p>}
      <div className="mobile-sheet-list">
        {options.map((option) => <button type="button" key={option.id} disabled={busy} className={current === option.id ? "selected" : ""} onClick={() => onSelect(option.id)}>
          <b>{option.label || option.id}</b>
          {option.description && <small>{option.description}</small>}
        </button>)}
        {options.length === 0 && <p className="mobile-sheet-empty">{models ? "这个工具没有内置模型目录，可以在下面填写模型名。" : "正在读取可用模型…"}</p>}
      </div>
      {models?.customAllowed !== false && <form className="mobile-sheet-custom" onSubmit={(event) => { event.preventDefault(); if (custom.trim()) onSelect(custom.trim()); }}>
        <label htmlFor="mobile-model-custom">自定义模型名</label>
        <div><input id="mobile-model-custom" value={custom} maxLength={128} disabled={busy} placeholder="例如 opus、gpt-5.4" onChange={(event) => setCustom(event.target.value)} /><button type="submit" disabled={busy || !custom.trim()}>使用</button></div>
      </form>}
    </section>
  </div>;
}

export function MobilePermissionSheet({ agentName, modes, current, busy, confirmingFullControl, onClose, onSelect, onConfirmFullControl }: {
  agentName: string;
  modes: PermissionMode[];
  current: string;
  busy: boolean;
  confirmingFullControl: boolean;
  onClose: () => void;
  onSelect: (mode: PermissionMode) => void;
  onConfirmFullControl: () => void;
}) {
  return <div className="mobile-sheet-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-permission-title" onClick={(event) => { if (event.target === event.currentTarget && !busy) onClose(); }}>
    <section className="mobile-sheet">
      <header><h2 id="mobile-permission-title">执行权限</h2><button type="button" onClick={onClose} disabled={busy} aria-label="关闭">×</button></header>
      {confirmingFullControl ? <>
        <p className="mobile-sheet-note">完全控制会允许 {agentName} 直接执行所有命令，不再逐条确认。</p>
        <footer className="mobile-sheet-actions">
          <button type="button" disabled={busy} onClick={onClose}>取消</button>
          <button type="button" className="danger" disabled={busy} onClick={onConfirmFullControl}>{busy ? "切换中" : "确认切换"}</button>
        </footer>
      </> : <>
        <div className="mobile-sheet-list" role="radiogroup" aria-label={`${agentName} 执行权限`}>
          {modes.map((mode) => <button type="button" key={mode} role="radio" aria-checked={current === mode} disabled={busy} className={current === mode ? "selected" : ""} onClick={() => onSelect(mode)}>
            <b>{permissionCopy[mode].title}</b>
            <small>{permissionCopy[mode].detail}</small>
          </button>)}
        </div>
      </>}
    </section>
  </div>;
}
