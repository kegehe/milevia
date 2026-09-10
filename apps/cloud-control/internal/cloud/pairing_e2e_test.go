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
