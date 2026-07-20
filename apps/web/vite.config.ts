import { defineConfig } from "vite";

const controlURL = process.env.VITE_CONTROL_URL || "http://127.0.0.1:8080";
const websocketURL = controlURL.replace(/^http/, "ws");

export default defineConfig({ server: { proxy: { "/api": controlURL, "/ws": { target: websocketURL, ws: true } } } });
