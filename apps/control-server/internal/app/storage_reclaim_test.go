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

// 已经是 INCREMENTAL 模式时走增量分支（收不干净才退回整库 VACUUM，见
// needsFullVacuum），两种路径都必须幂等可重复调用。
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

// 真机那一版库的判据回归：auto_vacuum 已经是 INCREMENTAL、freelist 仍压着 2 GB
// （631,934 页里 518,071 页空闲），而 incremental_vacuum 0.0 秒只放掉 1 页。这种情况
// 必须选整库 VACUUM，否则文件会永远停在 2.6 GB、每次启动都"回收成功"却一点没缩小。
func TestNeedsFullVacuumMatchesObservedBloat(t *testing.T) {
	const pageSize = int64(4096)
	for _, testCase := range []struct {
		name      string
		force     bool
		pageCount int64
		freelist  int64
		want      bool
	}{
		{name: "observed 2.6GB db", pageCount: 631934, freelist: 518071, want: true},
		{name: "few free pages", pageCount: 631934, freelist: 1000, want: false},
		{name: "force ignores ratio", force: true, pageCount: 631934, freelist: 10000, want: true},
		// force 不能比自动回收更保守：小库的高占比空洞重写成本也小，仍然要做。
		{name: "force vacuums small db with high ratio", force: true, pageCount: 10000, freelist: 3000, want: true},
		// 大库（20 GB）里的 40 MB 残留只有 0.2%：远低于按库大小放大的 1% 下限，跳过。
		{name: "force skips tiny residual in huge db", force: true, pageCount: 5242880, freelist: 10000, want: false},
		// 同一个大库里 250 MB 残留超过 1%，值得重写。
		{name: "force vacuums meaningful residual in huge db", force: true, pageCount: 5242880, freelist: 64000, want: true},
		// 增量回收只剩几 MB：不值得为它重写整库（真机 2.6 GB 库，~3 MB 残留）。
		{name: "force skips negligible residual", force: true, pageCount: 631934, freelist: 1000, want: false},
		{name: "no free pages", pageCount: 631934, freelist: 0, want: false},
		{name: "unknown page count", pageCount: 0, freelist: 10, want: false},
	} {
		if got := needsFullVacuum(testCase.force, pageSize, testCase.pageCount, testCase.freelist); got != testCase.want {
			t.Fatalf("%s: needsFullVacuum(force=%v, pageSize=%d, pageCount=%d, freelist=%d) = %v, want %v",
				testCase.name, testCase.force, pageSize, testCase.pageCount, testCase.freelist, got, testCase.want)
		}
	}
}

// 切到 INCREMENTAL 之后又攒出一大块空洞：这次必须真的收回去。以前这里无论收没收到都
// 直接返回成功，于是"回收"变成了空转 —— 上面那条真机读数就是这么来的。
func TestEnsureStorageReclaimedShrinksIncrementalFreelist(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 20000)
	if err := server.ensureStorageReclaimed(context.Background()); err != nil {
		t.Fatalf("first reclaim: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 2 {
		t.Fatalf("auto_vacuum = %d after first reclaim, want 2", mode)
	}
	buildFreelist(t, server, 20000)
	before := readFreelist(t, server)
	if before <= 0 {
		t.Fatalf("scratch did not leave any freelist pages: %d", before)
	}
	if err := server.ensureStorageReclaimed(context.Background()); err != nil {
		t.Fatalf("second reclaim: %v", err)
	}
	if after := readFreelist(t, server); after >= before {
		t.Fatalf("freelist = %d after second reclaim, want < %d (回收必须真的发生)", after, before)
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

// 用户显式点「清理」时不受占比阈值限制：可回收空间超过绝对下限就必须真的回收。
func TestReclaimFreePagesForceIgnoresThreshold(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 20000)

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

// fillLivePages 往库里写一块**保留**的数据，把 page_count 撑大（不制造 freelist），
// 用来模拟"大库里的少量空洞"。
func fillLivePages(t *testing.T, server *Server, rows int) {
	t.Helper()
	if _, err := server.db.Exec(`create table if not exists reclaim_keep (id integer primary key, blob text)`); err != nil {
		t.Fatalf("create keep table: %v", err)
	}
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`insert into reclaim_keep (blob) values (?)`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	blob := strings.Repeat("x", 2000)
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(blob); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert keep row %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// 点「清理」也不能为几 MB 残留重写一个几 GB 的库：占用比与绝对量都很小时跳过。
func TestReclaimFreePagesForceSkipsNegligibleResidual(t *testing.T) {
	server := newTestServer(t)
	fillLivePages(t, server, 20000) // ~40 MB 真实数据
	buildFreelist(t, server, 4000)  // ~7 MB 空洞：占比低、绝对值也低于 32 MB

	server.storageMu.Lock()
	err := server.reclaimFreePagesLocked(context.Background(), true)
	server.storageMu.Unlock()
	if err != nil {
		t.Fatalf("reclaimFreePagesLocked: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 0 {
		t.Fatalf("auto_vacuum = %d, want 0 (residual is too small to justify a full rewrite)", mode)
	}
}

// force 必须比自动回收更积极：小库的高占比空洞重写成本小，仍然要做。
func TestReclaimFreePagesForceVacuumsSmallDbWithHighRatio(t *testing.T) {
	server := newTestServer(t)
	buildFreelist(t, server, 4000)

	server.storageMu.Lock()
	err := server.reclaimFreePagesLocked(context.Background(), true)
	server.storageMu.Unlock()
	if err != nil {
		t.Fatalf("reclaimFreePagesLocked: %v", err)
	}
	if mode := readAutoVacuum(t, server); mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (force must reclaim a high-ratio residual)", mode)
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
