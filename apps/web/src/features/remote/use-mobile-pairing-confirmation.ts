import { useCallback, useRef, useState } from "react";
import type { MobilePairingClaim } from "./mobile-pairing-request";
import { useMobilePairingStatusPoll } from "./use-pairing-status-polls";

export function useMobilePairingConfirmation(options: {
  pairingID: string;
  request: <T>(path: string) => Promise<T>;
  onConfirmed: (claim: { token: string; instanceID: string }) => void;
  onExpired: () => void;
}) {
  const [pendingClaim, setPendingClaim] = useState({ token: "", instanceID: "" });
  const callbacksRef = useRef({ onConfirmed: options.onConfirmed, onExpired: options.onExpired });
  callbacksRef.current = { onConfirmed: options.onConfirmed, onExpired: options.onExpired };

  const beginConfirmation = useCallback((result: MobilePairingClaim) => {
    if (!result.accessToken) return;
    setPendingClaim({ token: result.accessToken, instanceID: result.instanceId || "" });
  }, []);

  const clearConfirmation = useCallback(() => {
    setPendingClaim({ token: "", instanceID: "" });
  }, []);

  useMobilePairingStatusPoll({
    pendingToken: pendingClaim.token,
    pairingID: options.pairingID,
    instanceID: pendingClaim.instanceID,
    request: options.request,
    onConfirmed: () => {
      const claim = pendingClaim;
      clearConfirmation();
      callbacksRef.current.onConfirmed(claim);
    },
    onExpired: () => {
      clearConfirmation();
      callbacksRef.current.onExpired();
    },
  });

  return { beginConfirmation, clearConfirmation, pendingClaim };
}
