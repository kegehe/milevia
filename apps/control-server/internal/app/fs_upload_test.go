package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSUploadBinaryFiles(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`,
		root, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	router := server.routes()
	write := func(path, content, encoding string, createOnly bool) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{"path": path, "content": content, "encoding": encoding, "createOnly": createOnly})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/project/fs/write", bytes.NewReader(body)))
		return response
	}
	for name, content := range map[string][]byte{
		"图片.png":     {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0xff, 0xfe},
		"report.pdf": []byte("%PDF-1.7\n\x00\xff\xfe"),
		"all-bytes.bin": func() []byte {
			data := make([]byte, 256)
			for i := range data {
				data[i] = byte(i)
			}
			return data
		}(),
		"empty.bin": {},
	} {
		t.Run(name, func(t *testing.T) {
			path := "assets/" + name
			response := write(path, base64.StdEncoding.EncodeToString(content), "base64", true)
			if response.Code != http.StatusOK {
				t.Fatalf("upload: %d %s", response.Code, response.Body.String())
			}
			var result map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["version"] != contentVersion(content) {
				t.Fatalf("version does not match original bytes: %s, %v", response.Body.String(), err)
			}
			if got, err := os.ReadFile(filepath.Join(root, path)); err != nil || !bytes.Equal(got, content) {
				t.Fatalf("uploaded bytes differ: %x, %v", got, err)
			}
			response = write(path, base64.StdEncoding.EncodeToString([]byte("replacement")), "base64", true)
			if response.Code != http.StatusConflict {
				t.Fatalf("duplicate upload: %d %s", response.Code, response.Body.String())
			}
			if got, err := os.ReadFile(filepath.Join(root, path)); err != nil || !bytes.Equal(got, content) {
				t.Fatalf("duplicate upload changed original: %x, %v", got, err)
			}
		})
	}
	t.Run("occupied workspace rejects upload", func(t *testing.T) {
		release, ok := server.acquireProjectWorkspace("project", "conversation:active")
		if !ok {
			t.Fatal("acquire AI lease")
		}
		defer release()
		response := write("locked.bin", "AP8=", "base64", true)
		if response.Code != http.StatusConflict {
			t.Fatalf("upload while occupied: %d %s", response.Code, response.Body.String())
		}
		if _, err := os.Stat(filepath.Join(root, "locked.bin")); !os.IsNotExist(err) {
			t.Fatalf("upload while occupied created a file: %v", err)
		}
	})

	t.Run("text calls remain compatible", func(t *testing.T) {
		response := write("notes.txt", "中文\nhello", "", true)
		if response.Code != http.StatusOK {
			t.Fatalf("text write: %d %s", response.Code, response.Body.String())
		}
		if got, err := os.ReadFile(filepath.Join(root, "notes.txt")); err != nil || string(got) != "中文\nhello" {
			t.Fatalf("text changed: %q, %v", got, err)
		}
	})
	for _, tc := range []struct{ name, content, encoding string }{
		{"invalid.bin", "not base64!", "base64"},
		{"unknown.bin", "hello", "hex"},
		{"large.bin", base64.StdEncoding.EncodeToString(make([]byte, maxFileSize+1)), "base64"},
		{"../escape.bin", "AA==", "base64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := write(tc.name, tc.content, tc.encoding, true)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid upload: %d %s", response.Code, response.Body.String())
			}
			if _, err := os.Stat(filepath.Join(root, tc.name)); !os.IsNotExist(err) {
				t.Fatalf("invalid upload created a file: %v", err)
			}
		})
	}
	t.Run("decoded size limit includes boundary", func(t *testing.T) {
		data := bytes.Repeat([]byte{0xff}, maxFileSize)
		response := write("limit.bin", base64.StdEncoding.EncodeToString(data), "base64", true)
		if response.Code != http.StatusOK {
			t.Fatalf("10 MiB upload: %d %s", response.Code, response.Body.String())
		}
		if got, err := os.ReadFile(filepath.Join(root, "limit.bin")); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("10 MiB upload differs, size=%d err=%v", len(got), err)
		}
	})
}

func TestRelayFSBinaryUploadUsesConversationWorkspace(t *testing.T) {
	server, projectRoot := newFSRelayTestServer(t)
	workspaceRoot := t.TempDir()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,is_current,created_at) values ('upload-conversation','file-project','upload-session','idle','approval_required',1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversation_workspaces (id,conversation_id,generation,mode,path,state,created_at) values ('upload-workspace','upload-conversation',1,'isolated_worktree',?,'ready',?)`, workspaceRoot, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update conversations set active_workspace_id='upload-workspace' where id='upload-conversation'`); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 0xff, 0x80, 0xfe}, (256<<10)/4)
	body := map[string]any{
		"op": "fs.write", "projectId": "file-project", "conversationId": "upload-conversation",
		"params": map[string]any{"path": "素材.bin", "content": base64.StdEncoding.EncodeToString(data), "encoding": "base64", "createOnly": true},
	}
	if encoded, err := json.Marshal(body); err != nil || len(encoded) >= relayFrameBudget {
		t.Fatalf("upload exceeds relay frame: size=%d err=%v", len(encoded), err)
	}
	result := relayRPCData(t, server, body)
	if result["version"] != contentVersion(data) {
		t.Fatalf("relay returned wrong version: %v", result)
	}
	if got, err := os.ReadFile(filepath.Join(workspaceRoot, "素材.bin")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("relay upload differs: size=%d err=%v", len(got), err)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "素材.bin")); !os.IsNotExist(err) {
		t.Fatalf("upload wrote to shared project root: %v", err)
	}
	status, duplicate := relayRPC(t, server, body)
	if status != http.StatusOK || duplicate["ok"] != false || duplicate["status"] != float64(http.StatusConflict) {
		t.Fatalf("relay did not preserve createOnly: transport=%d result=%v", status, duplicate)
	}
}
