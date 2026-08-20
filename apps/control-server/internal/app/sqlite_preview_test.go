package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func addFilesystemTestProject(t *testing.T, server *Server, root string) {
	t.Helper()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('files','files',?,'wsl-local','main',1,?)`, root, time.Now().UTC()); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

func TestFSDownloadStreamsBinaryFile(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	want := []byte{0, 1, 2, 255}
	if err := os.WriteFile(filepath.Join(root, "archive.bin"), want, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	addFilesystemTestProject(t, server, root)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/projects/files/fs/download?path=archive.bin", nil)
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, response = %s", response.Code, response.Body.String())
	}
	if got := response.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("download = %v, want %v", got, want)
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), "archive.bin") {
		t.Fatalf("missing attachment header: %q", response.Header().Get("Content-Disposition"))
	}
}

func TestFSSQLitePreviewListsAndPagesRows(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "sample.sqlite3")
	db, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(`create table people (id integer primary key, name text, payload blob); insert into people(name,payload) values ('Ada',x'0102'),('Lin',x'03')`); err != nil {
		_ = db.Close()
		t.Fatalf("create fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	addFilesystemTestProject(t, server, root)

	tablesResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(tablesResponse, httptest.NewRequest(http.MethodGet, "/api/projects/files/fs/sqlite/tables?path=sample.sqlite3", nil))
	if tablesResponse.Code != http.StatusOK || !strings.Contains(tablesResponse.Body.String(), `"people"`) {
		t.Fatalf("tables = %d %s", tablesResponse.Code, tablesResponse.Body.String())
	}

	rowsResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(rowsResponse, httptest.NewRequest(http.MethodGet, "/api/projects/files/fs/sqlite/rows?path=sample.sqlite3&table=people&limit=1", nil))
	if rowsResponse.Code != http.StatusOK {
		t.Fatalf("rows = %d %s", rowsResponse.Code, rowsResponse.Body.String())
	}
	var payload struct {
		Rows    [][]sqlitePreviewCell `json:"rows"`
		HasMore bool                  `json:"hasMore"`
	}
	if err := json.Unmarshal(rowsResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	if len(payload.Rows) != 1 || !payload.HasMore || payload.Rows[0][1].Value != "Ada" || payload.Rows[0][2].Kind != "blob" {
		t.Fatalf("unexpected rows: %#v", payload)
	}
}

func TestFSSQLitePreviewRejectsNonDatabase(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-database.db"), []byte("not sqlite"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	addFilesystemTestProject(t, server, root)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/files/fs/sqlite/tables?path=not-a-database.db", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "不是 SQLite") || !strings.Contains(response.Body.String(), `"code":"sqlite_not_database"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}
