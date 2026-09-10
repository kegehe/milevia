package cloud

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A conflicting event is permanent: retrying or reconnecting can never make it
// succeed, so the relay must distinguish it from a transient database failure.
func TestIsEventConflictMatchesOnlyPermanentConflicts(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"sequence conflict", fmt.Errorf("%w for instance pc-1: sequence 5", errEventSequenceConflict), true},
		{"event id conflict", fmt.Errorf("%w for instance pc-1: evt-1", errEventIDConflict), true},
		{"insert conflict", fmt.Errorf("%w for instance pc-1", errEventInsertConflict), true},
		{"database outage", errors.New("connection refused"), false},
		{"no error", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isEventConflict(test.err); got != test.want {
				t.Fatalf("isEventConflict(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestRateLimiterAllowsCapacityThenRefills(t *testing.T) {
	limiter := newRateLimiter(2, 60)
	now := time.Now().UTC()
	if !limiter.allow("client-a", now) || !limiter.allow("client-a", now) {
		t.Fatal("requests within capacity should be allowed")
	}
	if limiter.allow("client-a", now) {
		t.Fatal("a request beyond capacity should be rejected")
	}
	if !limiter.allow("client-b", now) {
		t.Fatal("limits must be tracked per client")
	}
	if !limiter.allow("client-a", now.Add(2*time.Second)) {
		t.Fatal("tokens should refill over time")
	}
}

func TestRateLimitMiddlewareReturns429(t *testing.T) {
	server := &Server{config: Config{}}
	handler := server.limit(newRateLimiter(1, 60), "probe")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/pairings/claim", nil))
	if first.Code != http.StatusNoContent {
		t.Fatalf("first request status = %d, want %d", first.Code, http.StatusNoContent)
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/pairings/claim", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("burst status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
}

// A nil limiter must not panic: several tests build a Server directly.
func TestRateLimitMiddlewareToleratesMissingLimiter(t *testing.T) {
	server := &Server{config: Config{}}
	handler := server.limit(nil, "probe")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/pairings/claim", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestClientIPPrefersForwardedAddress(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:9999"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("forwarded client = %q", got)
	}
	request.Header.Del("X-Forwarded-For")
	if got := clientIP(request); got != "127.0.0.1" {
		t.Fatalf("direct client = %q", got)
	}
}

func TestAgentOriginAllowedKeepsNonBrowserClientsWorking(t *testing.T) {
	server := &Server{config: Config{AppURL: "https://keyanjia.info:8443"}}
	if !server.agentOriginAllowed("") {
		t.Fatal("the Agent sends no Origin and must stay acceptable")
	}
	if !server.agentOriginAllowed("https://keyanjia.info:8443") {
		t.Fatal("the configured deployment origin should be accepted")
	}
	if server.agentOriginAllowed("https://evil.example") {
		t.Fatal("a foreign origin must be rejected")
	}
}
