import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";

const controlURL = process.env.VITE_CONTROL_URL || "http://127.0.0.1:8080";
const websocketURL = controlURL.replace(/^http/, "ws");
const webRoot = fileURLToPath(new URL(".", import.meta.url));

export default defineConfig({
  root: webRoot,
  server: { proxy: { "/api": controlURL, "/ws": { target: websocketURL, ws: true } } },
});
