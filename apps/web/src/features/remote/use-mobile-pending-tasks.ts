import { useCallback, useRef, useState } from "react";
import { withResolvedCreateID, type PendingTaskMutation } from "../../lib/task-mutations";

export function useMobilePendingTasks() {
  const pendingTaskRef = useRef(new Map<string, PendingTaskMutation>());
  const [pendingTaskRevision, setPendingTaskRevision] = useState(0);
  const [pendingTaskBusy, setPendingTaskBusy] = useState<Set<string>>(() => new Set());
  const markPendingTasksChanged = useCallback(() => setPendingTaskRevision((current) => current + 1), []);
  const setTaskBusy = useCallback((taskID: string, busy: boolean) => {
    setPendingTaskBusy((current) => {
      const next = new Set(current);
      if (busy) next.add(taskID);
      else next.delete(taskID);
      return next;
    });
  }, []);
  const trackPendingTask = useCallback((mutation: PendingTaskMutation) => {
    pendingTaskRef.current.set(mutation.taskId, mutation);
    markPendingTasksChanged();
  }, [markPendingTasksChanged]);
  const notePendingCommand = useCallback((taskID: string, commandId: string) => {
    const mutation = pendingTaskRef.current.get(taskID);
    if (mutation && commandId) pendingTaskRef.current.set(taskID, { ...mutation, commandId });
  }, []);
  const resolvePendingCreate = useCallback((pendingID: string, realID: string, commandId: string) => {
    const mutation = pendingTaskRef.current.get(pendingID);
    if (!mutation || realID === pendingID) return pendingID;
    const resolved = { ...withResolvedCreateID(mutation, realID), commandId };
    if (resolved.taskId === pendingID) return pendingID;
    pendingTaskRef.current.delete(pendingID);
    pendingTaskRef.current.set(resolved.taskId, resolved);
    setTaskBusy(pendingID, false);
    setTaskBusy(resolved.taskId, true);
    markPendingTasksChanged();
    return resolved.taskId;
  }, [markPendingTasksChanged, setTaskBusy]);
  const dropPendingTask = useCallback((taskID: string) => {
    if (pendingTaskRef.current.delete(taskID)) markPendingTasksChanged();
  }, [markPendingTasksChanged]);
  const clearPendingTasks = useCallback(() => {
    const changed = pendingTaskRef.current.size > 0;
    pendingTaskRef.current.clear();
    setPendingTaskBusy(new Set());
    if (changed) markPendingTasksChanged();
  }, [markPendingTasksChanged]);
  return { pendingTaskRef, pendingTaskRevision, pendingTaskBusy, setPendingTaskBusy, markPendingTasksChanged, setTaskBusy, trackPendingTask, notePendingCommand, resolvePendingCreate, dropPendingTask, clearPendingTasks };
}
