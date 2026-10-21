package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// buildFreelist 造出一大块可以被回收的空洞：写满再删光，页面进 freelist 但不归还文件系统。
// 这就是真实库里 6.9 GB free pages 的成因（会话级联删除 / 历史裁剪删掉的都是这种量级）。
func buildFreelist(t *testing.T, server *Server, rows int) {
	t.Helper()
	if _, err := server.db.Exec(`create table if not exists reclaim_scratch (id integer primary key, blob text)`); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`insert into reclaim_scratch (blob) values (?)`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	blob := strings.Repeat("x", 2000)
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(blob); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert scratch %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := server.db.Exec(`delete from reclaim_scratch`); err != nil {
		t.Fatalf("delete scratch: %v", err)
	}
}

func readAutoVacuum(t *testing.T, server *Server) int64 {
	t.Helper()
	var mode int64
	if err := server.db.QueryRow(`pragma auto_vacuum`).Scan(&mode); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	return mode
}

func readFreelist(t *testing.T, server *Server) int64 {
	t.Helper()
	var pages int64
	if err := server.db.QueryRow(`pragma freelist_count`).Scan(&pages); err != nil {
		t.Fatalf("read freelist_count: %v", err)
	}
	return pages
}

// freelist 占比超阈值时，一次性回收要既把空洞还回去，又把库切到 INCREMENTAL 模式，
// 让以后删除产生的空洞能逐步归还而不是再攒成几个 GB。
func TestEnsureStorageReclaimedSwitchesToIncremental(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 20000)

	before := readFreelist(t, server)
	if before <= 0 {
		t.Fatalf("scratch did not leave any freelist pages: %d", before)
	}
	if mode := readAutoVacuum(t, server); mode != 0 {
		t.Fatalf("auto_vacuum = %d before reclaim, want 0", mode)
	}

	if err := server.ensureStorageReclaimed(context.Background()); err != nil {
		t.Fatalf("ensureStorageReclaimed: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 2 {
		t.Fatalf("auto_vacuum = %d after reclaim, want 2 (INCREMENTAL)", mode)
	}
	if after := readFreelist(t, server); after >= before {
		t.Fatalf("freelist = %d after reclaim, want < %d before", after, before)
	}
}

// 已经是 INCREMENTAL 模式时走增量分支：不重写整库，仍然幂等可重复调用。
func TestEnsureStorageReclaimedIncrementalBranchIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 20000)
	if err := server.ensureStorageReclaimed(context.Background()); err != nil {
		t.Fatalf("first reclaim: %v", err)
	}
	buildFreelist(t, server, 2000)
	for i := 0; i < 3; i++ {
		if err := server.ensureStorageReclaimed(context.Background()); err != nil {
			t.Fatalf("incremental reclaim #%d: %v", i, err)
		}
	}
	if mode := readAutoVacuum(t, server); mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2", mode)
	}
}

// 没什么可回收时不做无谓的重写：auto_vacuum 保持 0，免得为一个小库白白 VACUUM 一遍。
func TestEnsureStorageReclaimedSkipsWhenNothingToReclaim(t *testing.T) {
	server := newTestServer(t)
	if err := server.ensureStorageReclaimed(context.Background()); err != nil {
		t.Fatalf("ensureStorageReclaimed: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 0 {
		t.Fatalf("auto_vacuum = %d, want 0 (nothing worth rewriting)", mode)
	}
}

// 用户显式点「清理」时不受阈值限制：force 路径必须真的回收。
func TestReclaimFreePagesForceIgnoresThreshold(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 4000)

	server.storageMu.Lock()
	err := server.reclaimFreePagesLocked(context.Background(), true)
	server.storageMu.Unlock()
	if err != nil {
		t.Fatalf("reclaimFreePagesLocked: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (force must reclaim)", mode)
	}
}

// 支出统计接口的行为：返回各表行数与采样估算的负载，且标记数字是否为估算值。
func TestStorageUsageEndpointReturnsSampledEstimates(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-storage"
	runID := seedConversation(t, server, "project-storage", conversationID)
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 5, time.Now().UTC())

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/system/storage", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var usage storageUsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !usage.MeasurementDone {
		t.Fatal("small database should be measured completely")
	}
	if usage.Reclaiming {
		t.Fatal("reclaiming should be false when no background reclaim is running")
	}
	var events *storageTableUsage
	for index := range usage.Tables {
		if usage.Tables[index].Name == "events" {
			events = &usage.Tables[index]
		}
	}
	if events == nil {
		t.Fatalf("events table missing from usage: %#v", usage.Tables)
	}
	if events.Rows != 5 {
		t.Fatalf("events rows = %d, want 5", events.Rows)
	}
	if !events.PayloadEstimated {
		t.Fatal("payload bytes must be flagged as an estimate")
	}
}

// 清理遗留遥测必须分批：真机库里有 42 万行 thinking_tokens 和 215 万行 stream_event，
// 一条无界 DELETE 会变成独占唯一 SQLite 连接几十分钟的巨型事务。
//
// 两类一起覆盖是有意的：stream_event 是"交互式流式会话漏了 guard"留下的整类分片，与
// thinking_tokens 同属"写入端已不再持久化、全仓没有读取方"的历史数据。清理范围必须把
// 两者都算上 —— 保留循环够不到它们（分片常年压在每会话 5 万条过程事件的上限之内），
// 漏掉任何一类，它就永远占着文件。
func TestDeleteLegacyTelemetryEventsBatchesAndSparesContent(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-thinking"
	runID := seedConversation(t, server, "project-thinking", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	// 留在库里的都是历史遗留数据：写入端早已不再持久化它们，所以这里直接写库。
	seedEvents(t, server, conversationID, runID, "system", "think-", 25, base)
	if _, err := server.db.Exec(`update events set payload='{"type":"system","subtype":"thinking_tokens"}' where id like 'think-%'`); err != nil {
		t.Fatalf("mark thinking_tokens payloads: %v", err)
	}
	seedEvents(t, server, conversationID, runID, "stream_event", "stream-", 25, base.Add(30*time.Second))
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 7, base.Add(time.Minute))

	// 批大小 10 < 50 行，必须跨多批删完。
	removed, err := server.deleteLegacyTelemetryEventsBatched(context.Background(), 10)
	if err != nil {
		t.Fatalf("deleteLegacyTelemetryEventsBatched: %v", err)
	}
	if removed != 50 {
		t.Fatalf("removed = %d, want 50", removed)
	}
	if got := countEventsOfType(t, server, conversationID, "system"); got != 0 {
		t.Fatalf("system events left = %d, want 0", got)
	}
	if got := countEventsOfType(t, server, conversationID, "stream_event"); got != 0 {
		t.Fatalf("stream_event left = %d, want 0", got)
	}
	if got := countEventsOfType(t, server, conversationID, "assistant"); got != 7 {
		t.Fatalf("assistant events = %d, want 7 (content must survive)", got)
	}
	// 没有可删的时候不该空转。
	removed, err = server.deleteLegacyTelemetryEventsBatched(context.Background(), 10)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if removed != 0 {
		t.Fatalf("second pass removed = %d, want 0", removed)
	}
}

// 清理前的精确统计要与删除范围同源：统计口径比删除范围窄的话，界面会报"删了 0 条"而
// 文件却变小了；反过来则会报出一批根本没删掉的条数。
func TestLegacyTelemetryStatsMatchTheDeleteScope(t *testing.T) {
	server := newTestServer(t)
	const conversationID = "conversation-telemetry-stats"
	runID := seedConversation(t, server, "project-telemetry-stats", conversationID)
	base := time.Now().UTC().Add(-time.Hour)

	seedEvents(t, server, conversationID, runID, "system", "think-", 3, base)
	const thinkingPayload = `{"type":"system","subtype":"thinking_tokens"}`
	if _, err := server.db.Exec(`update events set payload=? where id like 'think-%'`, thinkingPayload); err != nil {
		t.Fatalf("mark thinking_tokens payloads: %v", err)
	}
	seedEvents(t, server, conversationID, runID, "stream_event", "stream-", 4, base.Add(time.Minute))
	seedEvents(t, server, conversationID, runID, "assistant", "assistant-", 5, base.Add(2*time.Minute))

	count, bytes, err := server.legacyTelemetryStatsBatched(context.Background(), 2)
	if err != nil {
		t.Fatalf("legacyTelemetryStatsBatched: %v", err)
	}
	if count != 7 {
		t.Fatalf("counted events = %d, want 7", count)
	}
	// seedEvents 写的 payload 是 "{}"（2 字节）；3 条被改成了 thinking_tokens 的长 payload。
	wantBytes := int64(3*len(thinkingPayload) + 4*len("{}"))
	if bytes != wantBytes {
		t.Fatalf("counted payload bytes = %d, want %d", bytes, wantBytes)
	}
}
