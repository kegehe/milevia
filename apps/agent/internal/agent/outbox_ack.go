package agent

import (
	"context"
	"log"
	"time"
)

// 云端对每一条事件都会回一个 event.ack（或 event.reject）。老实现是在 WebSocket
// 读循环里**同步** POST 一次本地 /api/remote/outbox/ack —— 一条事件一个 HTTP 请求。
//
// 事件洪峰下这是致命的：云端按每秒几百条的节奏回执，读循环把全部时间花在这些
// POST 上（本地库越大每条越慢），下行的命令因此挤不进来。真机实测（2026-09-16）：
// 白天命令往返 3~8 秒，随着积压增长劣化到 19~75 秒，越过了手机端 30 秒的等待
// 上限，手机报失败而桌面端其实晚了几十秒才执行完。
//
// 现在改成「读循环只入队，后台成批提交」：读循环对每条回执只做一次内存入队，
// 由单个 goroutine 攒到 outboxAckBatchSize 条、或 outboxAckFlushDelay 到点后，
// 用**一次** HTTP 请求提交。本地 /api/remote/outbox/ack 与 /fail 本来就收数组
// （见 control-server 的 ackRemoteOutbox / failRemoteOutbox，单次上限 500）。
const (
	// outboxAckBatchSize 与本地端点单次接受的上限一致。
	outboxAckBatchSize = 500
	// outboxAckFlushDelay 是攒批的等待上限：到点就提交，不为凑满一整批拖延。
	outboxAckFlushDelay = 250 * time.Millisecond
	// outboxAckQueueSize 是收件队列长度。满了就丢（见 enqueue）。
	outboxAckQueueSize = 4096
	// outboxAckFlushTimeout 是一次批量提交自己的上限。它不跟连接的 ctx 走：
	// 连接断开时手上这一批已经收下了，丢掉只会让这些行白留一轮。
	//
	// 必须**小于** agentShutdownGrace：关连接时我们会等这个 goroutine 收尾，超时就
	// 撒手重连。取 10s 是为了让它总能在宽限期内自己结束，而不是被遗弃成一个还在发
	// 请求的孤儿。
	outboxAckFlushTimeout = 10 * time.Second
)

// outboxAck 是一条待落地的回执。drop=true 表示云端明确拒绝（永久冲突），
// 本地要删掉这一行而不是继续重试。
type outboxAck struct {
	eventID string
	reason  string
	drop    bool
}

type outboxAckQueue struct {
	ch chan outboxAck
}

func newOutboxAckQueue() *outboxAckQueue {
	return &outboxAckQueue{ch: make(chan outboxAck, outboxAckQueueSize)}
}

// enqueue 绝不阻塞读循环。队列满说明本地确认已经跟不上云端回执的节奏；丢掉一条
// 是安全的：本地 outbox 行并没有被删掉，下一轮重推会带来同一个回执，而确认与
// 丢弃都是幂等的（ack 按 event_id 删行，重复删是空操作）。
func (q *outboxAckQueue) enqueue(item outboxAck) {
	select {
	case q.ch <- item:
	default:
	}
}

// take 取走已经排队的全部回执，不阻塞。
func (q *outboxAckQueue) take() []outboxAck {
	items := make([]outboxAck, 0, len(q.ch))
	for {
		select {
		case item := <-q.ch:
			items = append(items, item)
		default:
			return items
		}
	}
}

// flushOutboxAcks 把一批回执提交给本地服务：正常确认走 outbox/ack，
// 云端明确拒绝的走 outbox/fail（permanent=true，本地直接删行）。
func (a *Agent) flushOutboxAcks(ctx context.Context, batch []outboxAck) {
	if len(batch) == 0 {
		return
	}
	acked := make([]string, 0, len(batch))
	dropped := make(map[string][]string)
	for _, item := range batch {
		if !item.drop {
			acked = append(acked, item.eventID)
			continue
		}
		reason := item.reason
		if reason == "" {
			reason = "the cloud rejected this event"
		}
		dropped[reason] = append(dropped[reason], item.eventID)
	}
	// 每个分片前都看一眼 ctx：连接结束时可能一口气塞进来几千条（queue.take 不限量），
	// 而本地服务一旦卡住，每个请求都会吃满整段超时 —— 没有这个判断就会串行发出好多个
	// 注定超时的请求，把收尾拖到几十秒。
	for start := 0; start < len(acked); start += outboxAckBatchSize {
		if ctx.Err() != nil {
			return
		}
		end := min(start+outboxAckBatchSize, len(acked))
		if err := a.localPost(ctx, "/api/remote/outbox/ack", map[string]any{"eventIds": acked[start:end]}, nil); err != nil {
			log.Printf("outbox ack flush failed: %v", err)
		}
	}
	for reason, ids := range dropped {
		for start := 0; start < len(ids); start += outboxAckBatchSize {
			if ctx.Err() != nil {
				return
			}
			end := min(start+outboxAckBatchSize, len(ids))
			if err := a.localPost(ctx, "/api/remote/outbox/fail", map[string]any{
				"eventIds":  ids[start:end],
				"error":     reason,
				"permanent": true,
			}, nil); err != nil {
				log.Printf("outbox reject flush failed: %v", err)
			}
		}
	}
}

// runOutboxAckQueue 是回执提交的唯一执行者。它一直跑到连接结束；退出前把手上
// 那一批和队列里剩余的尽量交出去。
func (a *Agent) runOutboxAckQueue(ctx context.Context, queue *outboxAckQueue) {
	flush := func(batch []outboxAck) {
		if len(batch) == 0 {
			return
		}
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxAckFlushTimeout)
		defer cancel()
		a.flushOutboxAcks(flushCtx, batch)
	}
	for {
		select {
		case <-ctx.Done():
			flush(queue.take())
			return
		case first := <-queue.ch:
			batch := []outboxAck{first}
			timer := time.NewTimer(outboxAckFlushDelay)
			collecting := true
			for collecting && len(batch) < outboxAckBatchSize {
				select {
				case item := <-queue.ch:
					batch = append(batch, item)
				case <-timer.C:
					collecting = false
				case <-ctx.Done():
					timer.Stop()
					flush(batch)
					flush(queue.take())
					return
				}
			}
			timer.Stop()
			flush(batch)
		}
	}
}
