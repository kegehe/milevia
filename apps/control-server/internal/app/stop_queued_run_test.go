package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 排队中的非流式回合（Codex / Cursor / OpenCode 这类一次性运行）在真正拉起之前没有
// cancel 函数，只躺在 nonStreamingLaunches 里。停止它时必须要么拒绝并要求强制停止，
// 要么真的把它从队列里摘掉 —— 不能把它自己的 queued 状态当成"没在跑"回给调用方。
// 后者会让桌面端收掉停止键，而队列里的那一轮照常开始。
func TestStopQueuedNonStreamingRunRequiresForceWhenAnotherTurnIsActive(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	seedConversationRuns(t, server, conversationID, []seededRun{
		{id: "running-run", status: "running", at: now.Add(-time.Minute)},
		{id: "queued-run", status: "queued", at: now},
	})
	server.mu.Lock()
	server.nonStreamingLaunches["queued-run"] = &nonStreamingRunLaunch{conversation: Conversation{ID: conversationID}}
	server.mu.Unlock()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/queued-run/stop", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stop status = %d body = %s, want 409", response.Code, response.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stop response %q: %v", response.Body.String(), err)
	}
	if payload.Code != "active_runs_present" {
		t.Fatalf("code = %q, want active_runs_present（桌面端按码弹强制停止）", payload.Code)
	}

	// 被拒的这一次不能动队列：还在跑的那一轮结束后，这一条仍应照常开始。
	server.mu.Lock()
	_, stillQueued := server.nonStreamingLaunches["queued-run"]
	server.mu.Unlock()
	if !stillQueued {
		t.Fatal("被拒的停止把排队中的回合从队列里摘掉了")
	}
	var status string
	if err := server.db.QueryRow(`select status from runs where id='queued-run'`).Scan(&status); err != nil {
		t.Fatalf("load queued run: %v", err)
	}
	if status != "queued" {
		t.Fatalf("queued run status = %q, want queued", status)
	}
}

// 队列里只剩一行"没人认领"的 queued（例如控制服务重启后残留）：它永远不会被拉起，
// 停止必须把它按 stopped 收尾，而不是把 queued 原样回给调用方当成功。
func TestStopOrphanQueuedRunFinishesIt(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "orphan-run", status: "queued", at: time.Now().UTC()}})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/orphan-run/stop", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d body = %s, want 202", response.Code, response.Body.String())
	}
	var status string
	if err := server.db.QueryRow(`select status from runs where id='orphan-run'`).Scan(&status); err != nil {
		t.Fatalf("load stopped run: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status = %q, want stopped", status)
	}
}

// 它是唯一活跃回合时，停止必须真的生效：从队列里摘掉并按 stopped 收尾。
func TestStopQueuedNonStreamingRunFinishesIt(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "queued-run", status: "queued", at: now}})
	server.mu.Lock()
	server.nonStreamingLaunches["queued-run"] = &nonStreamingRunLaunch{conversation: Conversation{ID: conversationID}}
	server.mu.Unlock()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/queued-run/stop", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d body = %s, want 202", response.Code, response.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stop response %q: %v", response.Body.String(), err)
	}
	if payload.Status != "stopping" {
		t.Fatalf("status = %q, want stopping", payload.Status)
	}
	server.mu.Lock()
	_, stillQueued := server.nonStreamingLaunches["queued-run"]
	server.mu.Unlock()
	if stillQueued {
		t.Fatal("排队中的回合没有被摘出队列")
	}
	var status, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='queued-run'`).Scan(&status); err != nil {
		t.Fatalf("load stopped run: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status = %q, want stopped", status)
	}
	if err := server.db.QueryRow(`select status from conversations where id=?`, conversationID).Scan(&conversationStatus); err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if conversationStatus != "idle" {
		t.Fatalf("conversation status = %q, want idle", conversationStatus)
	}
}

// 停止已受理、但清理链路没收敛（后裔进程占着 stdout / Run 永远不返回）时，兜底计时器
// 必须在有界时间内把运行收成 stopped —— 这是"停止键永远停在再次停止"的最后一道保险。
func TestStopDeadlineFinalizesHungRun(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "hung-run", status: "running", at: time.Now().UTC()}})
	server.armStopDeadlineWithin("hung-run", conversationID, 50*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for {
		var status string
		if err := server.db.QueryRow("select status from runs where id='hung-run'").Scan(&status); err != nil {
			t.Fatalf("load run: %v", err)
		}
		if status == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q after the stop deadline, want stopped", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	server.mu.Lock()
	_, pending := server.stopDeadlines["hung-run"]
	server.mu.Unlock()
	if pending {
		t.Fatal("停之后兜底计时器没有被摘掉")
	}
}

// 正常路径先到终态时，迟到的兜底计时器不能把结果改写成 stopped。
func TestStopDeadlineLeavesFinishedRunAlone(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "done-run", status: "completed", at: time.Now().UTC()}})
	server.armStopDeadlineWithin("done-run", conversationID, 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	var status string
	if err := server.db.QueryRow("select status from runs where id='done-run'").Scan(&status); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if status != "completed" {
		t.Fatalf("run status = %q, want completed", status)
	}
}

// 停止接口必须真的挂上兜底计时器 —— 只测 armStopDeadlineWithin 本身会漏掉"调用点忘了接"。
func TestStopArmsDeadlineForRunningStreamingSession(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "live-run", status: "running", at: time.Now().UTC()}})
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, activeRunID: "live-run"}
	server.runContexts["live-run"] = conversationID
	server.mu.Unlock()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/live-run/stop", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d body = %s, want 202", response.Code, response.Body.String())
	}
	server.mu.Lock()
	_, armed := server.stopDeadlines["live-run"]
	server.mu.Unlock()
	if !armed {
		t.Fatal("停止返回 202 之后没有挂兜底计时器；清理链路失灵时运行会永久停在 running")
	}
	server.disarmStopDeadline("live-run")
}

// 兜底收尾只能停"仍拥有这一轮"的会话：同一会话里上一轮结束后可能已经换成了新会话，
// 盲目 Stop 会把用户刚发起的新一轮一起杀掉。
func TestStopDeadlineDoesNotStopAReplacementSession(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "hung-run", status: "running", at: time.Now().UTC()}})
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, runIDs: map[string]struct{}{"other-run": {}}}
	server.mu.Unlock()
	server.armStopDeadlineWithin("hung-run", conversationID, 30*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for {
		var status string
		if err := server.db.QueryRow("select status from runs where id='hung-run'").Scan(&status); err != nil {
			t.Fatalf("load run: %v", err)
		}
		if status == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q, want stopped", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-session.done:
		t.Fatal("兜底收尾停掉了并不拥有这一轮的替换会话")
	default:
	}
}

// 兜底收尾必须真的去停"仍拥有这一轮"的会话（异步 Stop 也不能漏）。
func TestStopDeadlineStopsTheOwningSession(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "hung-run", status: "running", at: time.Now().UTC()}})
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, runIDs: map[string]struct{}{"hung-run": {}}}
	server.mu.Unlock()
	server.armStopDeadlineWithin("hung-run", conversationID, 30*time.Millisecond)

	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
		t.Fatal("兜底收尾没有停掉仍拥有这一轮的会话")
	}
	// Stop 是异步发出的、且排在 finishRun 之前；等运行也落成 stopped，避免测试在
	// 兜底事务提交前结束、与 t.Cleanup 关库赛跑。
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status string
		if err := server.db.QueryRow("select status from runs where id='hung-run'").Scan(&status); err != nil {
			t.Fatalf("load run: %v", err)
		}
		if status == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %q, want stopped", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 正常终态提交后必须真的摘掉兜底计时器；只验证状态复查不够（到点自己也会删表）。
func TestFinishRunDisarmsStopDeadline(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "run", status: "running", at: time.Now().UTC()}})
	server.armStopDeadlineWithin("run", conversationID, time.Minute)
	server.finishRun("run", conversationID, "stopped", nil)
	server.mu.Lock()
	_, armed := server.stopDeadlines["run"]
	server.mu.Unlock()
	if armed {
		t.Fatal("finishRun 提交成功后没有摘掉兜底计时器")
	}
	var status string
	if err := server.db.QueryRow("select status from runs where id='run'").Scan(&status); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status = %q, want stopped", status)
	}
}

// 强制停止必须把没有 launch 记录的 queued 也立即收尾，不能让它继续排队等兜底。
func TestForceStopFinishesQueuedRunWithoutLaunch(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "orphan-run", status: "queued", at: time.Now().UTC()}})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/orphan-run/stop?force=true", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("stop status = %d body = %s, want 202", response.Code, response.Body.String())
	}
	var status string
	if err := server.db.QueryRow("select status from runs where id='orphan-run'").Scan(&status); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status = %q, want stopped（强制停止不能把 queued 留到兜底）", status)
	}
}

// 兜底对非流式回合（不在 sessions 里）也必须补一次 cancel。
func TestStopDeadlineCancelsNonStreamingRun(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	seedConversationRuns(t, server, conversationID, []seededRun{{id: "hung-run", status: "running", at: time.Now().UTC()}})
	// cancel 必须幂等：Server.Close 收尾时还会再调一次，不能 close 两次。
	cancelled := make(chan struct{}, 1)
	server.mu.Lock()
	server.cancels["hung-run"] = func() {
		select {
		case cancelled <- struct{}{}:
		default:
		}
	}
	server.mu.Unlock()
	server.armStopDeadlineWithin("hung-run", conversationID, 30*time.Millisecond)
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("兜底没有对非流式回合补发 cancel")
	}
}

// 兜底强收后旧进程可能还活着：同一会话不能再起新回合，直到它的 runContexts 消失。
func TestStartMessageBlocksWhileForcedStopProcessAlive(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	server.mu.Lock()
	if server.forcedStopRuns == nil {
		server.forcedStopRuns = map[string]string{}
	}
	server.runContexts["forced-run"] = conversationID
	server.forcedStopRuns["forced-run"] = conversationID
	server.mu.Unlock()

	_, _, _, code, err := server.startMessage(context.Background(), conversationID, "hi", "", nil)
	if code != http.StatusConflict || err == nil || err.Error() != "上一轮运行还没有完全退出，请稍后重试" {
		t.Fatalf("startMessage code=%d err=%v, want the forced-stop guard", code, err)
	}
	server.mu.Lock()
	_, markerKept := server.forcedStopRuns["forced-run"]
	server.mu.Unlock()
	if !markerKept {
		t.Fatal("runContexts 还在时 forcedStopRuns 标记被提前清掉了")
	}

}
