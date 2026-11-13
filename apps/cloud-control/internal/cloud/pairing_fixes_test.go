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

// Through Nginx a rotating X-Forwarded-For is one client: the proxy appended hop
// is the same on every request, so the quota still bites.
func TestRateLimitMiddlewareIgnoresRotatedForwardedFor(t *testing.T) {
	server := &Server{config: Config{}}
	handler := server.limit(newRateLimiter(1, 60), "probe")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(spoofed string) int {
		request := httptest.NewRequest(http.MethodPost, "/v1/pairings/claim", nil)
		request.RemoteAddr = "127.0.0.1:9999"
		request.Header.Set("X-Forwarded-For", spoofed+", 203.0.113.9")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := send("198.51.100.1"); code != http.StatusNoContent {
		t.Fatalf("first request status = %d", code)
	}
	if code := send("198.51.100.2"); code != http.StatusTooManyRequests {
		t.Fatalf("rotated header status = %d, want %d", code, http.StatusTooManyRequests)
	}
}

// 端到端复现验收脚本第 2 条：请求先过一层与 Nginx 配置同语义的代理，客户端每次换一个
// 自带的 X-Forwarded-For。修复前每换一次就是一个新桶、永远不 429；修复后只有代理追加的
// 那一跳参与计数，配额照常生效，桶表里只有一个条目。
//
// 两种代理都对：现行配置同时写 X-Real-IP 和追加 XFF（第一种）；只追加 XFF 的配置
// （第二种）也必须只认最后一跳 —— 单测这一点是为了让"最后一跳"这条路径不会因为
// X-Real-IP 的存在而失去覆盖。
func TestRateLimitThroughProxyCountsRotatedForwardedForAsOneClient(t *testing.T) {
	for _, test := range []struct {
		name       string
		setsRealIP bool
	}{
		{name: "x-real-ip and forwarded-for", setsRealIP: true},
		{name: "forwarded-for tail only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{config: Config{}}
			limiter := newRateLimiter(3, 60)
			upstream := httptest.NewServer(server.limit(limiter, "pairing-claim")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			defer upstream.Close()

			const realClient = "203.0.113.9"
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.URL+r.URL.Path, nil)
				if err != nil {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				request.Header = r.Header.Clone()
				if test.setsRealIP {
					request.Header.Set("X-Real-IP", realClient)
				}
				request.Header.Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For")+", "+realClient)
				response, err := upstream.Client().Do(request)
				if err != nil {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				defer response.Body.Close()
				w.WriteHeader(response.StatusCode)
			}))
			defer proxy.Close()

			allowed := 0
			for i := 0; i < 10; i++ {
				request, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/pairings/claim", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
				response, err := proxy.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				switch response.StatusCode {
				case http.StatusNoContent:
					allowed++
				case http.StatusTooManyRequests:
				default:
					t.Fatalf("unexpected status %d", response.StatusCode)
				}
			}
			if allowed != 3 {
				t.Fatalf("allowed %d requests, want the 3-token quota", allowed)
			}
			limiter.mu.Lock()
			buckets := len(limiter.buckets)
			limiter.mu.Unlock()
			if buckets != 1 {
				t.Fatalf("bucket table holds %d entries for one client", buckets)
			}
		})
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

func TestClientIPPrefersProxyHeadersFromTrustedProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:9999"
	request.Header.Set("X-Real-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("client = %q, want the address Nginx recorded", got)
	}
}

// The client controls everything it sends; only the hop the proxy appended is
// trustworthy. Rotating the leading value must not produce a new bucket.
func TestClientIPSpoofedForwardedForRotatingYieldsOneKey(t *testing.T) {
	keys := map[string]bool{}
	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "127.0.0.1:9999"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d, 203.0.113.9", i))
		keys[clientIP(request)] = true
	}
	if len(keys) != 1 || !keys["203.0.113.9"] {
		t.Fatalf("rotating X-Forwarded-For produced keys %v", keys)
	}
}

// A request that does not come from the proxy keeps its own address: headers it
// wrote itself (including X-Real-IP) must not be able to reset its bucket.
func TestClientIPIgnoresHeadersFromUntrustedPeer(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:4444"
	request.Header.Set("X-Real-IP", "203.0.113.9")
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(request); got != "192.0.2.10" {
		t.Fatalf("client = %q, want the peer address", got)
	}
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:9999"
	if got := clientIP(request); got != "127.0.0.1" {
		t.Fatalf("direct client = %q", got)
	}
	// Garbage in the trusted header is not allowed to become a bucket key.
	request.Header.Set("X-Real-IP", "not-an-ip")
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(request); got != "127.0.0.1" {
		t.Fatalf("client = %q, want the peer address", got)
	}
	// IPv6 spellings of one host must canonicalise to one bucket key.
	request.Header.Del("X-Real-IP")
	request.Header.Del("X-Forwarded-For")
	request.RemoteAddr = "[::1]:9999"
	if got := clientIP(request); got != "::1" {
		t.Fatalf("ipv6 client = %q", got)
	}
}

// The bucket table has a hard ceiling; without one, a caller able to rotate keys
// grows process memory without limit.
func TestRateLimiterBoundsItsBucketTable(t *testing.T) {
	limiter := newRateLimiter(1, 60)
	now := time.Now().UTC()
	for i := 0; i < rateLimiterMaxBuckets*3; i++ {
		if !limiter.allow(fmt.Sprintf("client-%d", i), now) {
			t.Fatalf("a fresh client should be allowed")
		}
		if len(limiter.buckets) > rateLimiterMaxBuckets {
			t.Fatalf("bucket table grew to %d", len(limiter.buckets))
		}
	}
	// Stale entries are reclaimed before live ones, so a quiet table drains.
	later := now.Add(rateLimiterBucketTTL + time.Minute)
	limiter.allow("client-after-idle", later)
	if len(limiter.buckets) > rateLimiterMaxBuckets {
		t.Fatalf("bucket table grew to %d", len(limiter.buckets))
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

// 平台是**闭集**，不是自由文本。它会被渲染到电脑端屏幕上，也会被存进库里，
// 所以"手机报什么就存什么"是不行的 —— 这一条测试守的就是那道收口。
// 空串是**正常结果**（旧版手机端压根不上报这个字段），不是错误。
func TestSanitizePlatformKeepsAClosedSet(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{"android", "android"},
		{"Android", "android"},
		{"  IOS  ", "ios"},
		{"web", "web"},
		// 不上报 / 不认识的一律塌成空串：电脑端据此整行不渲染，而不是显示一个空格子。
		{"", ""},
		{"   ", ""},
		{"windows", ""},
		{"android; drop table cloud_access_tokens", ""},
		{"<script>", ""},
	} {
		if got := sanitizePlatform(test.raw); got != test.want {
			t.Fatalf("sanitizePlatform(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}
