package app

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seedRemoteOutboxRow 直接往出队表里塞一行，绕开 recordTaskEvent 那条链路 ——
// 这里要验的是投递窗口与容量上限，不关心事件是怎么产生的。
func seedRemoteOutboxRow(t *testing.T, server *Server, eventID string, sequence int64, createdAt time.Time) {
	t.Helper()
	if _, err := server.db.Exec(
		`insert into remote_outbox (event_id,agent_sequence,type,payload,created_at) values (?,?,?,?,?)`,
		eventID, sequence, "stream_event", "{}", createdAt); err != nil {
		t.Fatalf("insert remote_outbox %s: %v", eventID, err)
	}
}

func remoteOutboxIDs(t *testing.T, server *Server) []string {
	t.Helper()
	rows, err := server.db.Query(`select event_id from remote_outbox order by agent_sequence`)
	if err != nil {
		t.Fatalf("read remote_outbox: %v", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan remote_outbox: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// 读取端**不能**带时间过滤，只能按 agent_sequence 顺序取最老的几条。
//
// 这条是实测出来的，不是风格偏好：真机库积压 138 万行时，给读取加上
// `created_at >= 窗口` 会让一次读取从 0.8ms 变成 2659ms —— 符合条件的行都在
// agent_sequence 的末尾，扫描必须走完整张表才能凑够 limit 的条数；而每次读取都
// 持着那条唯一的 SQLite 连接，等于把整个服务按在地上。合成库上对照过多种写法
// （含建 (created_at, agent_sequence) 索引、用子查询定下界），只要存在大量过期行
// 就都会退化成全表扫描。
//
// "不投递过期事件"改由清理删除保证（见下面几条）。这里钉住查询形状，防止有人
// 出于好意再把过滤条件加回来。
func TestReadRemoteOutboxQueryStaysFreeOfTimeFilters(t *testing.T) {
	raw, err := os.ReadFile("remote_control.go")
	if err != nil {
		t.Fatalf("read remote_control.go: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "func (s *Server) readRemoteOutbox(")
	if start < 0 {
		t.Fatal("找不到 readRemoteOutbox")
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("找不到 readRemoteOutbox 的函数结尾")
	}
	fn := rest[:end]
	query := regexp.MustCompile(`(?s)select event_id,agent_sequence.*?limit \?`).FindString(fn)
	if query == "" {
		t.Fatal("找不到 readRemoteOutbox 里的查询语句")
	}
	// 只看 WHERE 里的比较：created_at 出现在 select 列表里是正常的。
	if comparison := regexp.MustCompile(`created_at\s*(>=|<=|>|<|=)`).FindString(query); comparison != "" {
		t.Fatalf("读取查询的 WHERE 里又出现了时间比较（%s），会退化成全表扫描：\n%s", comparison, query)
	}
	if !strings.Contains(query, "order by agent_sequence") {
		t.Fatalf("读取查询必须按 agent_sequence 顺序取最老的几条：\n%s", query)
	}
}

// 顺序保证：先投最老的。中继的语义是"按时间补齐"，顺序错了手机端的实时流就乱了。
func TestReadRemoteOutboxReturnsOldestRowsFirst(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	seedRemoteOutboxRow(t, server, "event-old", 7, now.Add(-2*time.Hour))
	seedRemoteOutboxRow(t, server, "event-new", 9, now)
	seedRemoteOutboxRow(t, server, "event-mid", 8, now.Add(-time.Minute))

	items, err := server.readRemoteOutbox(context.Background(), 100)
	if err != nil {
		t.Fatalf("readRemoteOutbox: %v", err)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.EventID)
	}
	if len(got) != 3 || got[0] != "event-old" || got[1] != "event-mid" || got[2] != "event-new" {
		t.Fatalf("readRemoteOutbox returned %v, want oldest-first", got)
	}
}

// 过期行要真的被删掉，而不是永远留在表里被每次读取跳过 —— 否则 111 万行的规模
// 还是照样拖慢每一条查询。
func TestPruneRemoteOutboxDropsExpiredRows(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	seedRemoteOutboxRow(t, server, "event-stale", 1, now.Add(-2*time.Hour))
	seedRemoteOutboxRow(t, server, "event-fresh", 2, now.Add(-time.Minute))

	if err := server.pruneRemoteOutbox(context.Background()); err != nil {
		t.Fatalf("pruneRemoteOutbox: %v", err)
	}
	if ids := remoteOutboxIDs(t, server); len(ids) != 1 || ids[0] != "event-fresh" {
		t.Fatalf("after prune = %v, want [event-fresh]", ids)
	}
}

// 只按时间删挡不住一次突发：窗口是一小时，一小时够产生几十万条流式分片。
// 容量上限保留**最新**的那些，更老的直接删（它们本来就排在队尾轮不到投）。
func TestPruneRemoteOutboxEnforcesRowCap(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for index := 0; index < 20; index++ {
		seedRemoteOutboxRow(t, server, "event-"+padIndex(index), int64(index+1), now.Add(-time.Duration(index)*time.Second))
	}

	if _, err := server.pruneRemoteOutboxWithLimits(context.Background(), remoteOutboxDeliveryWindow, 5); err != nil {
		t.Fatalf("pruneRemoteOutboxWithLimits: %v", err)
	}
	ids := remoteOutboxIDs(t, server)
	if len(ids) != 5 {
		t.Fatalf("outbox holds %d rows after cap, want 5 (%v)", len(ids), ids)
	}
	// agent_sequence 越大越新：留下的必须是 16..20。
	if ids[0] != "event-"+padIndex(15) || ids[4] != "event-"+padIndex(19) {
		t.Fatalf("cap kept %v, want the newest five (event-15..event-19)", ids)
	}
}

// 没越界时一次都不该删 —— 清理循环每 15 分钟跑一次，不能顺手吃掉正在投递的行。
func TestPruneRemoteOutboxKeepsRowsInsideBothBounds(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for index := 0; index < 5; index++ {
		seedRemoteOutboxRow(t, server, "event-"+padIndex(index), int64(index+1), now.Add(-time.Minute))
	}

	removed, err := server.pruneRemoteOutboxWithLimits(context.Background(), remoteOutboxDeliveryWindow, remoteOutboxMaxRows)
	if err != nil {
		t.Fatalf("pruneRemoteOutboxWithLimits: %v", err)
	}
	if removed != 0 {
		t.Fatalf("prune removed %d rows, want 0", removed)
	}
	if ids := remoteOutboxIDs(t, server); len(ids) != 5 {
		t.Fatalf("outbox holds %d rows, want 5", len(ids))
	}
}

// 清理必须分块提交：真机上第一次清理要删 112 万行，而整个服务只有**一条** SQLite
// 连接 —— 一条 delete 删完，桌面端所有请求就都排在它后面。分块之后每次提交之间连接
// 会被放开，等待中的请求能插进来。
func TestPruneRemoteOutboxDeletesInChunks(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for index := 0; index < 10; index++ {
		seedRemoteOutboxRow(t, server, "event-"+padIndex(index), int64(index+1), now.Add(-2*time.Hour))
	}

	// 单块只删 chunk 行，剩下的留给后面几块。
	removed, err := server.pruneRemoteOutboxChunk(context.Background(), remoteOutboxDeliveryWindow, remoteOutboxMaxRows, 4)
	if err != nil {
		t.Fatalf("pruneRemoteOutboxChunk: %v", err)
	}
	if removed != 4 {
		t.Fatalf("single chunk removed %d rows, want 4 (the chunk bound)", removed)
	}
	if ids := remoteOutboxIDs(t, server); len(ids) != 6 {
		t.Fatalf("after one chunk the outbox holds %d rows, want 6", len(ids))
	}

	// 循环版本会把剩下的删干净。
	total, err := server.pruneRemoteOutboxWithLimits(context.Background(), remoteOutboxDeliveryWindow, remoteOutboxMaxRows)
	if err != nil {
		t.Fatalf("pruneRemoteOutboxWithLimits: %v", err)
	}
	if total != 6 {
		t.Fatalf("follow-up prune removed %d rows, want the remaining 6", total)
	}
	if ids := remoteOutboxIDs(t, server); len(ids) != 0 {
		t.Fatalf("outbox still holds %v after a full prune", ids)
	}
}

// 清理是幂等的：连着跑两次结果一样。
func TestPruneRemoteOutboxIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	seedRemoteOutboxRow(t, server, "event-stale", 1, now.Add(-3*time.Hour))
	seedRemoteOutboxRow(t, server, "event-fresh", 2, now.Add(-time.Minute))

	if _, err := server.pruneRemoteOutboxWithLimits(context.Background(), remoteOutboxDeliveryWindow, remoteOutboxMaxRows); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	removed, err := server.pruneRemoteOutboxWithLimits(context.Background(), remoteOutboxDeliveryWindow, remoteOutboxMaxRows)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if removed != 0 {
		t.Fatalf("second prune removed %d rows, want 0", removed)
	}
	if ids := remoteOutboxIDs(t, server); len(ids) != 1 || ids[0] != "event-fresh" {
		t.Fatalf("after two prunes = %v, want [event-fresh]", ids)
	}
}
