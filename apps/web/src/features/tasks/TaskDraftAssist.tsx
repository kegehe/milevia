import { ReactNode, useEffect, useRef, useState } from "react";

import { priorityLabels, type Request } from "./task-model";
import { applyTaskDraft, confirmDraftTasks, draftWaitLabel, isDraftAbort, organizeTaskDraft, type TaskDraft, type TaskDraftItem } from "./task-draft";

export type TaskDraftState = { pending: boolean; holdSubmit: boolean };

type Props = {
  projectID: string;
  text: string;
  request: Request;
  disabled?: boolean;
  onUseDescription: (description: string) => void;
  onCreated: () => Promise<void> | void;
  onState?: (state: TaskDraftState) => void;
  fail: (message: string) => void;
  children: (controls: { button: ReactNode; panel: ReactNode }) => ReactNode;
};

export function TaskDraftAssist({ projectID, text, request, disabled = false, onUseDescription, onCreated, onState, fail, children }: Props) {
  const [phase, setPhase] = useState<"idle" | "loading" | "preview">("idle");
  const [draft, setDraft] = useState<TaskDraft | null>(null);
  const [items, setItems] = useState<TaskDraftItem[]>([]);
  const [creating, setCreating] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);

  const questions = (draft?.questions ?? []).map((item) => item.trim()).filter(Boolean);
  const pending = phase === "loading" || creating;
  const splitPreview = phase === "preview" && draft?.mode === "split" && questions.length === 0;
  const holdSubmit = pending || (phase === "preview" && questions.length > 0) || splitPreview;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      abortRef.current?.abort();
    };
  }, []);

  useEffect(() => {
    onState?.({ pending, holdSubmit });
  }, [holdSubmit, onState, pending]);

  const dismiss = () => {
    setPhase("idle");
    setDraft(null);
    setItems([]);
  };

  const organize = async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    setPhase("loading");
    setDraft(null);
    setItems([]);
    try {
      const result = await organizeTaskDraft({ request, projectID, text, signal: controller.signal });
      if (!mountedRef.current || controller.signal.aborted) return;
      const action = applyTaskDraft(result);
      if (action.kind === "single") {
        if (!action.description) {
          setPhase("idle");
          fail("AI 没有给出可用的任务说明");
          return;
        }
        onUseDescription(action.description);
        setItems([]);
      } else if (action.kind === "split") {
        setItems(action.tasks);
      } else {
        setItems([]);
      }
      setDraft(result);
      setPhase("preview");
    } catch (cause) {
      if (!mountedRef.current || controller.signal.aborted || isDraftAbort(cause)) {
        if (mountedRef.current && abortRef.current === controller) setPhase("idle");
        return;
      }
      if (mountedRef.current) {
        setPhase("idle");
        fail(cause instanceof Error ? cause.message : "无法起草任务");
      }
    }
  };

  const cancel = () => {
    abortRef.current?.abort();
    dismiss();
  };

  const removeItem = (key: string) => {
    setItems((current) => current
      .filter((item) => item.key !== key)
      .map((item) => ({ ...item, dependsOn: item.dependsOn.filter((dep) => dep !== key) })));
  };

  const createSplit = async () => {
    if (creating || items.length === 0 || items.some((item) => !item.description.trim())) return;
    setCreating(true);
    try {
      await confirmDraftTasks(request, projectID, items);
      if (!mountedRef.current) return;
      dismiss();
      await onCreated();
    } catch (cause) {
      if (mountedRef.current) fail(cause instanceof Error ? cause.message : "无法创建任务");
    } finally {
      if (mountedRef.current) setCreating(false);
    }
  };

  const button = (
    <button type="button" className="secondary task-draft-trigger" disabled={disabled || pending || splitPreview || !text.trim()} onClick={() => void organize()}>
      {phase === "loading" ? "起草中" : "起草任务"}
    </button>
  );

  let panel: ReactNode = null;
  if (phase === "loading") {
    panel = (
      <div className="task-draft-panel" role="status">
        <p>正在阅读项目并起草…</p>
        <button type="button" className="secondary" onClick={cancel}>取消</button>
      </div>
    );
  } else if (phase === "preview" && draft && questions.length > 0) {
    panel = (
      <div className="task-draft-panel">
        {draft.understood && <p>{draft.understood}</p>}
        <p>还需要补充这些信息，补完后可以再起草一次：</p>
        <ul>{questions.map((question) => <li key={question}>{question}</li>)}</ul>
        <button type="button" className="secondary" onClick={dismiss}>返回修改</button>
      </div>
    );
  } else if (phase === "preview" && draft && draft.mode !== "split") {
    panel = (
      <div className="task-draft-panel">
        <p>已写回任务说明。{draft.understood}</p>
        {draft.assumptions.length > 0 && <ul>{draft.assumptions.map((item) => <li key={item}>{item}</li>)}</ul>}
        <button type="button" className="secondary" onClick={dismiss}>关闭</button>
      </div>
    );
  } else if (splitPreview && draft) {
    panel = (
      <div className="task-draft-panel">
        {draft.understood && <p>{draft.understood}</p>}
        {draft.assumptions.length > 0 && <ul>{draft.assumptions.map((item) => <li key={item}>{item}</li>)}</ul>}
        {items.length === 0 ? <p>至少保留一条任务。</p> : items.map((item, index) => {
          const waiting = draftWaitLabel(item, items);
          return (
            <article className="task-draft-card" key={item.key}>
              <header>
                <b>第 {index + 1} 条 · {priorityLabels[item.priority]}</b>
                <button type="button" onClick={() => removeItem(item.key)} disabled={creating}>删除</button>
              </header>
              {waiting && <small>{waiting}</small>}
              <textarea aria-label={`第 ${index + 1} 条任务说明`} maxLength={12000} value={item.description} disabled={creating} onChange={(event) => {
                const value = event.target.value;
                setItems((current) => current.map((entry) => entry.key === item.key ? { ...entry, description: value } : entry));
              }} />
            </article>
          );
        })}
        <div className="task-draft-actions">
          <button type="button" className="secondary" disabled={creating} onClick={dismiss}>取消</button>
          <button type="button" className="primary" disabled={creating || items.length === 0 || items.some((item) => !item.description.trim())} onClick={() => void createSplit()}>{creating ? "创建中" : items.length > 1 ? "创建这些任务" : "创建这条任务"}</button>
        </div>
      </div>
    );
  }

  return children({ button, panel });
}
