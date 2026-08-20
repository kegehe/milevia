import { useEffect, useRef, useState } from "react";

type SQLiteObject = { name: string; type: "table" | "view" };
type SQLiteColumn = { name: string; type: string; notNull: boolean; default: string; primaryKey: boolean };
type SQLiteCell = { kind: string; value?: string | number | boolean; length?: number; truncated?: boolean };
type SQLiteRows = { columns: string[]; rows: SQLiteCell[][]; offset: number; limit: number; hasMore: boolean };

interface SqliteViewerProps {
  projectId: string;
  path: string;
  request: <T>(path: string, init?: RequestInit) => Promise<T>;
  onNotDatabase: () => void;
}

type RequestError = Error & { code?: string };

export function SqliteViewer({ projectId, path, request, onNotDatabase }: SqliteViewerProps) {
  const [objects, setObjects] = useState<SQLiteObject[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [columns, setColumns] = useState<SQLiteColumn[]>([]);
  const [rows, setRows] = useState<SQLiteRows | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const rowsRequestVersion = useRef(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setObjects([]);
    setSelected("");
    void request<{ tables: SQLiteObject[] }>(`/api/projects/${projectId}/fs/sqlite/tables?path=${encodeURIComponent(path)}`)
      .then((response) => {
        if (cancelled) return;
        const next = response.tables ?? [];
        setObjects(next);
        setSelected(next[0]?.name ?? "");
      })
      .catch((reason: unknown) => {
        if (cancelled) return;
        if ((reason as RequestError).code === "sqlite_not_database") {
          onNotDatabase();
          return;
        }
        setError(reason instanceof Error ? reason.message : "无法读取数据库");
      })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [onNotDatabase, path, projectId, request]);

  useEffect(() => {
    rowsRequestVersion.current++;
    if (!selected) {
      setColumns([]);
      setRows(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    const base = `/api/projects/${projectId}/fs/sqlite`;
    const query = `path=${encodeURIComponent(path)}&table=${encodeURIComponent(selected)}`;
    void Promise.all([
      request<{ columns: SQLiteColumn[] }>(`${base}/schema?${query}`),
      request<SQLiteRows>(`${base}/rows?${query}&limit=100&offset=0`),
    ]).then(([schema, page]) => {
      if (cancelled) return;
      setColumns(schema.columns ?? []);
      setRows(page);
    }).catch((reason) => { if (!cancelled) setError(reason instanceof Error ? reason.message : "无法读取数据表"); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [path, projectId, request, selected]);

  const changePage = (offset: number) => {
    if (!selected || offset < 0) return;
    const requestVersion = ++rowsRequestVersion.current;
    setLoading(true);
    setError(null);
    const query = `path=${encodeURIComponent(path)}&table=${encodeURIComponent(selected)}&limit=100&offset=${offset}`;
    void request<SQLiteRows>(`/api/projects/${projectId}/fs/sqlite/rows?${query}`)
      .then((page) => { if (requestVersion === rowsRequestVersion.current) setRows(page); })
      .catch((reason) => { if (requestVersion === rowsRequestVersion.current) setError(reason instanceof Error ? reason.message : "无法读取数据表"); })
      .finally(() => { if (requestVersion === rowsRequestVersion.current) setLoading(false); });
  };

  if (loading && objects.length === 0) return <div className="file-editor-loading"><div className="file-editor-spinner" /><span>加载数据库...</span></div>;
  if (error) return <div className="file-preview-message error">数据库预览失败：{error}</div>;
  if (objects.length === 0) return <div className="file-preview-message">数据库中没有可显示的表或视图。</div>;

  return (
    <div className="sqlite-viewer">
      <aside className="sqlite-objects" aria-label="数据库对象">
        {objects.map((object) => <button type="button" key={`${object.type}:${object.name}`} className={object.name === selected ? "active" : ""} onClick={() => setSelected(object.name)}><span>{object.type === "view" ? "视图" : "表"}</span>{object.name}</button>)}
      </aside>
      <section className="sqlite-data">
        <div className="sqlite-columns">{columns.map((column) => <span key={column.name} title={column.type || "未声明类型"}>{column.name}{column.primaryKey ? " PK" : ""}</span>)}</div>
        {rows && <div className="sqlite-table-wrap"><table><thead><tr>{rows.columns.map((column) => <th key={column}>{column}</th>)}</tr></thead><tbody>{rows.rows.map((row, rowIndex) => <tr key={rowIndex}>{row.map((cell, index) => <td key={index} title={cell.truncated ? `内容已截断，原始长度 ${cell.length}` : undefined}>{formatCell(cell)}</td>)}</tr>)}</tbody></table></div>}
        {rows && <div className="sqlite-pagination"><button type="button" onClick={() => changePage(rows.offset - rows.limit)} disabled={loading || rows.offset === 0}>上一页</button><span>第 {Math.floor(rows.offset / rows.limit) + 1} 页</span><button type="button" onClick={() => changePage(rows.offset + rows.limit)} disabled={loading || !rows.hasMore}>下一页</button></div>}
      </section>
    </div>
  );
}

function formatCell(cell: SQLiteCell): string {
  if (cell.kind === "null") return "NULL";
  if (cell.kind === "blob") return `BLOB (${cell.length ?? 0} bytes)`;
  return String(cell.value ?? "");
}
