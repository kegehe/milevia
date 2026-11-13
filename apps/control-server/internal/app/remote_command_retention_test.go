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

	"github.com/go-chi/chi/v5"
)

func insertRetentionCommand(t *testing.T, db *sql.DB, commandID, idempotencyKey, status, payload string, updatedAt, expiresAt time.Time) {
	t.Helper()
	// updated_at 始终是服务端自己写的 UTC；expires_at 故意不强制 UTC —— 旧版本按云端
	// 传来的时区原样入库，用例要能复现那种行。
	if _, err := db.Exec(`insert into processed_remote_commands(command_id,idempotency_key,type,status,result,received_at,updated_at,expires_at,request_payload,request_hash) values(?,?,?,?,?,?,?,?,?,?)`,
		commandID, idempotencyKey, "task.dispatch", status, "{}", updatedAt.UTC(), updatedAt.UTC(), expiresAt, payload, "hash-"+commandID); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCommandRetentionScrubsTerminalPayloadsOnly(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	terminal := []string{"completed", "failed", "expired", "cancelled", "indeterminate"}
	for _, status := range terminal {
		insertRetentionCommand(t, db, "old-"+status, "idem-old-"+status, status, `{"text":"`+strings.Repeat("x", 4096)+`"}`, now.Add(-2*time.Hour), expired)
	}
	insertRetentionCommand(t, db, "recent-completed", "idem-recent-completed", "completed", `{"text":"recent"}`, now.Add(-10*time.Minute), expired)
	// 非终态行是执行队列与幂等窗口本身，正文必须原样保留。
	insertRetentionCommand(t, db, "old-queued", "idem-old-queued", "queued", `{"text":"still waiting"}`, now.Add(-10*24*time.Hour), expired)
	insertRetentionCommand(t, db, "old-received", "idem-old-received", "received", `{"text":"in flight"}`, now.Add(-10*24*time.Hour), now.Add(time.Minute))
	insertRetentionCommand(t, db, "old-executing", "idem-old-executing", "executing", `{"text":"in flight"}`, now.Add(-10*24*time.Hour), now.Add(time.Minute))

	scrubbed, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if scrubbed != int64(len(terminal)) || removed != 0 {
		t.Fatalf("scrubbed=%d removed=%d", scrubbed, removed)
	}
	for _, status := range terminal {
		var payload string
		if err := db.QueryRow(`select request_payload from processed_remote_commands where command_id=?`, "old-"+status).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if payload != "{}" {
			t.Fatalf("terminal %s payload=%q", status, payload)
		}
	}
	for commandID, want := range map[string]string{
		"recent-completed": `{"text":"recent"}`,
		"old-queued":       `{"text":"still waiting"}`,
		"old-received":     `{"text":"in flight"}`,
		"old-executing":    `{"text":"in flight"}`,
	} {
		var payload string
		if err := db.QueryRow(`select request_payload from processed_remote_commands where command_id=?`, commandID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if payload != want {
			t.Fatalf("%s payload=%q want %q", commandID, payload, want)
		}
	}
}

func TestRemoteCommandRetentionDeletesOnlyPastWindowTerminalRows(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	insertRetentionCommand(t, db, "old-completed", "idem-old-completed", "completed", `{"text":"gone"}`, now.Add(-8*24*time.Hour), expired)
	insertRetentionCommand(t, db, "recent-completed", "idem-recent-completed", "completed", `{"text":"kept"}`, now.Add(-2*time.Hour), expired)
	// 非终态行不受保留期影响：它是执行队列与幂等窗口本身。
	insertRetentionCommand(t, db, "old-queued", "idem-old-queued", "queued", `{"text":"still waiting"}`, now.Add(-30*24*time.Hour), now.Add(time.Minute))

	_, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d", removed)
	}
	for commandID, want := range map[string]bool{"old-completed": false, "recent-completed": true, "old-queued": true} {
		var count int
		if err := db.QueryRow(`select count(*) from processed_remote_commands where command_id=?`, commandID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (count == 1) != want {
			t.Fatalf("%s exists=%v want %v", commandID, count == 1, want)
		}
	}
}

func TestRemoteCommandRetentionCapsTerminalRows(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	for index := 0; index < 5; index++ {
		insertRetentionCommand(t, db, "cmd-"+string(rune('a'+index)), "idem-"+string(rune('a'+index)), "completed", `{"text":"cap"}`, now.Add(-time.Duration(index+1)*time.Minute), expired)
	}

	_, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed=%d", removed)
	}
	rows, err := db.Query(`select command_id from processed_remote_commands order by updated_at desc`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var commandID string
		if err := rows.Scan(&commandID); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, commandID)
	}
	if strings.Join(kept, ",") != "cmd-a,cmd-b" {
		t.Fatalf("kept=%v", kept)
	}
}

// expires_at 在旧版本里按云端传来的时区偏移原样入库；比较必须按真实时刻，不能按文本。
// -05:00 的 04:xx（实际 09:xx UTC）在文本上"更早"，会把尚未到期的行提前删掉。
func TestRemoteCommandRetentionComparesExpiresAtByInstant(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	zone := time.FixedZone("cloud", -5*60*60)
	// 5 行终态，上限只留 2 行；被上限选中的 3 行 expiresAt = now+5m（以 -05:00 存储）。
	for index := 0; index < 5; index++ {
		expires := now.Add(-time.Minute)
		if index >= 2 {
			expires = now.Add(5 * time.Minute).In(zone)
		}
		insertRetentionCommand(t, db, fmt.Sprintf("cmd-%d", index), fmt.Sprintf("idem-%d", index), "completed", `{}`, now.Add(-time.Duration(index+1)*time.Minute), expires)
	}
	// 钉住夹具：入库文本必须真的带负偏移，否则这条用例就只是重复了 UTC 的正常路径。
	var stored string
	if err := db.QueryRow(`select cast(expires_at as text) from processed_remote_commands where command_id='cmd-4'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stored, "-05:00") {
		t.Fatalf("夹具没有复现非 UTC 偏移：expires_at=%q", stored)
	}
	_, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed=%d，负时区偏移下未到期的行被提前删掉了", removed)
	}
}

// 分块路径：超过一块的行必须多轮处理，循环要能正常收敛（终止条件写反就是死循环）。
func TestRemoteCommandRetentionProcessesMultipleChunks(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	total := remoteCommandPruneChunk + 1
	// 第一轮：全部超过保留期，验证删除分块。
	for index := 0; index < total; index++ {
		insertRetentionCommand(t, db, fmt.Sprintf("old-%04d", index), fmt.Sprintf("idem-old-%04d", index), "completed", `{}`, now.Add(-8*24*time.Hour), expired)
	}
	scrubbed, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != int64(total) || scrubbed != 0 {
		t.Fatalf("removed=%d scrubbed=%d want removed=%d", removed, scrubbed, total)
	}
	// 第二轮：全部在保留期内但过了宽限期，验证清正文分块。
	for index := 0; index < total; index++ {
		insertRetentionCommand(t, db, fmt.Sprintf("keep-%04d", index), fmt.Sprintf("idem-keep-%04d", index), "completed", `{"text":"payload"}`, now.Add(-2*time.Hour), expired)
	}
	scrubbed, removed, err = s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if scrubbed != int64(total) || removed != 0 {
		t.Fatalf("scrubbed=%d removed=%d want scrubbed=%d", scrubbed, removed, total)
	}
}

// 云端在 Agent 重连时会重投 received/executing 的命令；只要 expiresAt 还没到，
// 本地幂等记录就不能删，否则重投会被当成新命令再执行一次。条数上限是唯一可能在
// 15 分钟内删行的路径，必须尊重 expiresAt。
func TestRemoteCommandRetentionKeepsRowsCloudCanStillRedeliver(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// 5 行终态，上限只留 2 行；被上限选中的最老 3 行仍可能被云端重投（expiresAt 未到）。
	for index := 0; index < 5; index++ {
		expires := now.Add(-time.Minute)
		if index >= 2 {
			expires = now.Add(5 * time.Minute)
		}
		insertRetentionCommand(t, db, "cmd-"+string(rune('a'+index)), "idem-"+string(rune('a'+index)), "completed", `{}`, now.Add(-time.Duration(index+1)*time.Minute), expires)
	}
	_, removed, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed=%d，未过期行不该被条数上限删掉", removed)
	}
	// 16 分钟后全部过期，冗余行才被清理。
	_, removed, err = s.pruneRemoteCommandsWithLimits(context.Background(), now.Add(16*time.Minute), time.Hour, 7*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed=%d", removed)
	}
}

// 新入库的 expires_at 必须统一成 UTC，避免再产生依赖时区偏移的文本。
func TestRemoteCommandEnqueueStoresExpiresAtUTC(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(5 * time.Minute).In(time.FixedZone("cloud", -5*60*60)).Format(time.RFC3339)
	body := `{"type":"task.dispatch","taskId":"t1","idempotencyKey":"utc-expiry","payload":{},"expiresAt":"` + expiresAt + `"}`
	response := httptest.NewRecorder()
	s.enqueueRemoteCommand(response, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var accepted struct {
		CommandID string `json:"commandId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.CommandID == "" {
		t.Fatalf("decode response: %v %s", err, response.Body.String())
	}
	var stored string
	if err := db.QueryRow(`select cast(expires_at as text) from processed_remote_commands where command_id=?`, accepted.CommandID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stored, "+00:00") {
		t.Fatalf("expires_at 没有归一为 UTC：%q", stored)
	}
}

// 清正文之后，保留期内的重复请求仍必须命中幂等，且状态查询照常可用。
func TestRemoteCommandRetentionKeepsIdempotencyAndStatusQuery(t *testing.T) {
	db := newRemoteTestDB(t)
	defer db.Close()
	s := &Server{db: db}
	if err := s.migrateRemoteControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"task.dispatch","taskId":"t1","idempotencyKey":"retry-window","payload":{"text":"` + strings.Repeat("y", 2048) + `"}}`
	first := httptest.NewRecorder()
	s.enqueueRemoteCommand(first, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(body)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var accepted struct {
		CommandID string `json:"commandId"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil || accepted.CommandID == "" {
		t.Fatalf("decode first response: %v %s", err, first.Body.String())
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`update processed_remote_commands set status='completed',result='{"ok":true}',updated_at=? where command_id=?`, now.Add(-2*time.Hour), accepted.CommandID); err != nil {
		t.Fatal(err)
	}
	if scrubbed, _, err := s.pruneRemoteCommandsWithLimits(context.Background(), now, time.Hour, 7*24*time.Hour, 0); err != nil || scrubbed != 1 {
		t.Fatalf("scrubbed=%d err=%v", scrubbed, err)
	}
	// 只清正文：终态回执 result 必须还在，Agent 重启后云端重投时要靠它回传终态。
	var result string
	if err := db.QueryRow(`select result from processed_remote_commands where command_id=?`, accepted.CommandID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("result=%q，清正文不应动终态回执", result)
	}

	second := httptest.NewRecorder()
	s.enqueueRemoteCommand(second, httptest.NewRequest(http.MethodPost, "/api/remote/commands", strings.NewReader(body)))
	if second.Code != http.StatusAccepted {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	var repeated struct {
		CommandID  string `json:"commandId"`
		Status     string `json:"status"`
		Idempotent bool   `json:"idempotent"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if !repeated.Idempotent || repeated.CommandID != accepted.CommandID || repeated.Status != "completed" {
		t.Fatalf("repeated=%+v accepted=%s", repeated, accepted.CommandID)
	}

	router := chi.NewRouter()
	router.Get("/api/remote/commands/{commandID}", s.getRemoteCommand)
	statusResponse := httptest.NewRecorder()
	router.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/remote/commands/"+accepted.CommandID, nil))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status query=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var queried struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &queried); err != nil || queried.Status != "completed" {
		t.Fatalf("status query decode: %v %s", err, statusResponse.Body.String())
	}
}
