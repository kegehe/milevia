export type SourceLanguageID =
  | "javascript"
  | "typescript"
  | "tsx"
  | "jsx"
  | "css"
  | "html"
  | "json"
  | "markdown"
  | "python"
  | "go"
  | "sql"
  | "java"
  | "cpp"
  | "rust"
  | "yaml"
  | "xml"
  | "text";

type SourceLanguage = {
  id: SourceLanguageID;
  extensions: readonly string[];
  names?: readonly string[];
  load?: () => Promise<unknown>;
};

const languages: readonly SourceLanguage[] = [
  { id: "tsx", extensions: [".tsx"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true, typescript: true })) },
  { id: "typescript", extensions: [".ts", ".mts", ".cts"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ typescript: true })) },
  { id: "jsx", extensions: [".jsx"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true })) },
  { id: "javascript", extensions: [".js", ".mjs", ".cjs"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript()) },
  { id: "css", extensions: [".css", ".scss", ".sass", ".less"], load: () => import("@codemirror/lang-css").then((m) => m.css()) },
  { id: "html", extensions: [".html", ".htm", ".vue", ".svelte"], load: () => import("@codemirror/lang-html").then((m) => m.html()) },
  { id: "json", extensions: [".json", ".jsonc", ".json5", ".map", ".lock"], load: () => import("@codemirror/lang-json").then((m) => m.json()) },
  { id: "markdown", extensions: [".md", ".markdown", ".mdx"], names: ["readme", "changelog"], load: () => import("@codemirror/lang-markdown").then((m) => m.markdown()) },
  { id: "python", extensions: [".py"], load: () => import("@codemirror/lang-python").then((m) => m.python()) },
  { id: "go", extensions: [".go"], load: () => import("@codemirror/lang-go").then((m) => m.go()) },
  { id: "sql", extensions: [".sql"], load: () => import("@codemirror/lang-sql").then((m) => m.sql()) },
  { id: "java", extensions: [".java"], load: () => import("@codemirror/lang-java").then((m) => m.java()) },
  { id: "cpp", extensions: [".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx"], load: () => import("@codemirror/lang-cpp").then((m) => m.cpp()) },
  { id: "rust", extensions: [".rs"], load: () => import("@codemirror/lang-rust").then((m) => m.rust()) },
  { id: "yaml", extensions: [".yaml", ".yml"], load: () => import("@codemirror/lang-yaml").then((m) => m.yaml()) },
  { id: "xml", extensions: [".xml", ".svg"], load: () => import("@codemirror/lang-xml").then((m) => m.xml()) },
  { id: "text", extensions: [".txt", ".log", ".diff", ".patch", ".toml", ".ini", ".cfg", ".conf", ".env", ".sh", ".bash", ".zsh", ".fish", ".rb", ".kt", ".scala", ".cs", ".graphql", ".gql", ".proto", ".dart", ".swift", ".lua", ".r"], names: ["makefile", "dockerfile", "rakefile", "gemfile", "procfile", "vagrantfile", "license"] },
];

export function detectLanguage(filename: string): SourceLanguageID {
  const lower = filename.toLowerCase();
  const byName = languages.find((language) => language.names?.includes(lower));
  if (byName) return byName.id;
  const extension = lower.includes(".") ? `.${lower.split(".").pop()!}` : "";
  return languages.find((language) => language.extensions.includes(extension))?.id ?? "text";
}

export async function loadLanguageExtension(filename: string): Promise<unknown[]> {
  const id = detectLanguage(filename);
  const language = languages.find((item) => item.id === id);
  if (!language?.load) return [];
  return [await language.load()];
}

export type FilePreviewKind = "image" | "markdown" | "json" | "sqlite" | "source" | "binary" | "large";

const sqliteExtensions = new Set([".db", ".sqlite", ".sqlite3"]);
const jsonExtensions = new Set([".json", ".jsonc", ".json5"]);
const markdownExtensions = new Set([".md", ".markdown"]);

export function getPreviewKind(filename: string, isText: boolean, mimeType: string, size: number): FilePreviewKind {
  const lower = filename.toLowerCase();
  const extension = lower.includes(".") ? `.${lower.split(".").pop()!}` : "";
  if (mimeType.startsWith("image/") && mimeType !== "image/svg+xml") return "image";
  if (sqliteExtensions.has(extension)) return "sqlite";
  if (!isText) return "binary";
  if (size > 10 * 1024 * 1024) return "large";
  if (markdownExtensions.has(extension) || lower === "readme" || lower === "changelog") return "markdown";
  if (jsonExtensions.has(extension)) return "json";
  return "source";
}

export function isTextPreview(kind: FilePreviewKind): boolean {
  return kind === "markdown" || kind === "json" || kind === "source";
}
