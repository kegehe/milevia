package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxSQLitePreviewSize = 64 * 1024 * 1024
	maxSQLiteRows        = 100
	maxSQLiteCellRunes   = 4096
	maxSQLiteBlobBytes   = 1024
)

var errNotSQLiteDatabase = errors.New("不是 SQLite 数据库文件")

type sqlitePreviewObject struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type sqlitePreviewColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	NotNull    bool   `json:"notNull"`
	Default    string `json:"default"`
	PrimaryKey bool   `json:"primaryKey"`
}

type sqlitePreviewCell struct {
	Kind      string `json:"kind"`
	Value     any    `json:"value,omitempty"`
	Length    int    `json:"length,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *Server) fsSQLiteTables(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	db, closeDB, err := s.openSQLitePreview(r)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	defer closeDB()
	objects, err := sqlitePreviewObjects(ctx, db)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": objects})
}

func (s *Server) fsSQLiteSchema(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	table := r.URL.Query().Get("table")
	if table == "" {
		writeError(w, http.StatusBadRequest, errors.New("table 参数必填"))
		return
	}
	db, closeDB, err := s.openSQLitePreview(r)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	defer closeDB()
	object, err := sqlitePreviewObjectByName(ctx, db, table)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	var schemaSQL string
	if err := db.QueryRowContext(ctx, `select coalesce(sql,'') from sqlite_schema where name=? and type=?`, object.Name, object.Type).Scan(&schemaSQL); err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	columns, err := sqlitePreviewColumns(ctx, db, object.Name)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": object, "sql": schemaSQL, "columns": columns})
}

func (s *Server) fsSQLiteRows(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	table := r.URL.Query().Get("table")
	if table == "" {
		writeError(w, http.StatusBadRequest, errors.New("table 参数必填"))
		return
	}
	limit, offset, err := sqlitePreviewPagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	db, closeDB, err := s.openSQLitePreview(r)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	defer closeDB()
	object, err := sqlitePreviewObjectByName(ctx, db, table)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	columns, rows, hasMore, err := sqlitePreviewRows(ctx, db, object.Name, limit, offset)
	if err != nil {
		s.writeSQLitePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  object,
		"columns": columns,
		"rows":    rows,
		"limit":   limit,
		"offset":  offset,
		"hasMore": hasMore,
	})
}

func (s *Server) openSQLitePreview(r *http.Request) (*sql.DB, func(), error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		return nil, nil, errors.New("path 参数必填")
	}
	fs, err := s.getFilesystem(r)
	if err != nil {
		return nil, nil, err
	}
	localFS, ok := fs.(*LocalFilesystem)
	if !ok {
		return nil, nil, errors.New("暂不支持远程数据库预览")
	}
	absPath, err := validateSQLitePreviewPath(localFS, path)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	db, err := sql.Open("sqlite3", sqlitePreviewDSN(absPath))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("打开 SQLite 数据库失败：%w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		cancel()
		return nil, nil, fmt.Errorf("打开 SQLite 数据库失败：%w", err)
	}
	if _, err := db.ExecContext(ctx, "pragma query_only=on"); err != nil {
		db.Close()
		cancel()
		return nil, nil, fmt.Errorf("配置 SQLite 只读模式失败：%w", err)
	}
	return db, func() {
		_ = db.Close()
		cancel()
	}, nil
}

func (s *Server) writeSQLitePreviewError(w http.ResponseWriter, err error) {
	if offline, ok := err.(*runnerOfflineError); ok {
		s.writeFSError(w, offline)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func validateSQLitePreviewPath(fs *LocalFilesystem, path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".db" && ext != ".sqlite" && ext != ".sqlite3" {
		return "", errNotSQLiteDatabase
	}
	absPath, err := fs.resolvePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("路径是目录，不是文件")
	}
	if info.Size() > maxSQLitePreviewSize {
		return "", fmt.Errorf("数据库文件过大（%d 字节），最大支持 64MB", info.Size())
	}
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, len("SQLite format 3\x00"))
	if _, err := io.ReadFull(f, header); err != nil || !bytes.Equal(header, []byte("SQLite format 3\x00")) {
		return "", errNotSQLiteDatabase
	}
	return absPath, nil
}

func sqlitePreviewDSN(path string) string {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String()
}

func sqlitePreviewObjects(ctx context.Context, db *sql.DB) ([]sqlitePreviewObject, error) {
	rows, err := db.QueryContext(ctx, `select name,type from sqlite_schema where type in ('table','view') and name not like 'sqlite_%' order by type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make([]sqlitePreviewObject, 0)
	for rows.Next() {
		var object sqlitePreviewObject
		if err := rows.Scan(&object.Name, &object.Type); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func sqlitePreviewObjectByName(ctx context.Context, db *sql.DB, name string) (sqlitePreviewObject, error) {
	var object sqlitePreviewObject
	err := db.QueryRowContext(ctx, `select name,type from sqlite_schema where name=? and type in ('table','view') and name not like 'sqlite_%'`, name).Scan(&object.Name, &object.Type)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlitePreviewObject{}, errors.New("数据库对象不存在")
	}
	return object, err
}

func sqlitePreviewColumns(ctx context.Context, db *sql.DB, name string) ([]sqlitePreviewColumn, error) {
	rows, err := db.QueryContext(ctx, `select name,coalesce(type,''),"notnull",coalesce(dflt_value,''),pk from pragma_table_info(?) order by cid`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]sqlitePreviewColumn, 0)
	for rows.Next() {
		var column sqlitePreviewColumn
		var notNull, primaryKey int
		if err := rows.Scan(&column.Name, &column.Type, &notNull, &column.Default, &primaryKey); err != nil {
			return nil, err
		}
		column.NotNull = notNull != 0
		column.PrimaryKey = primaryKey != 0
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func sqlitePreviewRows(ctx context.Context, db *sql.DB, name string, limit, offset int) ([]string, [][]sqlitePreviewCell, bool, error) {
	query := "select * from " + quoteSQLiteIdentifier(name) + " limit ? offset ?"
	rows, err := db.QueryContext(ctx, query, limit+1, offset)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	result := make([][]sqlitePreviewCell, 0, limit)
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, nil, false, err
		}
		if len(result) == limit {
			return columns, result, true, nil
		}
		cells := make([]sqlitePreviewCell, len(values))
		for i, value := range values {
			cells[i] = sqlitePreviewValue(value)
		}
		result = append(result, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return columns, result, false, nil
}

func sqlitePreviewPagination(r *http.Request) (int, int, error) {
	limit := maxSQLiteRows
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxSQLiteRows {
			return 0, 0, fmt.Errorf("limit 必须在 1 到 %d 之间", maxSQLiteRows)
		}
		limit = parsed
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return 0, 0, errors.New("offset 必须是不小于 0 的整数")
		}
		offset = parsed
	}
	return limit, offset, nil
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func sqlitePreviewValue(value any) sqlitePreviewCell {
	switch typed := value.(type) {
	case nil:
		return sqlitePreviewCell{Kind: "null"}
	case []byte:
		preview := typed
		truncated := len(preview) > maxSQLiteBlobBytes
		if truncated {
			preview = preview[:maxSQLiteBlobBytes]
		}
		return sqlitePreviewCell{Kind: "blob", Value: base64.StdEncoding.EncodeToString(preview), Length: len(typed), Truncated: truncated}
	case string:
		preview, truncated := truncateSQLiteText(typed)
		return sqlitePreviewCell{Kind: "text", Value: preview, Length: len(typed), Truncated: truncated}
	case int64:
		return sqlitePreviewCell{Kind: "number", Value: typed}
	case float64:
		return sqlitePreviewCell{Kind: "number", Value: typed}
	case bool:
		return sqlitePreviewCell{Kind: "boolean", Value: typed}
	default:
		return sqlitePreviewCell{Kind: "text", Value: fmt.Sprint(value)}
	}
}

func truncateSQLiteText(value string) (string, bool) {
	runes := []rune(value)
	if len(runes) <= maxSQLiteCellRunes {
		return value, false
	}
	return string(runes[:maxSQLiteCellRunes]), true
}
