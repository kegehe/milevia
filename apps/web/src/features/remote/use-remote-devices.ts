import { useCallback, useState } from "react";
import {
  addOrReplaceDevice,
  activeDevice,
  reconcileDevices,
  readActiveToken,
  readDevices,
  removeDevice,
  setActiveToken,
  setDeviceAlias,
  updateDevice,
  type MobileDevice,
} from "../../lib/mobile-devices";

export function useRemoteDevices() {
  const [devices, setDevices] = useState<MobileDevice[]>(() => reconcileDevices());
  const [activeToken, setActiveTokenState] = useState(() => readActiveToken());
  const [deviceEpoch, setDeviceEpoch] = useState(0);

  const refresh = useCallback(() => {
    const next = readDevices();
    const token = readActiveToken();
    setDevices(next);
    setActiveTokenState(token);
    return { devices: next, activeToken: token };
  }, []);

  const activate = useCallback((token: string) => {
    // cloud() 以存储为认证权威；必须先写它，再发布 React 状态。
    setActiveToken(token);
    const next = readDevices();
    setDevices(next);
    setActiveTokenState(token);
    setDeviceEpoch((value) => value + 1);
  }, []);

  const add = useCallback((input: { token: string; instanceId: string; name?: string }) => {
    const next = addOrReplaceDevice(input);
    setDevices(next);
    setActiveTokenState(input.token);
    setDeviceEpoch((value) => value + 1);
  }, []);

  const remove = useCallback((token: string) => {
    removeDevice(token);
    const next = readDevices();
    const nextToken = readActiveToken();
    setDevices(next);
    setActiveTokenState(nextToken);
    setDeviceEpoch((value) => value + 1);
    return nextToken;
  }, []);

  const patch = useCallback((token: string, value: Partial<Omit<MobileDevice, "token">>) => {
    const next = updateDevice(token, value);
    setDevices(next);
    return next;
  }, []);

  const rename = useCallback((token: string, alias: string) => {
    const next = setDeviceAlias(token, alias);
    setDevices(next);
    return next;
  }, []);

  return {
    devices,
    activeToken,
    activeDevice: activeDevice(),
    deviceEpoch,
    refresh,
    activate,
    add,
    remove,
    patch,
    rename,
  };
}
