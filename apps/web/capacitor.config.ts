import type { CapacitorConfig } from "@capacitor/cli";

const config: CapacitorConfig = {
  appId: "info.keyanjia.milevia",
  appName: "Milevia",
  webDir: "dist",
  bundledWebRuntime: false,
  server: {
    androidScheme: "https",
  },
};

export default config;
