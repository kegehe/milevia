import { Capacitor } from "@capacitor/core";

const configuredCloudURL = (import.meta.env.VITE_CLOUD_URL as string | undefined)?.replace(/\/$/, "") || "";

export const cloudURL = Capacitor.isNativePlatform()
  ? configuredCloudURL || "https://keyanjia.info:8443"
  : import.meta.env.DEV ? "" : configuredCloudURL;
