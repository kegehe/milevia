package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSnapshotFingerprintIgnoresObservationTimestamp(t *testing.T) {
	first := snapshotFingerprint([]byte(`{"snapshotRevision":1,"observedAt":"2026-01-01T00:00:00Z","projects":[]}`))
	second := snapshotFingerprint([]byte(`{"snapshotRevision":1,"observedAt":"2026-01-01T00:00:01Z","projects":[]}`))
	if first == "" || first != second {
		t.Fatalf("fingerprints differ for timestamp-only change: %q vs %q", first, second)
	}
}

func TestSnapshotFingerprintChangesWhenContentChanges(t *testing.T) {
	first := snapshotFingerprint([]byte(`{"snapshotRevision":1,"observedAt":"2026-01-01T00:00:00Z","projects":[]}`))
	second := snapshotFingerprint([]byte(`{"snapshotRevision":2,"observedAt":"2026-01-01T00:00:00Z","projects":[]}`))
	if first == "" || first == second {
		t.Fatalf("fingerprints did not change with snapshot content: %q", first)
	}
}

func TestSnapshotRevisionFallsBackToSnapshotPayload(t *testing.T) {
	if got := snapshotRevision([]byte(`{"snapshotRevision":42,"projects":[]}`)); got != 42 {
		t.Fatalf("revision=%d, want 42", got)
	}
	if got := snapshotRevision([]byte(`not-json`)); got != 0 {
		t.Fatalf("invalid revision=%d, want 0", got)
	}
}

func TestSnapshotOverviewVersionAvoidsIdleRebuilds(t *testing.T) {
	var snapshotReads, uploads int
	revision := int64(4)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/remote/overview":
			_ = json.NewEncoder(w).Encode(map[string]int64{"lastAgentSequence": 2, "snapshotRevision": revision})
		case "/api/remote/snapshot":
			snapshotReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshotRevision": revision, "projects": []any{}})
		default:
			t.Fatalf("unexpected local path %q", r.URL.Path)
		}
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/snapshot" {
			t.Fatalf("unexpected cloud path %q", r.URL.Path)
		}
		uploads++
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()
	agent := New(Config{CloudURL: cloud.URL, InstanceID: "instance", CloudToken: "token", LocalURL: local.URL})
	if err := agent.syncSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.syncSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshotReads != 1 || uploads != 1 {
		t.Fatalf("idle sync reads=%d uploads=%d, want 1 each", snapshotReads, uploads)
	}
}

func TestSnapshotLegacyOverviewFallsBackToEventSequence(t *testing.T) {
	var snapshotReads, uploads int
	sequence := int64(4)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/remote/overview":
			_ = json.NewEncoder(w).Encode(map[string]int64{"lastAgentSequence": sequence})
		case "/api/remote/snapshot":
			snapshotReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshotRevision": sequence, "projects": []any{}})
		default:
			t.Fatalf("unexpected local path %q", r.URL.Path)
		}
	}))
	defer local.Close()
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()
	agent := New(Config{CloudURL: cloud.URL, InstanceID: "instance", CloudToken: "token", LocalURL: local.URL})
	if err := agent.syncSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.syncSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshotReads != 1 || uploads != 1 {
		t.Fatalf("legacy idle sync reads=%d uploads=%d, want 1 each", snapshotReads, uploads)
	}
}

func TestCloudWebSocketEndpointConvertsHTTPS(t *testing.T) {
	got, err := cloudWebSocketEndpoint("https://cloud.example.com/base", "instance 1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://cloud.example.com/base/v1/agent/connect?instanceId=instance+1" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestCloudWebSocketEndpointRejectsUnsupportedScheme(t *testing.T) {
	if _, err := cloudWebSocketEndpoint("ftp://cloud.example.com", "instance"); err == nil {
		t.Fatal("unsupported scheme was accepted")
	}
}

func TestCloudHTTPURLConvertsWSS(t *testing.T) {
	got, err := cloudHTTPURL("wss://cloud.example.com/base/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cloud.example.com/base" {
		t.Fatalf("HTTP URL = %q", got)
	}
}

func TestSendMessageStopsWhenConnectionContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writeCh := make(chan any)
	if sendMessage(ctx, writeCh, map[string]string{"kind": "test"}) {
		t.Fatal("sendMessage returned true for a cancelled context")
	}
}

func TestSendMessageDeliversBufferedMessage(t *testing.T) {
	ctx := context.Background()
	writeCh := make(chan any, 1)
	want := "event"
	if !sendMessage(ctx, writeCh, want) {
		t.Fatal("sendMessage returned false for an available channel")
	}
	if got := <-writeCh; got != want {
		t.Fatalf("message = %#v, want %#v", got, want)
	}
}

func TestRunRejectsIncompleteConfiguration(t *testing.T) {
	if err := New(Config{}).Run(context.Background()); err == nil {
		t.Fatal("Run accepted an incomplete configuration")
	}
}

func TestPublishCredentialsUsesHTTPCloudURL(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/remote/credentials" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("X-Milevia-Agent-Token"); got != "local-token" {
			t.Fatalf("local token=%q", got)
		}
		var received map[string]string
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		if received["cloudUrl"] != "https://cloud.example.com/base" || received["instanceId"] != "instance-1" || received["agentToken"] != "cloud-token" {
			t.Fatalf("credentials=%v", received)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	agent := New(Config{
		CloudURL:   "wss://cloud.example.com/base",
		InstanceID: "instance-1",
		CloudToken: "cloud-token",
		LocalURL:   local.URL,
		LocalToken: "local-token",
	})
	if err := agent.publishCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
}
