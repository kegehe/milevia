import { useEffect, useRef } from "react";

type Load = (options?: { force?: boolean; silent?: boolean }) => Promise<unknown>;

export function useRemoteSnapshotRefresh(options: {
  load: Load;
  hasPendingAgentProbe: boolean;
}) {
  const loadRef = useRef(options.load);
  loadRef.current = options.load;

  useEffect(() => { void loadRef.current(); }, [options.load]);

  useEffect(() => {
    if (!options.hasPendingAgentProbe) return;
    const timer = window.setTimeout(() => { void loadRef.current({ force: true, silent: true }); }, 3000);
    return () => window.clearTimeout(timer);
  }, [options.hasPendingAgentProbe]);
}
