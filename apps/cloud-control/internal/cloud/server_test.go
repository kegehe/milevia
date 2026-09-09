package cloud

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPairingClaimRouteIsPublic(t *testing.T) {
	s := &Server{config: Config{UserToken: "desktop-user-token", AppURL: "https://keyanjia.info:8443"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/pairings/claim", strings.NewReader("{"))
	req.Header.Set("Origin", "https://localhost")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("claim route status = %d, want %d (route must not require user auth)", res.Code, http.StatusBadRequest)
	}
}

func TestAgentAuthIsInstanceScoped(t *testing.T) {
	s := &Server{config: Config{AgentTokens: map[string]string{"pc-a": "token-a", "pc-b": "token-b"}}}
	req := httptest.NewRequest("GET", "/v1/agent/connect?instanceId=pc-a", nil)
	req.Header.Set("X-Milevia-Agent-Token", "token-a")
	if ok, err := s.agentAuth(req, "pc-a"); !ok || err != nil {
		t.Fatalf("matching instance token was rejected: ok=%v err=%v", ok, err)
	}
	if ok, err := s.agentAuth(req, "pc-b"); ok || err != nil {
		t.Fatalf("token for pc-a authenticated pc-b: ok=%v err=%v", ok, err)
	}
}

// TestAgentAuthDeniedDistinguishesDatabaseFailure ensures a DB lookup error
// (transient availability/connection issue) is not reported as a 401, which the
// Agent would interpret as credential revocation and re-enroll on. It must be a
// 503 so the Agent simply backs off without discarding its stored secret.
func TestAgentAuthDeniedDistinguishesDatabaseFailure(t *testing.T) {
	if !agentAuthDenied(httptest.NewRecorder(), false, nil) {
		t.Fatal("genuine rejection was not treated as a failure")
	}
	// The caller must produce 401 for ok=false with nil error, and 503 for a
	// DB error. Verify the dispatch by driving each through a recorder.
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid credentials -> 401", err: nil, want: http.StatusUnauthorized},
		{name: "db unavailable -> 503", err: errors.New("connection refused"), want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !agentAuthDenied(rec, false, test.err) {
				t.Fatalf("denied reported no failure for %q", test.name)
			}
			if rec.Code != test.want {
				t.Fatalf("%q status = %d, want %d", test.name, rec.Code, test.want)
			}
		})
	}
	// ok=true is not a denial and writes nothing.
	rec := httptest.NewRecorder()
	if agentAuthDenied(rec, true, errors.New("unused")) {
		t.Fatal("authenticated request was treated as denied")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request wrote a status: %d", rec.Code)
	}
}

func TestAppOriginIgnoresPath(t *testing.T) {
	if got := appOrigin("HTTPS://APP.EXAMPLE.COM:443/milevia"); got != "https://app.example.com" {
		t.Fatalf("origin = %q", got)
	}
	if got := appOrigin(""); got != "" {
		t.Fatalf("empty URL origin = %q", got)
	}
}

func TestIsAllowedWebOrigin(t *testing.T) {
	configured := "https://keyanjia.info:8443"
	for _, test := range []struct {
		origin string
		want   bool
	}{
		{configured, true},
		{"https://localhost", true},
		{"capacitor://localhost", true},
		{"https://evil.example", false},
		{"", false},
	} {
		if got := isAllowedWebOrigin(test.origin, configured); got != test.want {
			t.Fatalf("origin %q: got %v, want %v", test.origin, got, test.want)
		}
	}
}

func TestDecodeWithLimitAllowsLargeSnapshotOnly(t *testing.T) {
	payload := `{"snapshotRevision":1,"projects":[{"name":"` + strings.Repeat("x", 600*1024) + `"}]}`
	server := &Server{config: Config{AgentTokens: map[string]string{"pc-a": "token-a"}}}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent/snapshot", strings.NewReader(payload))
	request.Header.Set("X-Milevia-Instance-ID", "pc-a")
	request.Header.Set("X-Milevia-Agent-Token", "token-a")
	// The route reaches database setup after decoding; this test only verifies
	// the dedicated decoder accepts a snapshot over the normal 512 KiB limit.
	response := httptest.NewRecorder()
	var target any
	if !decodeWithLimit(response, request, &target, 8<<20) {
		t.Fatalf("large snapshot was rejected: %s", response.Body.String())
	}
	_ = server
}
