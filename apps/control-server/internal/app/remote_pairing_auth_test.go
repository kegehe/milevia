package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The desktop page holds the session token, not the Agent token, so the pairing
// endpoints have to accept it. Everything else must stay Agent-only, otherwise
// a compromised page session could forge Agent sync traffic.
func TestRemoteRelayAcceptsDesktopSessionForPairingOnly(t *testing.T) {
	server := &Server{config: Config{Mode: "desktop-api", SessionToken: "sess-1", RemoteAgentToken: "agent-1"}}

	for _, testCase := range []struct {
		name    string
		path    string
		headers map[string]string
		allowed bool
	}{
		{"session token may create a pairing", "/api/remote/pairing", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may confirm a pairing", "/api/remote/pairing/confirm", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may poll the pairing status", "/api/remote/pairing/status", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may read the agent status", "/api/remote/agent-status", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may read which phone is bound", "/api/remote/bindings", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may unbind the current phone", "/api/remote/bindings/revoke", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"a trailing slash still resolves to the pairing endpoint", "/api/remote/pairing/", map[string]string{"X-Milevia-Session": "sess-1"}, true},
		{"session token may not drain the outbox", "/api/remote/outbox", map[string]string{"X-Milevia-Session": "sess-1"}, false},
		{"session token may not submit command results", "/api/remote/commands", map[string]string{"X-Milevia-Session": "sess-1"}, false},
		{"session token may not forge credentials", "/api/remote/credentials", map[string]string{"X-Milevia-Session": "sess-1"}, false},
		// /api/remote/rpc 是这个命名空间里唯一提供文件读写的端点（见 app.go 里那段说明）。
		// 它必须**只认 Agent 令牌**：桌面页会话已经能通过 X-Milevia-Session 调本机
		// 全部 /api/projects/* 接口，再让它多一条"以 Agent 身份操作项目文件"的路，
		// 等于给一个被 XSS 拿到的页面加一条绕过 session 边界的出口。
		{"session token may not read project files", "/api/remote/rpc", map[string]string{"X-Milevia-Session": "sess-1"}, false},
		{"the agent token may use the file relay", "/api/remote/rpc", map[string]string{"X-Milevia-Agent-Token": "agent-1"}, true},
		{"a wrong session token is rejected", "/api/remote/pairing", map[string]string{"X-Milevia-Session": "nope"}, false},
		{"missing credentials are rejected", "/api/remote/pairing", nil, false},
		{"the agent token still works on relay endpoints", "/api/remote/outbox", map[string]string{"X-Milevia-Agent-Token": "agent-1"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			allowed := false
			handler := server.remoteAgentOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				allowed = true
			}))
			request := httptest.NewRequest(http.MethodPost, testCase.path, nil)
			for key, value := range testCase.headers {
				request.Header.Set(key, value)
			}
			handler.ServeHTTP(httptest.NewRecorder(), request)
			if allowed != testCase.allowed {
				t.Fatalf("allowed=%v, want %v", allowed, testCase.allowed)
			}
		})
	}
}

// Web mode issues no desktop session at all, so the pairing exception must not
// accidentally apply there.
func TestRemoteRelayRejectsDesktopSessionOutsideDesktopMode(t *testing.T) {
	server := &Server{config: Config{Mode: "web", SessionToken: "sess-1", RemoteAgentToken: "agent-1"}}
	allowed := false
	handler := server.remoteAgentOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		allowed = true
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/remote/pairing", nil)
	request.Header.Set("X-Milevia-Session", "sess-1")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if allowed {
		t.Fatal("web mode must not accept a desktop session on the relay namespace")
	}
}

func seedRemoteOutbox(t *testing.T, s *Server, db *sql.DB, taskID string, at time.Time) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, taskID, "", "task.created", map[string]string{"status": "todo"}, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := db.QueryRow(`select event_id from remote_outbox where task_id=?`, taskID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func newRemoteOutboxServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	db := newRemoteTestDB(t)
	s := &Server{db: db, config: Config{RemoteCloudURL: "https://cloud.example.com", RemoteCloudToken: "token"}}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

// A permanently rejected event must leave the queue, otherwise it keeps its
// position at the head of the ordered batch and starves everything behind it.
func TestPermanentOutboxFailureDropsTheEvent(t *testing.T) {
	s, db := newRemoteOutboxServer(t)
	defer db.Close()
	eventID := seedRemoteOutbox(t, s, db, "task-1", time.Now().UTC())

	body := fmt.Sprintf(`{"eventIds":[%q],"error":"sequence conflict","permanent":true}`, eventID)
	recorder := httptest.NewRecorder()
	s.failRemoteOutbox(recorder, httptest.NewRequest(http.MethodPost, "/api/remote/outbox/fail", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var remaining int
	if err := db.QueryRow(`select count(*) from remote_outbox`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expected the rejected event to be dropped, %d remain", remaining)
	}
}

// A non-permanent failure keeps the event but defers it.
func TestTransientOutboxFailureKeepsTheEvent(t *testing.T) {
	s, db := newRemoteOutboxServer(t)
	defer db.Close()
	eventID := seedRemoteOutbox(t, s, db, "task-1", time.Now().UTC())

	body := fmt.Sprintf(`{"eventIds":[%q],"error":"network"}`, eventID)
	recorder := httptest.NewRecorder()
	s.failRemoteOutbox(recorder, httptest.NewRequest(http.MethodPost, "/api/remote/outbox/fail", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var attempts int
	if err := db.QueryRow(`select attempts from remote_outbox where event_id=?`, eventID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}
}

// Events that exhausted their attempt budget leave the delivery window so they
// cannot block the events queued behind them.
func TestRemoteOutboxSkipsExhaustedEvents(t *testing.T) {
	s, db := newRemoteOutboxServer(t)
	defer db.Close()
	now := time.Now().UTC()
	seedRemoteOutbox(t, s, db, "task-head", now)
	if _, err := db.Exec(`update remote_outbox set attempts=?`, maxRemoteOutboxAttempts); err != nil {
		t.Fatal(err)
	}
	seedRemoteOutbox(t, s, db, "task-tail", now.Add(time.Second))

	recorder := httptest.NewRecorder()
	s.remoteOutbox(recorder, httptest.NewRequest(http.MethodGet, "/api/remote/outbox", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	var items []remoteOutboxItem
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TaskID != "task-tail" {
		t.Fatalf("expected only the tail event to be delivered, got %+v", items)
	}
}
