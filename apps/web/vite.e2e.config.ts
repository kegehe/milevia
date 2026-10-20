import { defineConfig, mergeConfig } from "vite";
import base from "./vite.config";
export default mergeConfig(base, defineConfig({ cacheDir: "node_modules/.vite-e2e" }));
