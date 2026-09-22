package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 一台电脑的 WS 断了、但 cloud_instances.status 还停在 online 的时候，命令必须在
// 宽限期过后**落成终态**，而不是留在 queued 让手机端干等。
//
// 真实事故（2026-09-17）：桌面端 agent 在 07:35 与云端断开，而 07:35:03 那次快照
// 上传把 status 重新写成了 online（agentSnapshot 一定带 status='online'）。于是
// createCommand 的前置检查放行、pushCommand 找不到连接直接 return，命令躺在
// queued 里，手机端一路轮询到 30 秒上限才报"仍在处理中"，五分钟后才过期 ——
// 而它永远不会被投递。
//
// 宽限期本身是必要的：Agent 断线后会自动重连，重连时 sendPendingCommands 会把
// queued 的命令补投出去，一次几秒的抖动不该被当成故障。
func TestCommandFailsAfterGraceWhenNoRelayConnectionEndToEnd(t *testing.T) {
	server, handler := openTestServer(t)

	previousGrace := commandDeliveryGrace
	commandDeliveryGrace = 300 * time.Millisecond
	defer func() { commandDeliveryGrace = previousGrace }()

	instanceID, _ := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()

	// 制造"状态是 online、但没有任何 Agent 连接"这一状态：快照上传就是这么一个
	// 会把 status 写回 online 的路径。
	if _, err := server.db.Exec(context.Background(),
		`update cloud_instances set status='online', last_seen_at=now(), updated_at=now() where instance_id=$1`,
		instanceID); err != nil {
		t.Fatalf("force stale online status: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/instances/"+instanceID+"/commands",
		strings.NewReader(`{"type":"task.create","projectId":"project-1","payload":{"title":"t","description":"d","priority":"normal"}}`))
	request.Header.Set("Authorization", "Bearer user-token")
	request.Header.Set("Idempotency-Key", "no-relay-connection")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	// 前置检查仍然放行（它只看状态字段），所以这里拿到的还是 202。
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("command status = %d, want %d: %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var accepted struct {
		CommandID string `json:"commandId"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil || accepted.CommandID == "" {
		t.Fatalf("command response has no commandId: %s", recorder.Body.String())
	}

	// 宽限期之内它必须还在 queued：那是 Agent 重连补投的窗口。
	var status string
	if err := server.db.QueryRow(context.Background(),
		`select status from cloud_commands where command_id=$1`, accepted.CommandID).Scan(&status); err != nil {
		t.Fatalf("read command: %v", err)
	}
	if status != "queued" {
		t.Fatalf("command status = %q during the grace period, want \"queued\" (an Agent reconnect must still be able to pick it up)", status)
	}

	// 宽限期过后，靠手机端的轮询把它推入终态。
	time.Sleep(commandDeliveryGrace + 100*time.Millisecond)
	poll := httptest.NewRequest(http.MethodGet, "/v1/commands/"+accepted.CommandID, nil)
	poll.Header.Set("Authorization", "Bearer user-token")
	pollRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pollRecorder, poll)
	if pollRecorder.Code != http.StatusOK {
		t.Fatalf("poll status = %d: %s", pollRecorder.Code, pollRecorder.Body.String())
	}

	var result string
	if err := server.db.QueryRow(context.Background(),
		`select status, coalesce(result::text,'') from cloud_commands where command_id=$1`,
		accepted.CommandID).Scan(&status, &result); err != nil {
		t.Fatalf("read command: %v", err)
	}
	if status != "failed" {
		t.Fatalf("command status = %q after the grace period, want \"failed\": a command that can never be delivered must not sit in queued", status)
	}
	if !strings.Contains(result, "不在线") {
		t.Fatalf("failure result should tell the user the computer is offline, got %s", result)
	}
	// 落成 failed 之后，重连补投必须够不着它 —— 否则会出现"手机说失败了、
	// 电脑端过一会儿又执行了"。
	if !strings.Contains(pollRecorder.Body.String(), `"failed"`) {
		t.Fatalf("poll response should carry the terminal status, got %s", pollRecorder.Body.String())
	}
}

// 反面对照：命令一旦被 Agent 接单（received/executing），后续的"投不出去"判定
// 就不能再覆盖它的状态 —— 那会把一条正在执行的命令说成失败。
func TestFailUndeliverableCommandLeavesDeliveredCommandsAlone(t *testing.T) {
	server, handler := openTestServer(t)

	instanceID, _ := registerTestInstance(t, handler)
	defer func() {
		_, _ = server.db.Exec(context.Background(), `delete from cloud_instances where instance_id=$1`, instanceID)
	}()

	commandID := "cmd-" + strings.Repeat("a", 32)
	if _, err := server.db.Exec(context.Background(),
		`insert into cloud_commands(command_id,instance_id,idempotency_key,type,payload,status,expires_at) values($1,$2,$3,$4,'{}'::jsonb,'executing',now()+interval '5 minutes')`,
		commandID, instanceID, "in-flight", "task.dispatch"); err != nil {
		t.Fatalf("seed in-flight command: %v", err)
	}

	server.failUndeliverableCommand(commandID, "电脑端当前不在线，请确认电脑上的 Milevia 正在运行并已连接")

	var status string
	if err := server.db.QueryRow(context.Background(), `select status from cloud_commands where command_id=$1`, commandID).Scan(&status); err != nil {
		t.Fatalf("read command: %v", err)
	}
	if status != "executing" {
		t.Fatalf("in-flight command status = %q, want \"executing\" (it must not be overwritten)", status)
	}
}
