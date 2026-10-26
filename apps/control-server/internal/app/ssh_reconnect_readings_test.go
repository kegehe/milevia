package app

import (
	"context"
	"testing"
	"time"
)

// TestSSHReconnectInvalidatesCachedReadings 钉住"重连之后旧读数不许再回放"。
//
// 换主机 / 换账号 / 换 root 之后重连，读数缓存仍按同一个 runnerID 存着（runnerID
// 是 `ssh-<连接ID>`，连接没换就还是同一个）—— 不作废的话，界面会先显示**上一台
// 机器**的工具状态，最长可达一个 TTL（5 分钟）。
//
// 这条正好是"装在 A 上的工具"与"装在 B 上的工具"被混为一谈的场景：用户改了连接
// 的主机，刷新页面看到的却是旧主机上装了什么。
func TestSSHReconnectInvalidatesCachedReadings(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	const connID = "conn"
	if _, err := server.db.Exec(`insert into ssh_connections
		(id,name,host,port,user,private_key_path,known_hosts,root_path,status,error_msg,created_at,updated_at,password)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		connID, "prod", "host", 22, "user", "key", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest", "/workspace", "connected", "", now, now, ""); err != nil {
		t.Fatalf("seed ssh connection: %v", err)
	}
	server.sshPrepare = func(_ context.Context, connection SSHConnection) (*sshRunner, RunnerMeta, error) {
		return &sshRunner{client: &sshClient{}, connID: connection.ID, rootPath: connection.RootPath},
			RunnerMeta{ID: "ssh-" + connection.ID, Name: connection.Name, Environment: "remote-linux", Root: connection.RootPath}, nil
	}

	ctx := context.Background()
	connection, err := server.loadSSHConnection(ctx, connID)
	if err != nil {
		t.Fatalf("load ssh connection: %v", err)
	}
	if err := server.tryConnectAndRegister(ctx, connection); err != nil {
		t.Fatalf("connect: %v", err)
	}
	runnerID := "ssh-" + connID

	// 塞一份"上一台机器"的读数。
	server.agentReadingsMu.Lock()
	if server.agentReadings == nil {
		server.agentReadings = map[string]agentReading{}
	}
	server.agentReadings[runnerID] = agentReading{
		at:       time.Now(),
		statuses: []AgentStatus{{ID: "claude-code", Status: agentStatusReady, Version: "2.1.1"}},
	}
	server.agentReadingsMu.Unlock()

	// 重连（换主机/换 root 之后走的就是这条路）。
	reloaded, err := server.loadSSHConnection(ctx, connID)
	if err != nil {
		t.Fatalf("reload ssh connection: %v", err)
	}
	if err := server.tryConnectAndRegister(ctx, reloaded); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	server.agentReadingsMu.Lock()
	entry := server.agentReadings[runnerID]
	server.agentReadingsMu.Unlock()
	if len(entry.statuses) != 0 {
		t.Fatalf("重连后旧读数仍在缓存里，会被回放到新环境上：%#v", entry.statuses)
	}
}
