import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import QRCode from "qrcode";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "../lib/api";
import { isDesktop } from "../lib/runtime";
import { useNavigate } from "react-router-dom";
import { Capacitor } from "@capacitor/core";
import { App as CapacitorApp } from "@capacitor/app";
import type { PluginListenerHandle } from "@capacitor/core";
import { BarcodeFormat, BarcodeScanner, LensFacing } from "@capacitor-mlkit/barcode-scanning";
import "./mobile-remote.css";

type Instance = { instanceId: string; name: string; status: string; lastAgentSequence: number; lastSeenAt?: string };
type Task = { id: string; title: string; description?: string; priority: string; status: string; updatedAt: string };
type Message = { id: string; runId?: string; role: "user" | "assistant"; content: string; createdAt: string };
type RemoteConversation = { id: string; title: string; status: string; agentId: string; lastActivityAt: string; isCurrent: boolean; messages: Message[] };
type ProjectEnvironment = "windows" | "wsl" | "remote-linux";
type Project = { id: string; name: string; runner: string; environment?: ProjectEnvironment; running?: boolean; gitBranch: string; tasks: Task[]; conversations: RemoteConversation[] };
type Snapshot = { snapshotRevision: number; observedAt: string; projects: Project[] };
type CommandState = { commandId: string; status: string; result?: unknown };
type AcceptedCommand = { commandId: string; status: string };
type PendingMessage = { requestId: string; content: string; createdAt: string };
type ProcessingConversation = { count: number; startedAt: number; snapshotRevision: number };
type ProcessingConversations = Record<string, ProcessingConversation>;

const terminalCommandStatuses = ["completed", "failed", "expired", "cancelled", "indeterminate"];

function commandStatusRank(status: string): number {
  if (status === "queued" || status === "pending") return 0;
  if (status === "received" || status === "executing") return 1;
  return terminalCommandStatuses.includes(status) ? 2 : 1;
}

function mergeCommandState(current: CommandState | null, next: CommandState): CommandState {
  if (!current || current.commandId !== next.commandId) return next;
  // Poll responses can arrive out of order. A terminal state must never be
  // replaced by an older queued/executing response.
  if (commandStatusRank(next.status) < commandStatusRank(current.status)) return current;
  if (terminalCommandStatuses.includes(current.status) && !terminalCommandStatuses.includes(next.status)) return current;
  return next;
}

function taskStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    todo: "待处理",
    queued: "排队中",
    action_required: "需要操作",
    running: "执行中",
    awaiting_review: "待验收",
    completed: "已完成",
    done: "已完成",
    failed: "失败",
    cancelled: "已取消",
    blocked: "已阻塞",
  };
  return labels[status] || status || "未知状态";
}

function taskStatusClass(status: string): string {
  return `status-${status.replace(/[^a-z0-9_-]/gi, "-") || "unknown"}`;
}

const taskFilters = [
  { id: "all", label: "全部" },
  { id: "todo", label: "待处理" },
  { id: "running", label: "执行中" },
  { id: "awaiting_review", label: "待验收" },
  { id: "action_required", label: "需处理" },
  { id: "done", label: "已完成" },
  { id: "cancelled", label: "已取消" },
] as const;

type TaskFilter = typeof taskFilters[number]["id"];

function taskSummary(task: Task): string {
  const title = task.title.trim();
  if (title) return title;
  const description = task.description?.trim() || "";
  return description || "暂无任务内容";
}

function conversationStatusLabel(status: string): string {
  const labels: Record<string, string> = { idle: "就绪", queued: "排队中", running: "执行中", completed: "已完成", failed: "失败", stopped: "已停止" };
  return labels[status] || status || "未知状态";
}

function conversationAgentLabel(agentId: string): string {
  return agentId === "codex" ? "Codex" : "Claude Code";
}

function projectEnvironment(project: Project): ProjectEnvironment {
  if (project.environment === "windows" || project.environment === "wsl" || project.environment === "remote-linux") return project.environment;
  return project.runner.startsWith("ssh-") ? "remote-linux" : "wsl";
}

function projectEnvironmentLabel(environment: ProjectEnvironment): string {
  return environment === "windows" ? "Windows" : environment === "wsl" ? "WSL" : "SSH";
}

function ProjectEnvironmentIcon({ environment }: { environment: ProjectEnvironment }) {
  if (environment === "windows") return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 5.5 10.5 4v7H3v-5.5ZM13 3.5 21 2v9h-8v-7.5ZM3 13h7.5v7L3 18.5V13ZM13 13h8v9l-8-1.5V13Z" /></svg>;
  if (environment === "wsl") return <svg viewBox="0 0 24 24" aria-hidden="true"><g fill="currentColor"><path fillRule="evenodd" d="M9 7.8h6c3.2 1.2 4.2 3.4 4 6.2-.2 2.8-1 5.2-3.4 6.6-1.2.8-6 .8-7.2 0C6 19.2 5.2 16.8 5 14c-.2-2.8.8-5 4-6.2Zm-.4 7.8a3.4 3.8 0 1 0 6.8 0 3.4 3.8 0 1 0-6.8 0ZM12 1.2a4.2 4.2 0 1 0 0 8.4 4.2 4.2 0 1 0 0-8.4Zm-1.8 3a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm3.6 0a1 1 0 1 0 0 2 1 1 0 1 0 0-2Zm-2.9 2.7h2.2L12 8.8Z" /><path d="M16.5 10.8c2.3 1 3.7 3.2 3.7 5.4 0 2.2-1.6 3.4-4 3.4-1 0-1.6-.6-1.8-1.2.4-2.2.8-5.4 2.1-7.6ZM7.5 10.8c-2.3 1-3.7 3.2-3.7 5.4 0 2.2 1.6 3.4 4 3.4 1 0 1.6-.6 1.8-1.2-.4-2.2-.8-5.4-2.1-7.6Z" /></g></svg>;
  return <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="6" rx="1.2" /><rect x="4" y="14" width="16" height="6" rx="1.2" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></svg>;
}

function conversationIDFromCommandResult(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const value = result as { id?: unknown; conversation?: { id?: unknown } };
  if (typeof value.id === "string") return value.id;
  return typeof value.conversation?.id === "string" ? value.conversation.id : "";
}

function commandFailureDetail(result: unknown): string {
  if (!result || typeof result !== "object") return "";
  const error = (result as { error?: unknown }).error;
  return typeof error === "string" ? error.trim() : "";
}

function normalizeCommandState(value: unknown, fallbackCommandID = ""): CommandState {
  if (!value || typeof value !== "object") return { commandId: fallbackCommandID, status: "indeterminate" };
  const raw = value as { commandId?: unknown; status?: unknown; result?: unknown; command?: { commandId?: unknown } };
  return {
    commandId: typeof raw.commandId === "string" ? raw.commandId
      : typeof raw.command?.commandId === "string" ? raw.command.commandId
        : fallbackCommandID,
    status: typeof raw.status === "string" ? raw.status : "indeterminate",
    result: raw.result,
  };
}

function conversationFromCommandResult(result: unknown): RemoteConversation | null {
  if (!result || typeof result !== "object") return null;
  const raw = (result as { conversation?: unknown }).conversation;
  const value = raw && typeof raw === "object" ? raw : result;
  const item = value as Partial<RemoteConversation> & { id?: unknown };
  if (typeof item.id !== "string" || item.id.trim() === "") return null;
  return {
    id: item.id,
    title: typeof item.title === "string" ? item.title : "新会话",
    status: typeof item.status === "string" ? item.status : "idle",
    agentId: typeof item.agentId === "string" ? item.agentId : "",
    lastActivityAt: typeof item.lastActivityAt === "string" ? item.lastActivityAt : new Date().toISOString(),
    isCurrent: item.isCurrent !== false,
    messages: Array.isArray(item.messages) ? item.messages as Message[] : [],
  };
}

// Native builds do not have a proxy origin. Keep a production fallback so an
// APK built without a local .env can still reach the single public endpoint.
const configuredCloudURL = (import.meta.env.VITE_CLOUD_URL as string | undefined)?.replace(/\/$/, "") || "";
// In Vite development use the local /v1 proxy to avoid browser CORS. Native
// and production builds keep an absolute public URL.
const cloudURL = Capacitor.isNativePlatform()
  ? configuredCloudURL || "https://keyanjia.info:8443"
  : import.meta.env.DEV ? "" : configuredCloudURL;
const cloudRequestTimeoutMs = 15_000;

type BarcodeDetectorResult = { rawValue?: string };
type BarcodeDetectorLike = new (options?: { formats?: string[] }) => { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };

function pairingFromScan(value: string): { pairingID: string; code: string } {
  try {
    const parsed = new URL(value);
    return {
      pairingID: parsed.searchParams.get("pairingId") || parsed.searchParams.get("pairing_id") || "",
      code: parsed.searchParams.get("code") || "",
    };
  } catch {
    return { pairingID: "", code: "" };
  }
}

function pairingURLWithCode(value: string | undefined, pairingID: string): string {
  try {
    const fallbackOrigin = globalThis.location?.origin || "https://keyanjia.info:8443";
    const parsed = new URL(value || `${fallbackOrigin}/mobile`);
    parsed.searchParams.set("pairingId", pairingID);
    // Keep the short-lived pairing code out of URLs, where it can leak through
    // browser history, proxy logs, or referrer data.
    parsed.searchParams.delete("code");
    return parsed.toString();
  } catch {
    return `${globalThis.location?.origin || "https://keyanjia.info:8443"}/mobile?pairingId=${encodeURIComponent(pairingID)}`;
  }
}

function idempotencyKey() {
  return globalThis.crypto?.randomUUID?.() || `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

async function cloud<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("Content-Type", "application/json");
  const token = localStorage.getItem("milevia.cloud.token");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const controller = new AbortController();
  const timeout = globalThis.setTimeout(() => controller.abort(), cloudRequestTimeoutMs);
  const abort = () => controller.abort();
  if (init?.signal?.aborted) controller.abort();
  else init?.signal?.addEventListener("abort", abort, { once: true });
  let response: Response;
  try {
    response = await fetch(`${cloudURL}${path}`, { ...init, headers, signal: controller.signal });
    const body = await response.json().catch(() => null);
    if (!response.ok) {
      const message = response.status === 401 ? "云端令牌无效或已过期，请重新输入"
        : response.status === 403 ? "当前令牌没有访问权限"
        : response.status === 404 ? "配对会话不存在或配对码错误"
        : response.status === 410 ? "配对码已过期或已使用"
        : response.status === 409 ? "配对码已被使用或存在冲突，请让电脑重新生成验证码"
        : body && typeof body.error === "string" ? body.error : `请求失败 (${response.status})`;
      throw new Error(message);
    }
    if (body === null || body === undefined) throw new Error("云端返回格式无效，请稍后重试");
    return body as T;
  } catch (cause) {
    if (controller.signal.aborted && !init?.signal?.aborted) {
      throw new Error("云端请求超时，请检查手机网络后重试");
    }
    throw cause;
  } finally {
    globalThis.clearTimeout(timeout);
    init?.signal?.removeEventListener("abort", abort);
  }
}

async function consumeMobileEventStream(url: string, token: string, lastEventID: string, onMessage: (event: MessageEvent) => void, signal: AbortSignal) {
  const headers = new Headers({ Accept: "text/event-stream", Authorization: `Bearer ${token}` });
  if (lastEventID) headers.set("Last-Event-ID", lastEventID);
  const response = await fetch(url, { headers, signal });
  if (!response.ok) throw new Error(`stream request failed (${response.status})`);
  if (!response.body) throw new Error("stream response has no body");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventID = "";
  let data: string[] = [];
  const dispatch = () => {
    if (data.length > 0) onMessage(new MessageEvent("message", { data: data.join("\n"), lastEventId: eventID }));
    data = [];
    eventID = "";
  };
  const processLine = (line: string) => {
    if (line === "") { dispatch(); return; }
    if (line.startsWith(":")) return;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
    if (field === "id") eventID = value;
    if (field === "data") data.push(value);
  };
  while (!signal.aborted) {
    const next = await reader.read();
    if (next.done) {
      buffer += decoder.decode();
      if (buffer) processLine(buffer);
      break;
    }
    buffer += decoder.decode(next.value, { stream: true });
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() || "";
    for (const line of lines) processLine(line);
  }
  dispatch();
}

export default function MobileRemotePage() {
  const navigate = useNavigate();
  // Capacitor 原生 WebView 可能同时注入桌面运行时对象；原生包始终
  // 使用移动端项目选择/对话布局，避免被桌面分支误判。
  const mobileApp = Capacitor.isNativePlatform() || !isDesktop();
  const [token, setToken] = useState(() => localStorage.getItem("milevia.cloud.token") || "");
  const [instances, setInstances] = useState<Instance[]>([]);
  const [instanceID, setInstanceID] = useState("");
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [selectedProject, setSelectedProject] = useState("");
  const [selectedConversation, setSelectedConversation] = useState("");
  const [conversationMenuOpen, setConversationMenuOpen] = useState(false);
  const [newConversationProject, setNewConversationProject] = useState<Project | null>(null);
  const [newConversationAgent, setNewConversationAgent] = useState<"claude-code" | "codex">("claude-code");
  const [mobileView, setMobileView] = useState<"projects" | "conversation">("projects");
  const [tasksOpen, setTasksOpen] = useState(false);
  const [taskFilter, setTaskFilter] = useState<TaskFilter>("all");
  const [messageDraft, setMessageDraft] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [editingTask, setEditingTask] = useState<Task | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editPriority, setEditPriority] = useState("normal");
  const [deletingTask, setDeletingTask] = useState<Task | null>(null);
  const [busy, setBusy] = useState(false);
  const [processingConversations, setProcessingConversations] = useState<ProcessingConversations>({});
  const [error, setError] = useState("");
  const [notificationPermission, setNotificationPermission] = useState<NotificationPermission | "unsupported">(() => typeof Notification === "undefined" ? "unsupported" : Notification.permission);
  const [pairingCode, setPairingCode] = useState(() => new URLSearchParams(location.search).get("code") || "");
  const [manualPairingCode, setManualPairingCode] = useState("");
  const [pairingID, setPairingID] = useState(() => new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "");
  const [pairingStatus, setPairingStatus] = useState("");
  const [pairingURL, setPairingURL] = useState("");
  const [pairingQR, setPairingQR] = useState("");
  const [pairingExpanded, setPairingExpanded] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [scanError, setScanError] = useState("");
  const [scanVideo, setScanVideo] = useState<HTMLVideoElement | null>(null);
  const nativeScanListener = useRef<PluginListenerHandle | null>(null);
  const conversationMenuRef = useRef<HTMLDivElement | null>(null);
  const messageInputRef = useRef<HTMLTextAreaElement | null>(null);
  const selectedConversationRef = useRef("");
  const pendingMessageRef = useRef(new Map<string, Map<string, PendingMessage>>());
  const realtimeMessagesRef = useRef(new Map<string, { conversationId: string; message: Message }>());
  const creatingProjectRef = useRef("");
  const instancesRequestGenerationRef = useRef(0);
  const snapshotRevisionRef = useRef(-1);
  const snapshotLoadGenerationRef = useRef(0);
  const scanAccepted = useRef(false);
  const initialPairing = useRef({
    pairingID: new URLSearchParams(location.search).get("pairingId") || new URLSearchParams(location.search).get("pairing_id") || "",
    code: new URLSearchParams(location.search).get("code") || "",
    attempted: false,
  });
  const notifiedEventIDs = useRef(new Set<string>());
  const [commandState, setCommandState] = useState<CommandState | null>(null);
  const [pendingAccessToken, setPendingAccessToken] = useState("");

  const markConversationProcessing = useCallback((conversationID: string, snapshotRevision: number) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      return {
        ...current,
        [conversationID]: {
          count: (existing?.count || 0) + 1,
          startedAt: existing?.startedAt || Date.now(),
          snapshotRevision: existing?.snapshotRevision ?? snapshotRevision,
        },
      };
    });
  }, []);

  const clearConversationProcessing = useCallback((conversationID: string, all = false) => {
    setProcessingConversations((current) => {
      const existing = current[conversationID];
      if (!existing) return current;
      if (!all && existing.count > 1) return { ...current, [conversationID]: { ...existing, count: existing.count - 1 } };
      const next = { ...current };
      delete next[conversationID];
      return next;
    });
  }, []);

  const acceptScannedPairing = useCallback((value: string) => {
    const scanned = pairingFromScan(value);
    if (!scanned.pairingID) {
      setScanError("这不是 Milevia 配对二维码，请对准电脑端刚生成的二维码");
      return false;
    }
    if (scanAccepted.current) return true;
    scanAccepted.current = true;
    setPairingID(scanned.pairingID);
    setPairingCode(scanned.code || "");
    setScanError("");
    setScanning(false);
    if (/^\d{6}$/.test(scanned.code)) {
      void claimPairing(scanned.pairingID, scanned.code);
    } else {
      setPairingStatus("已识别配对会话，请输入电脑端显示的 6 位校验码");
    }
    return true;
  }, []);

  useEffect(() => {
    if (!scanning || !Capacitor.isNativePlatform()) return;
    let cancelled = false;
    let started = false;
    let timeout: number | undefined;
    scanAccepted.current = false;

    const stop = async () => {
      if (timeout !== undefined) window.clearTimeout(timeout);
      document.body.classList.remove("barcode-scanner-active");
      const listener = nativeScanListener.current;
      nativeScanListener.current = null;
      await listener?.remove().catch(() => undefined);
      if (started) await BarcodeScanner.stopScan().catch(() => undefined);
    };

    void (async () => {
      try {
        const { supported } = await BarcodeScanner.isSupported();
        if (!supported) throw new Error("此设备没有可用摄像头");
        let permission = await BarcodeScanner.checkPermissions();
        if (permission.camera === "prompt" || permission.camera === "prompt-with-rationale") {
          permission = await BarcodeScanner.requestPermissions();
        }
        if (permission.camera !== "granted" && permission.camera !== "limited") {
          throw new Error("未获得摄像头权限，请在系统设置中允许 Milevia 使用摄像头");
        }
        if (cancelled) return;

        document.body.classList.add("barcode-scanner-active");
        nativeScanListener.current = await BarcodeScanner.addListener("barcodesScanned", ({ barcodes }) => {
          for (const barcode of barcodes) {
            if (acceptScannedPairing(barcode.rawValue || barcode.displayValue)) return;
          }
        });
        if (cancelled) return;
        await BarcodeScanner.startScan({ formats: [BarcodeFormat.QrCode], lensFacing: LensFacing.Back });
        started = true;
        if (cancelled) {
          await BarcodeScanner.stopScan().catch(() => undefined);
          return;
        }
        timeout = window.setTimeout(() => {
          if (!cancelled) {
            setScanError("30 秒内未识别到二维码，请调整距离、亮度后重试");
            setScanning(false);
          }
        }, 30_000);
      } catch (cause) {
        if (!cancelled) {
          setScanError(cause instanceof Error ? cause.message : "无法启动二维码扫描，请稍后重试");
          setScanning(false);
        }
      }
    })();

    return () => {
      cancelled = true;
      void stop();
    };
  }, [scanning, acceptScannedPairing]);

  useEffect(() => {
    if (!scanning || Capacitor.isNativePlatform() || !scanVideo) return;
    let cancelled = false;
    let stream: MediaStream | undefined;
    const timeout = window.setTimeout(() => {
      if (!cancelled) {
        setScanError("30 秒内未识别到二维码，请调整距离、亮度后重试");
        setScanning(false);
      }
    }, 30_000);
    const detectorCtor = (globalThis as typeof globalThis & { BarcodeDetector?: BarcodeDetectorLike }).BarcodeDetector;
    if (!detectorCtor) {
      window.clearTimeout(timeout);
      setScanError("当前浏览器不支持二维码识别，请使用 Milevia Android 应用扫码");
      setScanning(false);
      return;
    }
    const mediaDevices = navigator.mediaDevices;
    if (!mediaDevices?.getUserMedia) {
      window.clearTimeout(timeout);
      setScanError("当前设备无法访问摄像头，请检查浏览器摄像头权限");
      setScanning(false);
      return;
    }
    void mediaDevices.getUserMedia({ video: { facingMode: { ideal: "environment" } }, audio: false })
      .then(async (nextStream) => {
        if (cancelled) { nextStream.getTracks().forEach((track) => track.stop()); return; }
        stream = nextStream;
        scanVideo.srcObject = nextStream;
        await scanVideo.play();
        let detector: { detect(source: HTMLVideoElement): Promise<BarcodeDetectorResult[]> };
        try {
          detector = new detectorCtor({ formats: ["qr_code"] });
        } catch {
          throw new Error("当前 Android WebView 不支持二维码识别，请手动输入配对会话 ID");
        }
        while (!cancelled) {
          const results = await detector.detect(scanVideo).catch(() => []);
          if (results.some((item) => acceptScannedPairing(item.rawValue || ""))) break;
          if (results.some((item) => item.rawValue)) {
            setScanError("扫描到的二维码不是 Milevia 配对二维码");
          }
          await new Promise((resolve) => window.setTimeout(resolve, 250));
        }
      })
      .catch((cause: unknown) => {
        if (!cancelled) {
          setScanError(cause instanceof Error && cause.message.includes("不支持") ? cause.message : "无法打开摄像头，请允许浏览器使用摄像头");
          setScanning(false);
        }
      });
    return () => {
      cancelled = true;
      window.clearTimeout(timeout);
      stream?.getTracks().forEach((track) => track.stop());
      if (scanVideo) scanVideo.srcObject = null;
    };
  }, [scanning, scanVideo, acceptScannedPairing]);

  const loadInstances = useCallback(async () => {
    const requestGeneration = ++instancesRequestGenerationRef.current;
    if (!localStorage.getItem("milevia.cloud.token")) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return;
      setInstances([]);
      setInstanceID("");
      return;
    }
    setError("");
    try {
      const result = await cloud<Instance[]>("/v1/instances");
      if (requestGeneration !== instancesRequestGenerationRef.current) return;
      const nextInstances = Array.isArray(result) ? result : [];
      setInstances(nextInstances);
      setInstanceID((current) => nextInstances.some((item) => item.instanceId === current)
        ? current
        : nextInstances[0]?.instanceId || "");
      if (!Array.isArray(result)) {
        setError("云端返回了无效的电脑实例列表");
      }
    } catch (cause) {
      if (requestGeneration !== instancesRequestGenerationRef.current) return;
      setError(cause instanceof TypeError ? "无法连接云端，请确认 keyanjia.info:8443 已放行并可访问" : cause instanceof Error ? cause.message : "无法加载电脑实例");
    }
  }, []);

  useEffect(() => {
    snapshotRevisionRef.current = -1;
    // These maps are scoped to the selected desktop instance. Retaining them
    // across a logout or instance switch can make a coincidentally reused
    // conversation ID appear to be processing in the new instance.
    pendingMessageRef.current.clear();
    realtimeMessagesRef.current.clear();
    setProcessingConversations({});
  }, [instanceID]);
  const loadSnapshot = useCallback(async (): Promise<Snapshot | null> => {
    if (!instanceID) return null;
    const requestGeneration = ++snapshotLoadGenerationRef.current;
    setError("");
    try {
      const value = await cloud<Snapshot>(`/v1/instances/${encodeURIComponent(instanceID)}/snapshot`);
      if (!value || !Array.isArray(value.projects)) {
        throw new Error("云端返回了无效的项目快照");
      }
      // Snapshots from older mobile builds may not carry a revision. Treat
      // those as the initial revision instead of dropping a valid project
      // list because `undefined >= -1` is false.
      value.snapshotRevision = typeof value.snapshotRevision === "number" && Number.isFinite(value.snapshotRevision)
        ? value.snapshotRevision
        : 0;
      if (requestGeneration === snapshotLoadGenerationRef.current && value.snapshotRevision >= snapshotRevisionRef.current) {
        snapshotRevisionRef.current = value.snapshotRevision;
        const pending = pendingMessageRef.current;
        if (pending.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            const messages = pending.get(entry.id);
            if (!messages || messages.size === 0) return entry;
            // Consume persisted messages as a multiset so two identical
            // prompts do not accidentally clear both optimistic entries.
            const persistedCounts = new Map<string, number>();
            for (const message of entry.messages) {
              if (message.role === "user") persistedCounts.set(message.content, (persistedCounts.get(message.content) || 0) + 1);
            }
            const additions: Message[] = [];
            const matchedRequestIDs = new Set<string>();
            for (const pendingMessage of messages.values()) {
              const count = persistedCounts.get(pendingMessage.content) || 0;
              if (count > 0) {
                persistedCounts.set(pendingMessage.content, count - 1);
                matchedRequestIDs.add(pendingMessage.requestId);
              }
              else additions.push({ id: `pending-${pendingMessage.requestId}`, role: "user", content: pendingMessage.content, createdAt: pendingMessage.createdAt });
            }
            for (const requestID of matchedRequestIDs) messages.delete(requestID);
            if (messages.size === 0) pending.delete(entry.id);
            return additions.length > 0 ? { ...entry, messages: [...entry.messages, ...additions] } : entry;
          }) }));
        }
        // A message event can arrive before the Agent has uploaded its next
        // snapshot. Preserve those messages while accepting the older snapshot
        // so streamed output cannot briefly disappear from the mobile view.
        const realtimeMessages = realtimeMessagesRef.current;
        if (realtimeMessages.size > 0) {
          value.projects = value.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            let changed = false;
            const messages = entry.messages.map((message) => {
              const live = realtimeMessages.get(message.id);
              if (!live || live.conversationId !== entry.id) return message;
              realtimeMessages.delete(message.id);
              // The recovery snapshot intentionally bounds older content. If
              // the realtime event has the full message, prefer it so a fast
              // snapshot cannot overwrite a complete assistant response with
              // its 2000-character prefix.
              if (live.message.content.length > message.content.length) {
                changed = true;
                return live.message;
              }
              return message;
            });
            const extras = [...realtimeMessages.values()]
              .filter((item) => item.conversationId === entry.id && !entry.messages.some((message) => message.id === item.message.id))
              .map((item) => {
                realtimeMessages.delete(item.message.id);
                return item.message;
              });
            return changed || extras.length > 0 ? { ...entry, messages: [...messages, ...extras] } : entry;
          }) }));
        }
        setSnapshot(value);
        localStorage.setItem(`milevia.snapshot.${instanceID}`, JSON.stringify(value));
      }
      return value;
    } catch (cause) {
      if (requestGeneration !== snapshotLoadGenerationRef.current) return null;
      const cached = localStorage.getItem(`milevia.snapshot.${instanceID}`);
      if (cached) { try { const cachedValue = JSON.parse(cached) as Snapshot; setSnapshot(cachedValue); setError("当前显示的是最近一次同步快照"); return cachedValue; } catch { /* ignore invalid cache */ } }
      setError(cause instanceof Error ? cause.message : "无法加载项目快照");
      return null;
    }
  }, [instanceID]);

  const applyRealtimeEvent = useCallback((raw: MessageEvent): boolean => {
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; payload?: unknown; createdAt?: string };
      if (!event || !event.type || !event.payload) return false;
      const payload = event.payload as Partial<Message> & { conversationId?: string; projectId?: string; status?: string };
      if (event.type === "conversation.created" && typeof payload.conversationId === "string" && typeof payload.projectId === "string") {
        setSnapshot((current) => {
          if (!current) return current;
          return {
            ...current,
            projects: current.projects.map((item) => item.id !== payload.projectId || item.conversations.some((entry) => entry.id === payload.conversationId)
              ? item
              : {
                ...item,
                conversations: [{
                  id: payload.conversationId!,
                  title: "新会话",
                  status: "idle",
                  agentId: "",
                  lastActivityAt: event.createdAt || new Date().toISOString(),
                  isCurrent: true,
                  messages: [],
                }, ...item.conversations.map((entry) => ({ ...entry, isCurrent: false }))],
              }),
          };
        });
        if (creatingProjectRef.current === payload.projectId) {
          creatingProjectRef.current = "";
          setSelectedProject(payload.projectId);
          setSelectedConversation(payload.conversationId);
          setTasksOpen(false);
          setPairingExpanded(false);
          setMobileView("conversation");
          setBusy(false);
        }
        return true;
      }
      if ((event.type === "user.message" || event.type === "assistant.message") && typeof payload.id === "string" && typeof payload.conversationId === "string" && typeof payload.content === "string") {
        const role: Message["role"] = event.type === "assistant.message" ? "assistant" : "user";
        const realtimeMessage: Message = { id: payload.id, runId: payload.runId, role, content: payload.content, createdAt: payload.createdAt || event.createdAt || new Date().toISOString() };
        realtimeMessagesRef.current.set(realtimeMessage.id, { conversationId: payload.conversationId, message: realtimeMessage });
        if (realtimeMessagesRef.current.size > 500) {
          const oldest = realtimeMessagesRef.current.keys().next().value;
          if (oldest) realtimeMessagesRef.current.delete(oldest);
        }
        if (role === "user") {
          const pending = pendingMessageRef.current.get(payload.conversationId);
          if (pending) {
            const matching = [...pending.values()].find((item) => item.content === payload.content);
            if (matching) pending.delete(matching.requestId);
            if (pending.size === 0) pendingMessageRef.current.delete(payload.conversationId);
          }
        }
        setSnapshot((current) => {
          if (!current) return current;
          return { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => {
            if (entry.id !== payload.conversationId) return entry;
            const exists = entry.messages.some((message) => message.id === payload.id);
            const optimisticIndex = role === "user" ? entry.messages.findIndex((message) => message.id.startsWith("pending-") && message.role === "user" && message.content === payload.content) : -1;
            if (exists) return entry;
            const messages = optimisticIndex >= 0
              ? entry.messages.map((message, index) => index === optimisticIndex ? realtimeMessage : message)
              : [...entry.messages, realtimeMessage];
            return { ...entry, messages };
          }) })) };
        });
        return true;
      }
    } catch { /* malformed realtime payloads are recovered by the snapshot */ }
    return false;
  }, []);

  const notifyHiddenMobileEvent = useCallback((raw: MessageEvent) => {
    if (!document.hidden || typeof Notification === "undefined" || Notification.permission !== "granted") return;
    try {
      const event = JSON.parse(String(raw.data)) as { eventId?: string; type?: string; payload?: unknown };
      if (!event.eventId || notifiedEventIDs.current.has(event.eventId)) return;
      // Conversation messages are already visible when the user returns. Task
      // and run state changes are the events that need an interruption notice.
      if (!event.type || !/^(task\.|run\.|approval\.)/.test(event.type)) return;
      notifiedEventIDs.current.add(event.eventId);
      const payload = event.payload && typeof event.payload === "object" ? event.payload as { summary?: unknown; status?: unknown } : {};
      const detail = typeof payload.summary === "string" ? payload.summary : typeof payload.status === "string" ? `状态：${payload.status}` : event.type;
      new Notification("Milevia", { body: detail, tag: event.eventId });
    } catch {
      // Malformed events are recovered by the normal snapshot refresh.
    }
  }, []);

  useEffect(() => {
    if (!token.trim()) return;
    void loadInstances();
    const timer = window.setInterval(() => { void loadInstances(); }, 5000);
    return () => window.clearInterval(timer);
  }, [token, loadInstances]);
  useEffect(() => { void loadSnapshot(); }, [loadSnapshot]);
  useEffect(() => {
    if (!instanceID) return;
    const tokenValue = localStorage.getItem("milevia.cloud.token") || "";
    const controller = new AbortController();
    let stopped = false;
    let lastEventID = "";
    // SSE is the fast path; periodic snapshots remain active because
    // conversation output is produced independently of task events.
    // SSE is the normal fast path. Keep a slower safety poll for proxies or
    // networks that silently drop events, without competing with every event
    // refresh and increasing full-snapshot traffic threefold.
    let fallbackTimer: number | null = window.setInterval(() => { void loadSnapshot(); }, 15_000);
    let refreshTimer: number | null = null;
    const scheduleSnapshotRefresh = (delay: number) => {
      // Trailing throttle: a busy assistant stream cannot keep postponing
      // durable task metadata and history reconciliation indefinitely.
      if (refreshTimer !== null) return;
      refreshTimer = window.setTimeout(() => {
        refreshTimer = null;
        void loadSnapshot();
      }, delay);
    };
    const wait = (delay: number) => new Promise<void>((resolve) => {
      const timer = window.setTimeout(resolve, delay);
      controller.signal.addEventListener("abort", () => { window.clearTimeout(timer); resolve(); }, { once: true });
    });
    const runStream = async () => {
      let retryDelay = 1000;
      while (!stopped && !controller.signal.aborted) {
        try {
          const query = new URLSearchParams({ instanceId: instanceID });
          await consumeMobileEventStream(`${cloudURL}/v1/stream?${query.toString()}`, tokenValue, lastEventID, (event) => {
            if (event.lastEventId) lastEventID = event.lastEventId;
            notifyHiddenMobileEvent(event);
            const isMessage = applyRealtimeEvent(event);
            scheduleSnapshotRefresh(isMessage ? 1000 : 500);
          }, controller.signal);
          if (stopped || controller.signal.aborted) break;
          await wait(retryDelay);
          retryDelay = 1000;
        } catch {
          if (stopped || controller.signal.aborted) break;
          if (fallbackTimer === null) fallbackTimer = window.setInterval(() => { void loadSnapshot(); }, 15_000);
          await wait(retryDelay);
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    };
    void runStream();
    return () => {
      stopped = true;
      controller.abort();
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer);
      if (refreshTimer !== null) window.clearTimeout(refreshTimer);
    };
  }, [instanceID, loadSnapshot, applyRealtimeEvent, notifyHiddenMobileEvent]);
  useEffect(() => {
    let cancelled = false;
    if (!pairingURL) {
      setPairingQR("");
      return () => { cancelled = true; };
    }
    void QRCode.toDataURL(pairingURL, { width: 240, margin: 1, errorCorrectionLevel: "M" })
      .then((dataURL) => { if (!cancelled) setPairingQR(dataURL); })
      .catch(() => { if (!cancelled) setPairingQR(""); });
    return () => { cancelled = true; };
  }, [pairingURL]);
  useEffect(() => {
    if (!commandState || terminalCommandStatuses.includes(commandState.status)) return;
    const timer = window.setInterval(() => {
      void cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandState.commandId)}`)
        .then((value) => setCommandState((current) => mergeCommandState(current, normalizeCommandState(value, commandState.commandId))))
        .catch(() => undefined);
    }, 5000);
    return () => window.clearInterval(timer);
  }, [commandState?.commandId, commandState?.status]);
  useEffect(() => {
    if (!pendingAccessToken || !pairingID) return;
    const timer = window.setInterval(() => {
      void cloud<{ status: string }>(`/v1/pairings/${encodeURIComponent(pairingID)}/status`)
        .then((state) => {
          if (state.status === "confirmed") {
            localStorage.setItem("milevia.cloud.token", pendingAccessToken);
            setToken(pendingAccessToken);
            setPendingAccessToken("");
            setPairingExpanded(false);
            setPairingStatus("电脑已确认，绑定完成");
            void loadInstances();
          } else if (["expired", "cancelled"].includes(state.status)) {
            setPendingAccessToken("");
            setPairingStatus("配对已失效，请让电脑重新生成二维码");
          }
        })
        .catch(() => undefined);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [pendingAccessToken, pairingID, loadInstances]);

  const instance = instances.find((item) => item.instanceId === instanceID);
  const projects = snapshot?.projects || [];
  const project = projects.find((item) => item.id === selectedProject);
  // Keep the agent visible wherever the mobile conversation title is shown.
  // The API title remains untouched; this is presentation-only decoration.
  const conversations = useMemo(() => (project?.conversations || []).map((item) => ({
    ...item,
    title: `${item.title || "未命名会话"} · ${conversationAgentLabel(item.agentId)}`,
  })), [project?.conversations]);
  const conversation = conversations.find((item) => item.id === selectedConversation) || conversations.find((item) => item.isCurrent) || conversations[0];
  const conversationProcessing = Boolean(conversation && (conversation.status === "running" || processingConversations[conversation.id]));

  // Normalize an older cached snapshot as well as network responses. This
  // keeps revision comparisons numeric during offline recovery.
  useEffect(() => {
    if (!snapshot || Number.isFinite(snapshot.snapshotRevision)) return;
    setSnapshot((current) => current && !Number.isFinite(current.snapshotRevision)
      ? { ...current, snapshotRevision: 0 }
      : current);
  }, [snapshot]);

  // SSE can be unavailable while snapshots continue to refresh. Clear the
  // local optimistic indicator once a completed assistant reply is present.
  useEffect(() => {
    if (!snapshot || Object.keys(processingConversations).length === 0) return;
    const completedIDs = new Set<string>();
    for (const [conversationID, state] of Object.entries(processingConversations)) {
      const remoteConversation = snapshot.projects.flatMap((item) => item.conversations).find((item) => item.id === conversationID);
      if (!remoteConversation) {
        completedIDs.add(conversationID);
        continue;
      }
      if (["failed", "stopped", "cancelled"].includes(remoteConversation.status)) {
        if (snapshot.snapshotRevision > state.snapshotRevision) completedIDs.add(conversationID);
        continue;
      }
      // A run can finish without an assistant message (for example, a
      // startup failure, cancellation, or service restart). Once a newer
      // durable snapshot says the conversation is no longer running, the
      // indicator must not remain stuck. Keep it while the optimistic user
      // message is still waiting to be persisted so a queued command cannot
      // be cleared by an unrelated snapshot update.
      const pending = pendingMessageRef.current.get(conversationID);
      const hasPendingMessage = Boolean(pending && pending.size > 0);
      if (snapshot.snapshotRevision > state.snapshotRevision && remoteConversation.status !== "running" && !hasPendingMessage) completedIDs.add(conversationID);
    }
    if (completedIDs.size > 0) {
      setProcessingConversations((current) => {
        const next = { ...current };
        completedIDs.forEach((id) => delete next[id]);
        return next;
      });
    }
  }, [snapshot, processingConversations]);
  const taskCount = useMemo(() => projects.reduce((sum, item) => sum + item.tasks.length, 0), [projects]);
  const visibleTasks = useMemo(() => {
    const tasks = project?.tasks || [];
    return taskFilter === "all" ? tasks : tasks.filter((task) => task.status === taskFilter);
  }, [project?.tasks, taskFilter]);

  // Keep the extra task controls synchronized with React state. The task row
  // markup predates these controls, so reconcile the small imperative portion
  // whenever the filtered snapshot or command state changes.
  useEffect(() => {
    if (!tasksOpen) return;
    const timer = window.setTimeout(() => {
      document.querySelectorAll<HTMLElement>(".mobile-task-drawer .mobile-task").forEach((row, index) => {
        const task = visibleTasks[index];
        const actions = row.querySelector<HTMLElement>(".mobile-task-actions");
        if (!task || !actions) return;
        const canEdit = task.status === "todo" || task.status === "action_required";
        let edit = actions.querySelector<HTMLButtonElement>("[data-mobile-task-edit]");
        if (canEdit && !edit) {
          edit = document.createElement("button");
          edit.type = "button"; edit.textContent = "编辑"; edit.dataset.mobileTaskEdit = "true";
          actions.append(edit);
        }
        if (edit) {
          edit.hidden = !canEdit;
          edit.disabled = busy || !canEdit;
          edit.onclick = (event) => { event.stopPropagation(); openTaskEditor(task); };
        }
        let remove = actions.querySelector<HTMLButtonElement>("[data-mobile-task-delete]");
        if (!remove) {
          remove = document.createElement("button");
          remove.type = "button"; remove.textContent = "删除"; remove.dataset.mobileTaskDelete = "true"; remove.className = "mobile-task-delete";
          actions.append(remove);
        }
        remove.hidden = false;
        remove.disabled = busy || task.status === "running";
        remove.onclick = (event) => { event.stopPropagation(); setDeletingTask(task); };
      });
    }, 0);
    return () => window.clearTimeout(timer);
  }, [tasksOpen, visibleTasks, taskFilter, busy]);
  const showMobilePairing = mobileApp && mobileView === "projects" && (!token.trim() || instances.length === 0 || pairingExpanded);

  useEffect(() => {
    if (!selectedProject || projects.some((item) => item.id === selectedProject)) return;
    setSelectedProject("");
    setSelectedConversation("");
    setMobileView("projects");
    setTasksOpen(false);
  }, [projects, selectedProject]);
  useEffect(() => {
    setMessageDraft("");
    setConversationMenuOpen(false);
  }, [selectedProject, selectedConversation]);
  useEffect(() => {
    if (!conversationMenuOpen) return;
    const closeOnOutsidePointer = (event: PointerEvent) => {
      if (!conversationMenuRef.current?.contains(event.target as Node)) setConversationMenuOpen(false);
    };
    document.addEventListener("pointerdown", closeOnOutsidePointer);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointer);
  }, [conversationMenuOpen]);
  useEffect(() => {
    selectedConversationRef.current = selectedConversation;
  }, [selectedConversation]);
  useEffect(() => {
    const input = messageInputRef.current;
    if (!input) return;
    input.style.height = "auto";
    input.style.height = `${Math.min(input.scrollHeight, 120)}px`;
  }, [messageDraft]);

  function saveToken(event: FormEvent) {
    event.preventDefault();
    const nextToken = token.trim();
    localStorage.setItem("milevia.cloud.token", nextToken);
    setInstances([]);
    setInstanceID("");
    setSnapshot(null);
    setSelectedProject("");
    setSelectedConversation("");
    setMobileView("projects");
    setPairingExpanded(false);
    void loadInstances();
  }

  async function enableMobileNotifications() {
    if (typeof Notification === "undefined") return;
    try {
      const permission = await Notification.requestPermission();
      setNotificationPermission(permission);
    } catch {
      setError("无法请求通知权限，请在浏览器或系统设置中允许通知");
    }
  }

  async function claimPairing(pairingIDValue = pairingID, pairingCodeValue = pairingCode) {
    if (!pairingIDValue.trim() || !/^\d{6}$/.test(pairingCodeValue.trim())) {
      setError("二维码缺少有效的配对信息，请让电脑重新生成二维码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ instanceId: string; status: string }>(`/v1/pairings/${encodeURIComponent(pairingIDValue.trim())}/claim`, { method: "POST", body: JSON.stringify({ code: pairingCodeValue.trim() }) });
      const accessToken = (result as { accessToken?: string }).accessToken;
      if (accessToken) setPendingAccessToken(accessToken);
      setPairingStatus("已扫描，等待电脑确认");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "配对失败"); }
    finally { setBusy(false); }
  }

  useEffect(() => {
    const { pairingID: initialPairingID, code: initialPairingCode } = initialPairing.current;
    if (!mobileApp || initialPairing.current.attempted || !initialPairingID || !/^\d{6}$/.test(initialPairingCode)) return;
    initialPairing.current.attempted = true;
    void claimPairing(initialPairingID, initialPairingCode);
  }, [mobileApp]);

  async function claimPairingByCode(event: FormEvent) {
    event.preventDefault();
    const code = manualPairingCode.trim();
    if (!/^\d{6}$/.test(code)) {
      setError("请输入 6 位校验码");
      return;
    }
    setBusy(true); setError("");
    try {
      const result = await cloud<{ pairingId: string; instanceId: string; status: string; accessToken?: string }>("/v1/pairings/claim", { method: "POST", body: JSON.stringify({ code }) });
      setPairingID(result.pairingId || "");
      setPairingCode(code);
      if (result.accessToken) setPendingAccessToken(result.accessToken);
      setPairingStatus("校验码已提交，等待电脑确认");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "校验码配对失败");
    } finally {
      setBusy(false);
    }
  }

  async function confirmDesktopPairing() {
    if (!pairingID.trim()) return;
    setBusy(true); setError("");
    try {
      await api(`/api/remote/pairing/confirm`, { method: "POST", body: JSON.stringify({ pairingId: pairingID.trim() }) });
      setPairingStatus("已确认绑定，手机可以开始使用");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "确认绑定失败"); }
    finally { setBusy(false); }
  }

  async function createDesktopPairing() {
    setBusy(true); setError("");
    try {
      const value = await api<{ pairingId: string; code: string; pairingURL?: string }>("/api/remote/pairing", { method: "POST" });
      value.pairingURL = pairingURLWithCode(value.pairingURL, value.pairingId || "");
      setPairingID(value.pairingId || ""); setPairingCode(value.code || ""); setPairingURL(value.pairingURL || ""); setPairingStatus("二维码已生成，有效期 5 分钟");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "无法生成配对二维码"); }
    finally { setBusy(false); }
  }

  async function createTask(event: FormEvent) {
    event.preventDefault();
    if (!project || !title.trim()) return;
    setBusy(true); setError("");
    try {
	  const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "task.create", projectId: project.id, payload: { title: title.trim(), description: description.trim(), priority: "normal" } }),
      });
	  setCommandState(accepted);
	  const finalState = await waitForCommand(accepted.commandId);
	  if (!finalState || finalState.status !== "completed") {
	    setError(finalState ? "任务创建失败，请检查电脑端 Agent 状态" : "任务仍在处理中，请稍后刷新查看结果");
	    return;
	  }
      setTitle(""); setDescription("");
      await loadSnapshot();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "创建任务失败"); }
    finally { setBusy(false); }
  }

  async function sendTaskCommand(taskID: string, type: string, payload: Record<string, unknown> = {}): Promise<boolean> {
    setBusy(true); setError("");
    try {
      const commandPayload = type === "task.review" ? { action: "accept", ...payload } : payload;
      const accepted = await cloud<AcceptedCommand>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: JSON.stringify({ type, taskId: taskID, payload: commandPayload }) });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (!finalState || finalState.status !== "completed") {
        setError(finalState ? "任务操作失败，请检查任务状态" : "任务操作仍在处理中，请稍后刷新");
        return false;
      }
      await loadSnapshot();
      return true;
    } catch (cause) { setError(cause instanceof Error ? cause.message : "命令发送失败"); }
    finally { setBusy(false); }
    return false;
  }

  function openTaskEditor(task: Task) {
    setEditingTask(task);
    setEditTitle(task.title || "");
    setEditDescription(task.description || "");
    setEditPriority(task.priority || "normal");
  }

  async function saveTaskEdit(event: FormEvent) {
    event.preventDefault();
    if (!editingTask || !editTitle.trim()) return;
    if (!editDescription.trim()) {
      setError("任务描述不能为空");
      return;
    }
    if (await sendTaskCommand(editingTask.id, "task.update", { title: editTitle.trim(), description: editDescription.trim(), priority: editPriority })) setEditingTask(null);
  }

  async function confirmTaskDelete() {
    if (!deletingTask) return;
    if (await sendTaskCommand(deletingTask.id, "task.delete")) setDeletingTask(null);
  }

  async function sendConversationMessage(event: FormEvent) {
    event.preventDefault();
    if (!conversation || !messageDraft.trim()) return;
    const content = messageDraft.trim();
    const conversationID = conversation.id;
    const clientRequestId = idempotencyKey();
    const createdAt = new Date().toISOString();
    const optimisticID = `pending-${clientRequestId}`;
    const pending = pendingMessageRef.current.get(conversationID) || new Map<string, PendingMessage>();
    pending.set(clientRequestId, { requestId: clientRequestId, content, createdAt });
    pendingMessageRef.current.set(conversationID, pending);
    markConversationProcessing(conversationID, snapshot?.snapshotRevision ?? -1);
    setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID && !entry.messages.some((message) => message.id === optimisticID) ? { ...entry, messages: [...entry.messages, { id: optimisticID, role: "user", content, createdAt }] } : entry) })) } : current);
    setMessageDraft("");
    setBusy(true); setError("");
    try {
      const accepted = await cloud<{ commandId: string; status: string }>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": clientRequestId },
        body: JSON.stringify({ type: "conversation.message", payload: { conversationId: conversationID, content, clientRequestId } }),
      });
      setCommandState(accepted);
      while (pending.size > 50) {
        const oldest = pending.keys().next().value;
        if (!oldest) break;
        pending.delete(oldest);
      }
      void loadSnapshot();
      void waitForCommand(accepted.commandId).then((finalState) => {
        if (finalState?.status === "completed") {
          // The optimistic entry and SSE event are already visible. One
          // background snapshot reconciles status/history without a polling
          // storm when the Agent uploads its next snapshot.
          void loadSnapshot();
          return;
        }
        const stillOnConversation = selectedConversationRef.current === conversationID;
        if (finalState?.status !== "completed") {
          clearConversationProcessing(conversationID);
          const pendingForConversation = pendingMessageRef.current.get(conversationID);
          pendingForConversation?.delete(clientRequestId);
          if (pendingForConversation?.size === 0) pendingMessageRef.current.delete(conversationID);
          setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID ? { ...entry, messages: entry.messages.filter((message) => message.id !== optimisticID) } : entry) })) } : current);
        }
        const anotherMessagePending = (pendingMessageRef.current.get(conversationID)?.size || 0) > 0;
        if (finalState && stillOnConversation && !anotherMessagePending) {
          setMessageDraft((current) => current.trim() ? current : content);
          const detail = commandFailureDetail(finalState.result);
          setError(`消息执行失败：${detail || "输入内容仍保留在输入框"}`);
        } else if (!finalState && !anotherMessagePending) {
          if (stillOnConversation) setMessageDraft((current) => current.trim() ? current : content);
          setError("消息状态暂时无法确认，输入内容仍保留在输入框");
        }
      });
    } catch (cause) {
      clearConversationProcessing(conversationID);
      pending.delete(clientRequestId);
      if (pending.size === 0) pendingMessageRef.current.delete(conversationID);
      setSnapshot((current) => current ? { ...current, projects: current.projects.map((item) => ({ ...item, conversations: item.conversations.map((entry) => entry.id === conversationID ? { ...entry, messages: entry.messages.filter((message) => message.id !== optimisticID) } : entry) })) } : current);
      setMessageDraft(content);
      setError(cause instanceof Error ? cause.message : "消息发送失败");
    }
    finally { setBusy(false); }
  }

  async function createConversationForProject(projectValue: Project, agentId?: "claude-code" | "codex") {
    if (!agentId) {
      openNewConversation(projectValue);
      return;
    }
    const previousIDs = new Set((projectValue.conversations || []).map((item) => item.id));
    creatingProjectRef.current = projectValue.id;
    setBusy(true);
    setError("");
    try {
      const accepted = await cloud<{ commandId: string; status: string }>(`/v1/instances/${encodeURIComponent(instanceID)}/commands`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: JSON.stringify({ type: "conversation.create", projectId: projectValue.id, payload: { agentId } }),
      });
      setCommandState(accepted);
      const finalState = await waitForCommand(accepted.commandId);
      if (finalState?.status !== "completed") {
        const detail = commandFailureDetail(finalState?.result);
        setError(finalState ? `无法创建会话：${detail || "请检查电脑端 Agent 状态"}` : "创建会话仍在处理中，请稍后刷新");
        return;
      }
      const preferredConversationID = conversationIDFromCommandResult(finalState.result);
      const returnedConversation = conversationFromCommandResult(finalState.result);
      const created = returnedConversation || projectValue.conversations?.find((item) => item.id === preferredConversationID)
        || projectValue.conversations?.find((item) => !previousIDs.has(item.id));
      if (!created || (preferredConversationID ? created.id !== preferredConversationID : previousIDs.has(created.id))) {
        setError("会话已创建，但同步尚未完成，请稍后刷新");
        return;
      }
      if (returnedConversation) {
        setSnapshot((current) => current ? {
          ...current,
          projects: current.projects.map((item) => item.id !== projectValue.id ? item : {
            ...item,
            conversations: [returnedConversation, ...item.conversations.filter((entry) => entry.id !== returnedConversation.id).map((entry) => ({ ...entry, isCurrent: false }))],
          }),
        } : current);
      }
      setSelectedProject(projectValue.id);
      setSelectedConversation(created?.id || "");
      setTasksOpen(false);
      setPairingExpanded(false);
      setMobileView("conversation");
      setNewConversationProject(null);
      // Reconcile project/task metadata in the background. The returned
      // conversation is already sufficient to render and send immediately.
      void loadSnapshot();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "无法创建会话");
    } finally {
      if (creatingProjectRef.current === projectValue.id) creatingProjectRef.current = "";
      setBusy(false);
    }
  }

  async function openMobileProject(projectValue: Project) {
    setSelectedProject(projectValue.id);
    setTasksOpen(false);
    setPairingExpanded(false);
    const existingConversation = projectValue.conversations?.find((entry) => entry.isCurrent) || projectValue.conversations?.[0];
    if (existingConversation) {
      setSelectedConversation(existingConversation.id);
      setMobileView("conversation");
      return;
    }
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function openNewConversation(projectValue: Project) {
    setNewConversationAgent("claude-code");
    setNewConversationProject(projectValue);
  }

  function cancelNewConversation() {
    if (!busy) setNewConversationProject(null);
  }

  async function waitForCommand(commandID: string) {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      const requestController = new AbortController();
      const requestTimeout = window.setTimeout(() => requestController.abort(), Math.min(remaining, cloudRequestTimeoutMs));
      try {
        const response = await cloud<CommandState & { command?: { commandId?: string } }>(`/v1/commands/${encodeURIComponent(commandID)}`, { signal: requestController.signal });
        const state = normalizeCommandState(response, commandID);
        setCommandState((current) => mergeCommandState(current, state));
        if (terminalCommandStatuses.includes(state.status)) return state;
      } catch {
        // A transient mobile network failure must not be treated as command
        // failure. Keep polling until the bounded wait expires.
      } finally {
        window.clearTimeout(requestTimeout);
      }
      await new Promise((resolve) => window.setTimeout(resolve, Math.min(500, Math.max(0, deadline - Date.now()))));
    }
    return null;
  }

  function goBack() {
    if (mobileApp && mobileView === "conversation") {
      setMobileView("projects");
      setTasksOpen(false);
      setPairingExpanded(false);
      return;
    }
    if (window.history.length > 1 && !Capacitor.isNativePlatform()) {
      navigate(-1);
    } else if (Capacitor.isNativePlatform()) {
      void CapacitorApp.minimizeApp().catch(() => undefined);
    } else if (!Capacitor.isNativePlatform()) {
      navigate("/");
    }
  }

  return <main className={`mobile-remote ${mobileApp && mobileView === "conversation" ? "mobile-conversation-mode" : ""}`}>
    {editingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-edit-title"><form className="mobile-task-modal" onSubmit={(event) => void saveTaskEdit(event)}><header><h2 id="mobile-task-edit-title">编辑任务</h2><button type="button" onClick={() => setEditingTask(null)} disabled={busy} aria-label="关闭">×</button></header><label>标题<input value={editTitle} onChange={(event) => setEditTitle(event.target.value)} required /></label><label>描述<textarea value={editDescription} onChange={(event) => setEditDescription(event.target.value)} rows={4} /></label><label>优先级<select value={editPriority} onChange={(event) => setEditPriority(event.target.value)}><option value="urgent">紧急</option><option value="high">高</option><option value="normal">普通</option><option value="low">低</option></select></label><footer><button type="button" onClick={() => setEditingTask(null)} disabled={busy}>取消</button><button type="submit" disabled={busy || !editTitle.trim()}>保存</button></footer></form></div>}
    {deletingTask && <div className="mobile-task-modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-task-delete-title"><section className="mobile-task-modal mobile-task-delete-modal"><header><h2 id="mobile-task-delete-title">删除任务</h2><button type="button" onClick={() => setDeletingTask(null)} disabled={busy} aria-label="关闭">×</button></header><p>确定删除“{taskSummary(deletingTask)}”吗？删除后无法恢复。</p><footer><button type="button" onClick={() => setDeletingTask(null)} disabled={busy}>取消</button><button type="button" className="mobile-task-delete-confirm" onClick={() => void confirmTaskDelete()} disabled={busy}>确认删除</button></footer></section></div>}
    <header className="mobile-remote-header"><div className="mobile-remote-title">{(!mobileApp || mobileView === "conversation") && <button className="mobile-back" type="button" onClick={goBack} title={mobileView === "conversation" ? "返回项目" : "返回"} aria-label={mobileView === "conversation" ? "返回项目" : "返回"}>←</button>}<div className="mobile-brand"><img className="mobile-brand-mark" src="/milevia-mark.svg" width="36" height="36" alt="" /><h1>{mobileApp && mobileView === "conversation" ? (project?.name || "项目对话") : "Milevia"}</h1></div></div><div className="mobile-header-actions">{notificationPermission === "default" && <button className="mobile-notification-button" type="button" onClick={() => void enableMobileNotifications()} title="开启后台通知">开启通知</button>}<button className="mobile-refresh" type="button" onClick={() => { void loadInstances(); void loadSnapshot(); }} title="刷新">刷新</button></div></header>
    {(!mobileApp || showMobilePairing) && <section className="mobile-pairing"><div><h2>扫码配对</h2><p>{mobileApp ? "扫描电脑上的二维码即可自动完成配对。" : "点击生成二维码，用手机扫码后，再点击确认绑定。"}</p></div>{!mobileApp && <button className="mobile-pairing-generate" onClick={() => void createDesktopPairing()} disabled={busy}>生成二维码</button>}{mobileApp && <button className="mobile-pairing-generate" onClick={() => { scanAccepted.current = false; setScanError(""); setScanning(true); }} disabled={busy || scanning}>扫描二维码</button>}{scanning && <div className="mobile-pairing-scanner-shell"><video className="mobile-pairing-scanner" ref={setScanVideo} muted playsInline /><div className="mobile-pairing-scanner-frame" /><p>将二维码放入框内</p><button className="mobile-pairing-scan-cancel" onClick={() => setScanning(false)}>取消扫描</button></div>}{scanError && <small className="mobile-pairing-scan-error">{scanError}</small>}{pairingQR && <img className="mobile-pairing-qr" src={pairingQR} alt="Milevia 配对二维码" />}{!mobileApp && pairingID && <button className="mobile-pairing-confirm" onClick={() => void confirmDesktopPairing()} disabled={busy}>确认绑定</button>}{pairingStatus && <small>{pairingStatus}</small>}</section>}
    {mobileApp && showMobilePairing && <section className="mobile-pairing-manual"><h2>使用校验码</h2><p>在电脑端生成校验码后，在此输入 6 位数字。</p><form onSubmit={(event) => void claimPairingByCode(event)}><input inputMode="numeric" pattern="[0-9]{6}" maxLength={6} value={manualPairingCode} onChange={(event) => setManualPairingCode(event.target.value.replace(/\D/g, "").slice(0, 6))} placeholder="6 位校验码" aria-label="6 位校验码" /><button type="submit" disabled={busy || manualPairingCode.length !== 6}>验证并配对</button></form>{token.trim() && <button className="mobile-pairing-collapse" type="button" onClick={() => setPairingExpanded(false)}>返回项目</button>}</section>}
    {!mobileApp && pairingCode && <div className="mobile-pairing-code">校验码：<strong>{pairingCode}</strong></div>}
    {error && <div className="mobile-error" role="alert">{error}</div>}
    {commandState && <div className={`mobile-command-status ${commandState.status}`}>命令 {commandState.commandId}：{commandState.status}</div>}
    {(!mobileApp || mobileView === "projects") && instance && <section className="mobile-instance-status"><div><strong>{instance.name || instance.instanceId}</strong><span className={`mobile-status ${instance.status}`}>{instance.status}</span></div><small>事件序号 {instance.lastAgentSequence} · {instance.lastSeenAt ? new Date(instance.lastSeenAt).toLocaleString() : "尚未连接"}</small></section>}
    {(!mobileApp || mobileView === "projects") && <section className="mobile-summary"><span><b>{projects.length}</b><small>项目</small></span><span><b>{taskCount}</b><small>任务</small></span></section>}
    {mobileApp && mobileView === "projects" && <section className="mobile-project-picker"><div className="mobile-section-heading"><h2>选择项目</h2><span>{snapshot ? new Date(snapshot.observedAt).toLocaleTimeString() : "加载中"}</span></div>{projects.length === 0 ? <p className="mobile-empty">暂无项目或电脑尚未同步。</p> : projects.map((item) => { const environment = projectEnvironment(item); const running = item.running === true; return <button type="button" className="mobile-project-choice" key={item.id} onClick={() => void openMobileProject(item)} disabled={busy}><span className="mobile-project-choice-content"><strong>{item.name}</strong><span className="mobile-project-choice-status"><span className={`mobile-project-environment ${environment}`} title={`${projectEnvironmentLabel(environment)} 项目`}><ProjectEnvironmentIcon environment={environment} />{projectEnvironmentLabel(environment)}</span><span className={`mobile-project-running ${running ? "running" : "idle"}`}><i></i>{running ? "运行中" : "未运行"}</span></span><small>{item.gitBranch || "默认分支"} · {item.tasks.length} 个任务</small></span><b>{item.conversations?.length || 0} 个会话</b></button>; })}</section>}
    {mobileApp && mobileView === "projects" && token.trim() && instances.length > 0 && !pairingExpanded && <button className="mobile-repair" type="button" onClick={() => setPairingExpanded(true)}>重新配对此设备</button>}
    {mobileApp && mobileView === "conversation" && project && <section className="mobile-conversation"><div className="mobile-conversation-toolbar"><div><h2>{conversation?.title || "暂无会话"}</h2><small>{conversation ? conversationStatusLabel(conversation.status) : "该项目还没有对话"}</small></div><div className="mobile-conversation-toolbar-actions"><div className="mobile-conversation-picker" ref={conversationMenuRef}><button className="mobile-conversation-picker-trigger" type="button" aria-label="选择会话" aria-haspopup="listbox" aria-expanded={conversationMenuOpen} onClick={() => setConversationMenuOpen((open) => !open)} disabled={conversations.length === 0}>{conversation?.title || "暂无会话"}<span aria-hidden="true">⌄</span></button>{conversationMenuOpen && <div className="mobile-conversation-options" role="listbox" aria-label="会话列表">{conversations.map((item) => <button type="button" role="option" aria-selected={item.id === conversation?.id} key={item.id} onClick={() => { setSelectedConversation(item.id); setConversationMenuOpen(false); }}>{item.title || "未命名会话"}<small>{conversationStatusLabel(item.status)}</small></button>)}</div>}</div><button type="button" className="mobile-new-conversation" onClick={() => void createConversationForProject(project)} disabled={busy} title="新建会话">新会话</button><button type="button" className="mobile-task-toggle" onClick={() => setTasksOpen(true)} aria-expanded={tasksOpen}>任务 <span>{project.tasks.length}</span></button></div></div>{conversationProcessing && <div className="mobile-agent-processing" role="status" aria-live="polite"><span className="mobile-agent-processing-dots" aria-hidden="true"><i></i><i></i><i></i></span><span>{conversation ? conversationAgentLabel(conversation.agentId) : "Agent"} 正在处理...</span></div>}<div className="mobile-message-list">{conversation?.messages?.length ? conversation.messages.map((message) => <article className={`mobile-message ${message.role}`} key={message.id}><small>{message.role === "user" ? "我" : "Agent"} · {new Date(message.createdAt).toLocaleString()}</small><div className="mobile-message-markdown markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer">{children}</a> }}>{message.content}</ReactMarkdown></div></article>) : <p className="mobile-empty">{conversation ? "该会话暂无对话内容。" : "请先新建会话。"}</p>}</div>{tasksOpen && <><button type="button" className="mobile-task-drawer-backdrop" aria-label="关闭任务面板" onClick={() => setTasksOpen(false)} /><aside className="mobile-task-drawer" aria-label="任务与操作"><header><div><h3>任务与操作</h3><small>{project.tasks.length} 个任务</small></div><button type="button" onClick={() => setTasksOpen(false)} aria-label="关闭任务面板" title="关闭">×</button></header><nav className="mobile-task-filters" aria-label="任务状态分类" role="tablist">{taskFilters.map((filter) => { const count = filter.id === "all" ? project.tasks.length : project.tasks.filter((task) => task.status === filter.id).length; return <button type="button" role="tab" aria-selected={taskFilter === filter.id} className={taskFilter === filter.id ? "active" : ""} key={filter.id} onClick={() => setTaskFilter(filter.id)}>{filter.label}<span>{count}</span></button>; })}</nav><div className="mobile-task-list">{visibleTasks.length === 0 ? <p className="mobile-empty">当前分类没有任务。</p> : visibleTasks.map((task) => <div className="mobile-task" key={task.id}><div><strong>{taskSummary(task)}</strong><span className={`mobile-task-status ${taskStatusClass(task.status)}`}>{taskStatusLabel(task.status)}</span><small>{task.priority || "normal"}</small><details className="mobile-task-disclosure"><summary>查看详情</summary><p>{task.description?.trim() || "暂无任务描述"}</p><time dateTime={task.updatedAt}>更新于 {new Date(task.updatedAt).toLocaleString()}</time></details></div><div className="mobile-task-actions">{task.status === "todo" || task.status === "action_required" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.dispatch")}>下发</button> : null}{task.status === "awaiting_review" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.review")}>验收</button> : null}{task.status === "running" ? <button type="button" disabled={busy} onClick={() => void sendTaskCommand(task.id, "task.stop")}>停止</button> : null}</div></div>)}</div><div className="mobile-create"><h3>创建任务</h3><form onSubmit={createTask}><label>标题<input value={title} onChange={(event) => setTitle(event.target.value)} required placeholder="要处理的事情" /></label><label>描述<textarea value={description} onChange={(event) => setDescription(event.target.value)} placeholder="补充上下文（可选）" rows={3} /></label><button type="submit" disabled={busy || !title.trim()}>创建并排队</button></form></div></aside></>}<form className="mobile-composer" onSubmit={sendConversationMessage}><textarea ref={messageInputRef} value={messageDraft} onChange={(event) => setMessageDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} placeholder={conversation ? "输入消息..." : "请先新建会话"} aria-label="输入消息" rows={1} disabled={busy || !conversation} /><button type="submit" disabled={busy || !conversation || !messageDraft.trim()} aria-label="发送消息" title="发送消息">↑</button></form></section>}
    {!mobileApp && <section className="mobile-projects"><div className="mobile-section-heading"><h2>项目与任务</h2><span>{snapshot ? new Date(snapshot.observedAt).toLocaleTimeString() : "加载中"}</span></div>{projects.length === 0 ? <p className="mobile-empty">暂无项目或电脑尚未同步。</p> : projects.map((item) => <article className={`mobile-project ${project?.id === item.id ? "selected" : ""}`} key={item.id} role="button" tabIndex={0} aria-expanded={project?.id === item.id} onClick={() => setSelectedProject(item.id)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); setSelectedProject(item.id); } }}><header><div><h3>{item.name}</h3><small>{item.gitBranch || "默认分支"}</small></div><span>{item.tasks.length} 个任务</span></header>{project?.id === item.id && <div className="mobile-task-list">{item.tasks.length === 0 ? <p className="mobile-empty">还没有任务。</p> : item.tasks.map((task) => <div className="mobile-task" key={task.id}><div><strong>{task.title}</strong><small>{task.priority} · {task.status}</small></div><div className="mobile-task-actions"><button type="button" disabled={busy} onClick={(event) => { event.stopPropagation(); void sendTaskCommand(task.id, task.status === "running" ? "task.stop" : task.status === "awaiting_review" ? "task.review" : "task.dispatch"); }}>操作</button></div></div>)}</div>}</article>)}</section>}
    {!mobileApp && project && <section className="mobile-create"><h2>创建任务 · {project.name}</h2><form onSubmit={createTask}><label>标题<input value={title} onChange={(event) => setTitle(event.target.value)} required placeholder="要处理的事情" /></label><label>描述<textarea value={description} onChange={(event) => setDescription(event.target.value)} placeholder="补充上下文（可选）" rows={3} /></label><button type="submit" disabled={busy || !title.trim()}>创建并排队</button></form></section>}
    {newConversationProject && <div className="mobile-new-conversation-backdrop" role="dialog" aria-modal="true" aria-labelledby="mobile-new-conversation-title"><section className="mobile-new-conversation-dialog"><header><div><h2 id="mobile-new-conversation-title">新会话</h2><p>选择执行 Agent</p></div><button type="button" onClick={cancelNewConversation} disabled={busy} aria-label="关闭">×</button></header><div className="mobile-agent-options" role="radiogroup" aria-label="选择执行 Agent"><button type="button" role="radio" aria-checked={newConversationAgent === "claude-code"} className={newConversationAgent === "claude-code" ? "active" : ""} onClick={() => setNewConversationAgent("claude-code")}><strong>Claude Code</strong><small>使用 Claude Code 执行</small></button><button type="button" role="radio" aria-checked={newConversationAgent === "codex"} className={newConversationAgent === "codex" ? "active" : ""} onClick={() => setNewConversationAgent("codex")}><strong>Codex</strong><small>使用 Codex 执行</small></button></div><footer><button type="button" className="mobile-new-conversation-cancel" onClick={cancelNewConversation} disabled={busy}>取消</button><button type="button" className="mobile-new-conversation-confirm" onClick={() => void createConversationForProject(newConversationProject, newConversationAgent)} disabled={busy}>创建会话</button></footer></section></div>}
  </main>;
}
