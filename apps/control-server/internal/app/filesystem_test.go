package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalFilesystemWriteFilePreservesExistingMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatalf("create script: %v", err)
	}
	fs := &LocalFilesystem{projectPath: root}
	if err := fs.WriteFile(context.Background(), "script.sh", []byte("#!/bin/sh\necho new\n"), "", false); err != nil {
		t.Fatalf("write script: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("script mode = %04o, want 0755", got)
	}
}

func TestLocalFilesystemReadFileReturnsContentVersion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("create file: %v", err)
	}
	fs := &LocalFilesystem{projectPath: root}
	first, err := fs.ReadFile(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("read first version: %v", err)
	}
	if first.Version == "" {
		t.Fatal("first version is empty")
	}
	if err := fs.WriteFile(context.Background(), "notes.txt", []byte("second"), "", false); err != nil {
		t.Fatalf("write second version: %v", err)
	}
	second, err := fs.ReadFile(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("read second version: %v", err)
	}
	if first.Version == second.Version {
		t.Fatalf("version did not change: %q", first.Version)
	}
}

func TestLocalFilesystemWriteFileRejectsChangedExpectedVersion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("create file: %v", err)
	}
	fs := &LocalFilesystem{projectPath: root}
	initial, err := fs.ReadFile(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("read initial version: %v", err)
	}
	if err := os.WriteFile(path, []byte("external update"), 0o644); err != nil {
		t.Fatalf("modify file externally: %v", err)
	}
	if err := fs.WriteFile(context.Background(), "notes.txt", []byte("stale replacement"), initial.Version, false); !errors.Is(err, errFileVersionConflict) {
		t.Fatalf("write error = %v, want version conflict", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "external update" {
		t.Fatalf("file content = %q, want external update", got)
	}
}

func TestLocalFilesystemCreateOnlyDoesNotOverwriteExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("existing content"), 0o644); err != nil {
		t.Fatalf("create file: %v", err)
	}
	fs := &LocalFilesystem{projectPath: root}
	if err := fs.WriteFile(context.Background(), "notes.txt", []byte(""), "", true); !errors.Is(err, errFileAlreadyExists) {
		t.Fatalf("write error = %v, want file already exists", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "existing content" {
		t.Fatalf("file content = %q, want existing content", got)
	}
}

func TestLocalFilesystemRenameRejectsExistingDestination(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.txt")
	newPath := filepath.Join(root, "new.txt")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatalf("create destination: %v", err)
	}

	fs := &LocalFilesystem{projectPath: root}
	if err := fs.Rename(context.Background(), "old.txt", "new.txt"); !errors.Is(err, errFileAlreadyExists) {
		t.Fatalf("rename error = %v, want destination-exists conflict", err)
	}
	for path, want := range map[string]string{oldPath: "old", newPath: "new"} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != want {
			t.Fatalf("%s content = %q, err=%v; want %q", path, content, err, want)
		}
	}
}

func TestLocalFilesystemRemoveRemovesSymlinkNotTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("create target file: %v", err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	fs := &LocalFilesystem{projectPath: root}
	if err := fs.Remove(context.Background(), "link"); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink still exists or could not be inspected: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(target, "keep.txt"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("target was modified: content=%q err=%v", content, err)
	}
}

func TestLocalFilesystemRenameMovesSymlinkNotTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	fs := &LocalFilesystem{projectPath: root}
	if err := fs.Rename(context.Background(), "link", "renamed-link"); err != nil {
		t.Fatalf("rename symlink: %v", err)
	}
	info, err := os.Lstat(filepath.Join(root, "renamed-link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("renamed path is not the original symlink: info=%v err=%v", info, err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep" {
		t.Fatalf("target was modified: content=%q err=%v", content, err)
	}
}

func TestLocalFilesystemWriteFileRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	fs := &LocalFilesystem{projectPath: root}
	if err := fs.WriteFile(context.Background(), "link.txt", []byte("replacement"), "", false); err == nil {
		t.Fatal("write through symlink succeeded")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "keep" {
		t.Fatalf("target was modified: content=%q err=%v", content, err)
	}
}

func TestFSRenameReturnsConflictForExistingDestination(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if _, err := server.db.Exec(
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`,
		root,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("create project: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/projects/project/fs/rename", bytes.NewBufferString(`{"oldPath":"old.txt","newPath":"new.txt"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; response = %s", response.Code, http.StatusConflict, response.Body.String())
	}
	for path, want := range map[string]string{"old.txt": "old", "new.txt": "new"} {
		content, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(content) != want {
			t.Fatalf("%s content = %q, err=%v; want %q", path, content, err, want)
		}
	}
}

func TestFSWriteFileRejectsStaleContentVersion(t *testing.T) {
	server := newTestServer(t)
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("current content"), 0o644); err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := server.db.Exec(
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`,
		root,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("create project: %v", err)
	}

	body, err := json.Marshal(map[string]string{
		"path":            "notes.txt",
		"content":         "stale replacement",
		"expectedVersion": contentVersion([]byte("older content")),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/projects/project/fs/write", bytes.NewReader(body))
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; response = %s", response.Code, http.StatusConflict, response.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "current content" {
		t.Fatalf("file content = %q, want current content", got)
	}
}
