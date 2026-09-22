package agent

import (
	"testing"
	"time"
)

// 一次收尾要等五个后台 goroutine。期限必须是**共用**的绝对时刻：各给一份的话
// 总等待时间随数量线性增长（五个各 15 秒 = 75 秒），重连会被拖到用户以为是卡死。
func TestShutdownWaitsShareOneDeadline(t *testing.T) {
	blocked := make(chan struct{}) // 永不关闭：模拟一个卡住的后台 goroutine
	deadline := time.Now().Add(80 * time.Millisecond)

	start := time.Now()
	for _, name := range []string{"websocket writer", "command reader", "ack flusher", "snapshot sync", "outbox pump"} {
		if waitForBackground(name, blocked, deadline) {
			t.Fatalf("%s reported a stop that never happened", name)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 400*time.Millisecond {
		t.Fatalf("five blocked goroutines took %s to give up; the five waits are not sharing one deadline", elapsed)
	}
}

// 已经停下了的 goroutine 必须被立刻认出来，不能被期限拖住。
func TestShutdownWaitReturnsAsSoonAsTheGoroutineStops(t *testing.T) {
	done := make(chan struct{})
	close(done)

	start := time.Now()
	if !waitForBackground("already stopped", done, time.Now().Add(time.Second)) {
		t.Fatal("a goroutine that had already stopped was reported as timed out")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("waiting for an already-stopped goroutine took %s", elapsed)
	}
}

// 期限已经用光时直接放弃，不再白等一轮。
func TestShutdownWaitSkipsWhenTheGraceIsSpent(t *testing.T) {
	blocked := make(chan struct{})
	if waitForBackground("late arrival", blocked, time.Now().Add(-time.Second)) {
		t.Fatal("reported success after the shutdown grace had already been spent")
	}
}
