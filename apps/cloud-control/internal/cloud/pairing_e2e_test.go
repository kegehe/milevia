package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// This exercises the full pairing handshake against a real PostgreSQL instance,
// which is the only way to cover the activated_at gate in userAuth and the
// desktop-confirmation transaction. It is skipped unless a test database is
// configured, so a workstation without PostgreSQL stays green:
//
//	MILEVIA_CLOUD_TEST_DATABASE_URL=postgres://user:pw@127.0.0.1:5432/milevia_test?sslmode=disable go test ./...
//
// The DSN must name a test database; the guard below refuses anything else so a
// production connection string can never be pointed at this test.
// openTestServer connects to the configured test database, or skips the test
// when none is set up. The DSN must name a test database so a production
// connection string can never be pointed at these tests.
func openTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MILEVIA_CLOUD_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set MILEVIA_CLOUD_TEST_DATABASE_URL to run the pairing end-to-end test")
	}
	if !strings.Contains(strings.ToLower(dsn), "test") {
		t.Skip("refusing to run against a database whose name does not contain \"test\"")
	}
	server, err := New(context.Background(), Config{
		DatabaseURL:     dsn,
		EnrollmentToken: "enroll-token",
		UserToken:       "user-token",
		AppURL:          "https://app.example.com",
	})
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(server.Close)
	return server, server.Handler()
}

// A command aimed at an offline computer must be refused outright. Accepting it
// would queue the command, expire it minutes later, and leave the user
// believing the task had been dispatched.
func TestCommandRejectedWhileInstanceOfflineEndToEnd(t *testing.T) {
	server, handler := openTestServer(t)

	instanceID, _ := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()
	// registerTestInstance leaves the instance offline: no Agent has connected.

	request := httptest.NewRequest(http.MethodPost, "/v1/instances/"+instanceID+"/commands", strings.NewReader(`{"type":"task.dispatch","taskId":"task-1"}`))
	request.Header.Set("Authorization", "Bearer user-token")
	request.Header.Set("Idempotency-Key", "offline-probe")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("offline command status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "instance_offline") {
		t.Fatalf("offline rejection should name the reason, got %s", recorder.Body.String())
	}
}

func TestPairingRequiresDesktopConfirmationEndToEnd(t *testing.T) {
	server, handler := openTestServer(t)

	instanceID, agentToken := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()

	pairingID, code := createTestPairing(t, handler, instanceID, agentToken)
	accessToken := claimTestPairing(t, handler, pairingID, code)

	// The token handed back by the claim must be inert until the desktop
	// confirms: this is the authorization boundary the original code lacked.
	if status := getInstancesStatus(t, handler, accessToken); status != http.StatusUnauthorized {
		t.Fatalf("claimed-but-unconfirmed token status = %d, want %d", status, http.StatusUnauthorized)
	}

	confirmTestPairing(t, handler, instanceID, agentToken, pairingID)

	if status := getInstancesStatus(t, handler, accessToken); status != http.StatusOK {
		t.Fatalf("confirmed token status = %d, want %d", status, http.StatusOK)
	}

	// Revoking the phone must not touch the machine's own credential.
	if status := postJSON(t, handler, "/v1/instances/"+instanceID+"/revoke", accessToken, map[string]string{"scope": "mobile"}).Code; status != http.StatusOK {
		t.Fatalf("revoke status = %d, want %d", status, http.StatusOK)
	}
	if status := getInstancesStatus(t, handler, accessToken); status != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want %d", status, http.StatusUnauthorized)
	}
	// Revoking the phone must leave the machine's own credential intact.
	agentRequest := httptest.NewRequest(http.MethodPost, "/v1/agent/events", strings.NewReader("[]"))
	agentRequest.Header.Set("X-Milevia-Agent-Token", agentToken)
	agentRequest.Header.Set("X-Milevia-Instance-ID", instanceID)
	agentRecorder := httptest.NewRecorder()
	handler.ServeHTTP(agentRecorder, agentRequest)
	if agentRecorder.Code == http.StatusUnauthorized {
		t.Fatal("revoking the phone also revoked the machine credential")
	}
}

func registerTestInstance(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/agent/register", strings.NewReader(`{"name":"e2e"}`))
	request.Header.Set("X-Milevia-Enrollment-Token", "enroll-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		InstanceID string `json:"instanceId"`
		AgentToken string `json:"agentToken"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.InstanceID == "" || payload.AgentToken == "" {
		t.Fatalf("register response incomplete: %s", recorder.Body.String())
	}
	return payload.InstanceID, payload.AgentToken
}

func createTestPairing(t *testing.T, handler http.Handler, instanceID, agentToken string) (string, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/agent/pairings", nil)
	request.Header.Set("X-Milevia-Agent-Token", agentToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create pairing status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingID string `json:"pairingId"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PairingID == "" || len(payload.Code) != 6 {
		t.Fatalf("pairing response incomplete: %s", recorder.Body.String())
	}
	return payload.PairingID, payload.Code
}

func claimTestPairing(t *testing.T, handler http.Handler, pairingID, code string) string {
	t.Helper()
	body := strings.NewReader(`{"code":"` + code + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/pairings/"+pairingID+"/claim", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" {
		t.Fatalf("claim response carried no token: %s", recorder.Body.String())
	}
	return payload.AccessToken
}

func confirmTestPairing(t *testing.T, handler http.Handler, instanceID, agentToken, pairingID string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/agent/pairings/"+pairingID+"/confirm", nil)
	request.Header.Set("X-Milevia-Agent-Token", agentToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("confirm status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func getInstancesStatus(t *testing.T, handler http.Handler, token string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/instances", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

func postJSON(t *testing.T, handler http.Handler, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// claimTestPairingAs 与 claimTestPairing 的唯一差别是带上手机名（新版手机端会上报）。
func claimTestPairingAs(t *testing.T, handler http.Handler, pairingID, code, deviceName string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"code": code, "deviceName": deviceName})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/pairings/"+pairingID+"/claim", strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" {
		t.Fatalf("claim response carried no token: %s", recorder.Body.String())
	}
	return payload.AccessToken
}

// testBinding 故意把 lastUsedAt 收成 json.RawMessage：**「键不存在」和「值是 null」
// 必须能被分开断言**。用 *string 反序列化时两者都是 nil，测试会自证通过，
// 而前端正是靠这两种状态说两句完全不同的话（见 apps/web 的 desktop-phone.ts）。
type testBinding struct {
	DeviceName string          `json:"deviceName"`
	Platform   string          `json:"platform"`
	LastUsedAt json.RawMessage `json:"lastUsedAt"`
}

func getAgentBindings(t *testing.T, handler http.Handler, instanceID, agentToken string) []testBinding {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/agent/bindings", nil)
	request.Header.Set("X-Milevia-Agent-Token", agentToken)
	request.Header.Set("X-Milevia-Instance-ID", instanceID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bindings status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Bindings []testBinding `json:"bindings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Bindings
}

// claimTestPairingAsPlatform 与 claimTestPairingAs 的差别是同时上报平台（新版手机端）。
func claimTestPairingAsPlatform(t *testing.T, handler http.Handler, pairingID, code, deviceName, platform string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"code": code, "deviceName": deviceName, "platform": platform})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/pairings/"+pairingID+"/claim", strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" {
		t.Fatalf("claim response carried no token: %s", recorder.Body.String())
	}
	return payload.AccessToken
}

// 一台电脑同一时间只服务一台手机：确认新手机的那一刻把旧手机顶掉。
// 这条链路上三件事必须一起成立 —— 旧令牌失效、新令牌可用、电脑端能读到"现在是谁在用"；
// 少任何一件，用户看到的都是"我明明确认了，但两台手机都还能进"或者"我看不出占了这台机器的是谁"。
func TestPairingReplacesPreviousPhoneEndToEnd(t *testing.T) {
	server, handler := openTestServer(t)

	instanceID, agentToken := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()

	firstPairing, firstCode := createTestPairing(t, handler, instanceID, agentToken)
	firstToken := claimTestPairingAs(t, handler, firstPairing, firstCode, "Xiaomi 14")
	confirmTestPairing(t, handler, instanceID, agentToken, firstPairing)
	if status := getInstancesStatus(t, handler, firstToken); status != http.StatusOK {
		t.Fatalf("first phone should be active after confirmation, status = %d", status)
	}

	secondPairing, secondCode := createTestPairing(t, handler, instanceID, agentToken)
	secondToken := claimTestPairingAs(t, handler, secondPairing, secondCode, "iPhone 15")
	confirmTestPairing(t, handler, instanceID, agentToken, secondPairing)

	// 旧手机被顶掉：它的令牌必须立刻不可用（换手机的核心体验就在这里）。
	if status := getInstancesStatus(t, handler, firstToken); status != http.StatusUnauthorized {
		t.Fatalf("replaced phone status = %d, want %d", status, http.StatusUnauthorized)
	}
	if status := getInstancesStatus(t, handler, secondToken); status != http.StatusOK {
		t.Fatalf("new phone status = %d, want %d", status, http.StatusOK)
	}
	bindings := getAgentBindings(t, handler, instanceID, agentToken)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want exactly 1 active phone: %+v", len(bindings), bindings)
	}
	if bindings[0].DeviceName != "iPhone 15" {
		t.Fatalf("binding device name = %q, want %q", bindings[0].DeviceName, "iPhone 15")
	}
}

// 电脑端要能回答两个新问题：**这台手机是哪个平台**、**它现在还在不在用**。
// 在此之前 bindings 只回 deviceName/activatedAt/createdAt —— 桌面端于是只能答"绑过谁"，
// 用户在手机上看到"已连接"、在电脑上看到一片沉默（2026-09-18 补）。
//
// 这条测试守三段，少任何一段桌面端那一屏就是错的：
//   ① 平台闭集落库（不认识的值塌成空串，前端据此整行不渲染）
//   ② 「绑定后还没同步过」必须是**键在、值为 null**，不是缺键 ——
//      前端把缺键读成"本机云端版本不提供这一项"，两句措辞完全不同
//   ③ 手机一发请求 lastUsedAt 就落下来，且**节流**（每次轮询都写会把连接打满）
func TestPhoneActivityAndPlatformReachTheDesktop(t *testing.T) {
	server, handler := openTestServer(t)

	instanceID, agentToken := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()

	pairingID, code := createTestPairing(t, handler, instanceID, agentToken)
	token := claimTestPairingAsPlatform(t, handler, pairingID, code, "Xiaomi 14", "android")
	confirmTestPairing(t, handler, instanceID, agentToken, pairingID)

	bindings := getAgentBindings(t, handler, instanceID, agentToken)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1: %+v", len(bindings), bindings)
	}
	if bindings[0].Platform != "android" {
		t.Fatalf("binding platform = %q, want %q", bindings[0].Platform, "android")
	}
	if len(bindings[0].LastUsedAt) == 0 {
		t.Fatal("lastUsedAt 键必须存在：缺键会被桌面端读成「本机云端版本不提供这一项」")
	}
	if string(bindings[0].LastUsedAt) != "null" {
		t.Fatalf("绑定后未同步时 lastUsedAt = %s, want null", bindings[0].LastUsedAt)
	}

	// 手机发一次请求（就是它 5 秒一次的那条轮询）。
	if status := getInstancesStatus(t, handler, token); status != http.StatusOK {
		t.Fatalf("phone request status = %d, want 200", status)
	}
	afterFirst := getAgentBindings(t, handler, instanceID, agentToken)
	if len(afterFirst) != 1 || string(afterFirst[0].LastUsedAt) == "null" {
		t.Fatalf("手机发过请求后 lastUsedAt 仍未落下来: %+v", afterFirst)
	}
	firstSeen := string(afterFirst[0].LastUsedAt)

	// 节流：30 秒窗口内的第二次请求**不许**再写一次。判据是"时刻一模一样" ——
	// 只断言"值还在"会让每次轮询都写一版的实现照样绿。
	if status := getInstancesStatus(t, handler, token); status != http.StatusOK {
		t.Fatalf("second phone request status = %d, want 200", status)
	}
	afterSecond := getAgentBindings(t, handler, instanceID, agentToken)
	if len(afterSecond) != 1 {
		t.Fatalf("bindings = %d, want 1: %+v", len(afterSecond), afterSecond)
	}
	if string(afterSecond[0].LastUsedAt) != firstSeen {
		t.Fatalf("节流失效：30 秒窗口内 lastUsedAt 被重写了（%s → %s）", firstSeen, afterSecond[0].LastUsedAt)
	}
}
