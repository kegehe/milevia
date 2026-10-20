import { useCallback, useRef, useState } from "react";

export type RemoteInstance = {
  instanceId: string;
  name: string;
  status: string;
  lastSeenAt?: string;
};

export type InstanceLoadResult<T extends RemoteInstance> = {
  instances: T[];
  outcome: "ok" | "failed" | "superseded";
  reason?: string;
};

export function useRemoteInstances<T extends RemoteInstance>(options: {
  readToken: () => string;
  scopeKey?: unknown;
  request: () => Promise<T[]>;
  patchDevice: (token: string, value: { instanceId: string; name: string; status: string; lastSeenAt: string }) => unknown;
  reportError: (message: string) => void;
}) {
  const [instances, setInstances] = useState<T[]>([]);
  const [instanceID, setInstanceID] = useState("");
  const requestGenerationRef = useRef(0);
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const loadInstances = useCallback(async (): Promise<InstanceLoadResult<T>> => {
    const current = optionsRef.current;
    const requestGeneration = ++requestGenerationRef.current;
    const superseded: InstanceLoadResult<T> = { instances: [], outcome: "superseded" };
    const requestToken = current.readToken();
    const requestScope = current.scopeKey;
    if (!requestToken) {
      if (requestGeneration !== requestGenerationRef.current) return superseded;
      setInstances([]);
      setInstanceID("");
      return { instances: [], outcome: "ok" };
    }
    current.reportError("");
    try {
      const result = await current.request();
      if (requestGeneration !== requestGenerationRef.current || requestToken !== optionsRef.current.readToken() || requestScope !== optionsRef.current.scopeKey) return superseded;
      const nextInstances = Array.isArray(result) ? result : [];
      setInstances(nextInstances);
      setInstanceID((current) => nextInstances.some((item) => item.instanceId === current)
        ? current
        : nextInstances[0]?.instanceId || "");
      if (nextInstances.length > 0) {
        const info = nextInstances[0];
        current.patchDevice(requestToken, {
          instanceId: info.instanceId || "",
          name: info.name || "",
          status: info.status || "",
          lastSeenAt: info.lastSeenAt || "",
        });
      }
      if (!Array.isArray(result)) current.reportError("云端返回了无效的电脑实例列表");
      return { instances: nextInstances, outcome: "ok" };
    } catch (cause) {
      if (requestGeneration !== requestGenerationRef.current || requestToken !== optionsRef.current.readToken() || requestScope !== optionsRef.current.scopeKey) return superseded;
      const reason = cause instanceof TypeError ? "无法连接云端，请确认 keyanjia.info:8443 已放行并可访问" : cause instanceof Error ? cause.message : "无法加载电脑实例";
      current.reportError(reason);
      return { instances: [], outcome: "failed", reason };
    }
  }, []);

  return { instances, setInstances, instanceID, setInstanceID, loadInstances };
}
