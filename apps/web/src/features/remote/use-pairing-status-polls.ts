import { useEffect, useRef } from "react";

type MobilePollOptions = {
  pendingToken: string;
  pairingID: string;
  instanceID: string;
  request: <T>(path: string) => Promise<T>;
  onConfirmed: () => void;
  onExpired: () => void;
};

export function useMobilePairingStatusPoll({ pendingToken, pairingID, instanceID, request, onConfirmed, onExpired }: MobilePollOptions) {
  const callbacksRef = useRef({ onConfirmed, onExpired });
  callbacksRef.current = { onConfirmed, onExpired };
  useEffect(() => {
    if (!pendingToken || !pairingID) return;
    let cancelled = false;
    const poll = () => {
      void request<{ status: string }>(`/v1/pairings/${encodeURIComponent(pairingID)}/status`)
        .then((state) => {
          if (cancelled) return;
          if (state.status === "confirmed") callbacksRef.current.onConfirmed();
          else if (["expired", "cancelled"].includes(state.status)) callbacksRef.current.onExpired();
        })
        .catch(() => undefined);
    };
    poll();
    const timer = window.setInterval(poll, 1500);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [pendingToken, pairingID, instanceID, request]);
}

type DesktopPollOptions = {
  enabled: boolean;
  pairingID: string;
  confirmed: boolean;
  request: <T>(path: string) => Promise<T>;
  onStatus: (status: string) => void;
};

export function useDesktopPairingStatusPoll({ enabled, pairingID, confirmed, request, onStatus }: DesktopPollOptions) {
  const onStatusRef = useRef(onStatus);
  onStatusRef.current = onStatus;
  useEffect(() => {
    if (!enabled || !pairingID.trim() || confirmed) return;
    let cancelled = false;
    const poll = () => {
      void request<{ status?: string }>(`/api/remote/pairing/status?pairingId=${encodeURIComponent(pairingID.trim())}`)
        .then((state) => { if (!cancelled) onStatusRef.current(String(state?.status || "")); })
        .catch(() => undefined);
    };
    poll();
    const timer = window.setInterval(poll, 2000);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [enabled, pairingID, confirmed, request]);
}
