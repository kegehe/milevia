import { useEffect, useRef } from "react";

export function useTokenClearedListener(onTokenCleared: () => void) {
  const callbackRef = useRef(onTokenCleared);
  callbackRef.current = onTokenCleared;

  useEffect(() => {
    const listener = () => callbackRef.current();
    globalThis.addEventListener("milevia:token-cleared", listener);
    return () => globalThis.removeEventListener("milevia:token-cleared", listener);
  }, []);
}
