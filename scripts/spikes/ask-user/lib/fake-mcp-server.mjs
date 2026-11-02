// 假的 Streamable-HTTP MCP server：只实现 spike 需要的最小面。
//
// 存在的意义：在真正的 Milevia 交互 MCP 写出来之前，先回答两个问题——
//   1. 每个 CLI 能不能连上 http 形态的 MCP（而不是 stdio）；
//   2. 一次工具调用能挂多久不被 CLI 自己的超时掐断。
//
// 只实现 POST /mcp 上的 JSON-RPC（initialize / tools/list / tools/call）。
// 不实现 SSE：我们的真实 server 也没有服务端主动消息，不需要它。
import { createServer } from "node:http";
import { appendFileSync } from "node:fs";

const logFile = process.env.SPIKE_MCP_LOG || "";
const requireAuth = process.argv.includes("--require-auth");

function log(entry) {
  if (!logFile) return;
  try { appendFileSync(logFile, JSON.stringify(entry) + "\n"); } catch {}
}

const tools = [
  {
    name: "echo_marker",
    description: "Return a fixed marker immediately. Use it to verify connectivity alone.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
  },
  {
    name: "sleep",
    description: "Sleep for the requested number of seconds, then return SPIKE_MCP_OK_<seconds>.",
    inputSchema: {
      type: "object",
      properties: { seconds: { type: "number", description: "How long to sleep." } },
      required: ["seconds"],
    },
  },
];

const server = createServer((req, res) => {
  if (req.method === "GET") {
    res.writeHead(405, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: "GET not supported" }));
    return;
  }
  if (req.method === "DELETE") {
    res.writeHead(200);
    res.end();
    return;
  }
  if (req.method !== "POST") {
    res.writeHead(405);
    res.end();
    return;
  }

  const auth = req.headers["authorization"] || "";
  log({ kind: "request", path: req.url, authPresent: Boolean(auth), at: Date.now() });
  if (requireAuth && !auth) {
    res.writeHead(401, { "content-type": "application/json" });
    res.end(JSON.stringify({ jsonrpc: "2.0", error: { code: -32001, message: "missing bearer token" }, id: null }));
    return;
  }

  const chunks = [];
  req.on("data", (chunk) => chunks.push(chunk));
  req.on("end", () => {
    let body = null;
    try { body = JSON.parse(Buffer.concat(chunks).toString("utf8")); } catch {}
    if (!body) {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(JSON.stringify({ jsonrpc: "2.0", error: { code: -32700, message: "parse error" }, id: null }));
      return;
    }

    // 通知没有 id，按规范回 202 空体。
    if (body.id === undefined) {
      log({ kind: "notification", method: body.method, at: Date.now() });
      res.writeHead(202);
      res.end();
      return;
    }

    const reply = (result) => {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ jsonrpc: "2.0", id: body.id, result }));
    };

    switch (body.method) {
      case "initialize": {
        const requested = body.params && body.params.protocolVersion;
        log({ kind: "initialize", requested, at: Date.now() });
        reply({
          protocolVersion: requested || "2025-06-18",
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: "spike", version: "0.0.1" },
        });
        return;
      }
      case "tools/list":
        log({ kind: "tools/list", at: Date.now() });
        reply({ tools });
        return;
      case "tools/call": {
        const name = body.params && body.params.name;
        const args = (body.params && body.params.arguments) || {};
        if (name === "echo_marker") {
          log({ kind: "tools/call", name, delayMs: 0, at: Date.now() });
          reply({ content: [{ type: "text", text: "SPIKE_MCP_OK" }] });
          return;
        }
        if (name === "sleep") {
          const seconds = Number(args.seconds);
          const safe = Number.isFinite(seconds) && seconds >= 0 ? Math.min(seconds, 600) : 5;
          log({ kind: "tools/call", name, delayMs: safe * 1000, at: Date.now() });
          setTimeout(() => {
            reply({ content: [{ type: "text", text: "SPIKE_MCP_OK_" + safe }] });
          }, safe * 1000);
          return;
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ jsonrpc: "2.0", id: body.id, error: { code: -32602, message: "unknown tool " + name } }));
        return;
      }
      default:
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ jsonrpc: "2.0", id: body.id, error: { code: -32601, message: "unknown method " + body.method } }));
    }
  });
});

server.listen(0, "127.0.0.1", () => {
  const port = server.address().port;
  process.stdout.write("READY " + port + "\n");
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => { server.close(() => process.exit(0)); });
}
