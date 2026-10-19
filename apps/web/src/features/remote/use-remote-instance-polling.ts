import { useEffect, useRef } from "react";

export function useRemoteInstancePolling(options: {
  activeToken: string;
  deviceEpoch: number;
  visible: boolean;
  load: () => Promise<unknown>;
}) {
  const loadRef = useRef(options.load);
  loadRef.current = options.load;

  useEffect(() => {
    if (!options.activeToken.trim()) return;
    void loadRef.current();
    if (!options.visible) return;
    const timer = window.setInterval(() => { void loadRef.current(); }, 5000);
    return () => window.clearInterval(timer);
  }, [options.activeToken, options.deviceEpoch, options.visible]);
}
