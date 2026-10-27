package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

func newNotificationRetentionTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`create table projects (id text primary key, name text)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	if err := s.migrateNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedNotificationRows(t *testing.T, s *Server, prefix string, count, dismissed int, createdAt time.Time) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`insert into notifications (id,type,project_id,title,body,action_url,dismissed,created_at)
		values (?,'task.done','','title','body','/',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < count; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("%s%06d", prefix, i), dismissed, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationRetentionExpiryBoundaries(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	fixtures := []struct {
		id        string
		dismissed int
		age       time.Duration
		keep      bool
	}{
		{"read-expired", 1, notificationReadRetention + time.Second, false},
		{"read-boundary", 1, notificationReadRetention, true},
		{"read-recent", 1, time.Hour, true},
		{"unread-expired", 0, notificationUnreadRetention + time.Second, false},
		{"unread-boundary", 0, notificationUnreadRetention, true},
		{"unread-older-than-read-window", 0, 10 * 24 * time.Hour, true},
	}
	for _, item := range fixtures {
		seedNotificationRows(t, s, item.id, 1, item.dismissed, now.Add(-item.age))
	}
	sub := &stateEventSubscriber{send: make(chan StateEvent, 1)}
	s.stateEventSubs = map[*websocket.Conn]*stateEventSubscriber{nil: sub}
	removed, err := s.pruneNotifications(context.Background(), now)
	if err != nil || removed != 2 {
		t.Fatalf("prune = (%d, %v), want (2, nil)", removed, err)
	}
	var count int
	for _, item := range fixtures {
		var exists bool
		if err := s.db.QueryRow(`select exists(select 1 from notifications where id=?)`, item.id+"000000").Scan(&exists); err != nil || exists != item.keep {
			t.Fatalf("notification %s retained=%v, err=%v, want %v", item.id, exists, err, item.keep)
		}
	}
	if err := s.db.QueryRow(`select count(*) from notifications`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("retained rows = %d, err=%v", count, err)
	}
	select {
	case event := <-sub.send:
		if event.Type != stEvNotifications {
			t.Fatalf("invalidation = %#v", event)
		}
	default:
		t.Fatal("missing notification invalidation after prune")
	}
	removed, err = s.pruneNotifications(context.Background(), now)
	if err != nil || removed != 0 || len(sub.send) != 0 {
		t.Fatalf("second prune = (%d, %v), invalidations=%d", removed, err, len(sub.send))
	}
}

func TestNotificationRetentionChunksAndCapacity(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	// 同时覆盖过期行跨块和超量行跨块；新记录全部同一时间戳，裁剪必须有稳定次序。
	seedNotificationRows(t, s, "expired-read-", notificationPruneChunk+1, 1, now.Add(-notificationReadRetention-time.Hour))
	seedNotificationRows(t, s, "expired-unread-", notificationPruneChunk+1, 0, now.Add(-notificationUnreadRetention-time.Hour))
	freshRows := notificationMaxRows + 2*notificationPruneChunk + 3
	seedNotificationRows(t, s, "fresh-", freshRows, 0, now)
	removed, err := s.pruneNotificationsChunk(context.Background(), now)
	if err != nil || removed != notificationPruneChunk {
		t.Fatalf("first chunk = (%d, %v), want (%d, nil)", removed, err, notificationPruneChunk)
	}
	rest, err := s.pruneNotifications(context.Background(), now)
	wantRemoved := 2*(notificationPruneChunk+1) + freshRows - notificationMaxRows
	if err != nil || removed+rest != int64(wantRemoved) {
		t.Fatalf("total removed = %d, err=%v, want %d", removed+rest, err, wantRemoved)
	}
	var count int
	var first, last string
	if err := s.db.QueryRow(`select count(*),min(id),max(id) from notifications`).Scan(&count, &first, &last); err != nil {
		t.Fatal(err)
	}
	if count != notificationMaxRows || first != fmt.Sprintf("fresh-%06d", freshRows-notificationMaxRows) || last != fmt.Sprintf("fresh-%06d", freshRows-1) {
		t.Fatalf("retained = (%d, %s, %s)", count, first, last)
	}
}

func TestListNotificationsBoundsBeforeFirstMaintenance(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	seedNotificationRows(t, s, "new-", notificationMaxRows+3, 0, now)
	seedNotificationRows(t, s, "expired-", 3, 0, now.Add(-notificationUnreadRetention-time.Hour))
	seedNotificationRows(t, s, "dismissed-", 3, 1, now.Add(time.Second))
	w := httptest.NewRecorder()
	s.listNotifications(w, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	var items []NotificationEvent
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode response status=%d: %v", w.Code, err)
	}
	if w.Code != http.StatusOK || len(items) != notificationMaxRows {
		t.Fatalf("response status=%d count=%d", w.Code, len(items))
	}
	for i, item := range items {
		if want := fmt.Sprintf("new-%06d", notificationMaxRows+2-i); item.ID != want {
			t.Fatalf("item %d = %q, want %q", i, item.ID, want)
		}
	}
}

func TestListNotificationsExcludesExpiredUnread(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	seedNotificationRows(t, s, "expired-", 1, 0, now.Add(-notificationUnreadRetention-time.Hour))
	seedNotificationRows(t, s, "dismissed-", 1, 1, now)
	w := httptest.NewRecorder()
	s.listNotifications(w, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty response status=%d body=%s", w.Code, w.Body)
	}
}

func TestNotificationRetentionCancellation(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	seedNotificationRows(t, s, "expired-", 1, 0, time.Now().UTC().Add(-notificationUnreadRetention-time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if removed, err := s.pruneNotifications(ctx, time.Now().UTC()); removed != 0 || err != context.Canceled {
		t.Fatalf("canceled prune = (%d, %v)", removed, err)
	}
	s.runtimeCtx = ctx
	s.startNotificationRetentionLoop()
	done := make(chan struct{})
	go func() {
		s.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notification retention loop did not stop with runtime context")
	}
}

func TestNotificationRetentionDoesNotStartAfterClose(t *testing.T) {
	// 已进入停机的 Server 不能再 Add 到 runWG，也不能启动使用已关闭数据库的任务。
	s := &Server{closing: true}
	s.startNotificationRetentionLoop()
	s.runWG.Wait()
}

func TestNotificationRetentionInvalidatesAfterPartialFailure(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	seedNotificationRows(t, s, "expired-", 2*notificationPruneChunk+1, 0, now.Add(-notificationUnreadRetention-time.Hour))
	if _, err := s.db.Exec(fmt.Sprintf(`create trigger fail_next_notification_chunk before delete on notifications
		when old.id='expired-%06d' begin select raise(abort,'simulated prune failure'); end`, notificationPruneChunk)); err != nil {
		t.Fatal(err)
	}
	sub := &stateEventSubscriber{send: make(chan StateEvent, 2)}
	s.stateEventSubs = map[*websocket.Conn]*stateEventSubscriber{nil: sub}
	removed, err := s.pruneNotifications(context.Background(), now)
	if err == nil || removed != notificationPruneChunk {
		t.Fatalf("partial prune = (%d, %v), want one committed chunk and an error", removed, err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from notifications`).Scan(&count); err != nil || count != notificationPruneChunk+1 {
		t.Fatalf("rows after rollback = %d, err=%v", count, err)
	}
	if len(sub.send) != 1 || (<-sub.send).Type != stEvNotifications {
		t.Fatal("committed deletions were not invalidated after the later chunk failed")
	}
	if _, err := s.db.Exec(`drop trigger fail_next_notification_chunk`); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.pruneNotifications(context.Background(), now); err != nil || removed != notificationPruneChunk+1 {
		t.Fatalf("resumed prune = (%d, %v)", removed, err)
	}
}

func TestNotificationMigrationReplacesLegacyIndex(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	seedNotificationRows(t, s, "keep-", 1, 0, time.Now().UTC())
	if _, err := s.db.Exec(`drop index idx_notifications_unread_order;
		create index idx_notifications_dismissed on notifications(dismissed,created_at desc)`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	var obsoleteIndexes int
	if err := s.db.QueryRow(`select count(*) from sqlite_master where type='index' and name='idx_notifications_dismissed'`).Scan(&obsoleteIndexes); err != nil || obsoleteIndexes != 0 {
		t.Fatalf("redundant indexes after upgrade = %d, err=%v", obsoleteIndexes, err)
	}
	items, err := s.getUnreadNotifications(context.Background())
	if err != nil || len(items) != 1 || items[0].ID != "keep-000000" {
		t.Fatalf("unread notifications after upgrade = %#v, err=%v", items, err)
	}
}

func TestNotificationWebSocketHistoryIsBounded(t *testing.T) {
	s := newTestServer(t)
	seedNotificationRows(t, s, "history-", notificationMaxRows+3, 0, time.Now().UTC())
	httpServer := httptest.NewServer(s.routes())
	t.Cleanup(httpServer.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws/notifications", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < notificationSubscriberQueueSize; i++ {
		var event NotificationEvent
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read history %d: %v", i, err)
		}
		if want := fmt.Sprintf("history-%06d", notificationMaxRows+2-i); event.ID != want {
			t.Fatalf("history %d = %s, want %s", i, event.ID, want)
		}
	}
	s.broadcastNotification(NotificationEvent{ID: "live-after-history", Type: "task.done", Title: "live", ActionURL: "/"})
	var event NotificationEvent
	if err := conn.ReadJSON(&event); err != nil || event.ID != "live-after-history" {
		t.Fatalf("after history = %s, err=%v; expected live event without extra history", event.ID, err)
	}
}

func TestNotificationSubscriberEnqueueAndClose(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		sub := &notificationSubscriber{send: make(chan []byte, notificationSubscriberQueueSize)}
		start := make(chan struct{})
		var workers sync.WaitGroup
		for writer := 0; writer < 2; writer++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for item := 0; item < notificationSubscriberQueueSize; item++ {
					sub.enqueue([]byte(`{"type":"history-or-live"}`))
				}
			}()
		}
		close(start)
		sub.close()
		workers.Wait()
		if sub.enqueue([]byte(`{}`)) {
			t.Fatal("closed subscriber accepted a notification")
		}
	}
}

func TestDismissNotificationIsIdempotentAfterRetention(t *testing.T) {
	s := newNotificationRetentionTestServer(t)
	now := time.Now().UTC()
	seedNotificationRows(t, s, "expired-", 1, 0, now.Add(-notificationUnreadRetention-time.Hour))
	seedNotificationRows(t, s, "current-", 1, 0, now)
	if _, err := s.pruneNotifications(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post("/api/notifications/{notificationID}/dismiss", s.dismissNotification)
	for _, id := range []string{"expired-000000", "current-000000", "current-000000", "missing"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/notifications/"+id+"/dismiss", nil))
		if w.Code != http.StatusNoContent {
			t.Fatalf("dismiss %s status=%d, body=%s", id, w.Code, w.Body)
		}
	}
	var dismissed bool
	if err := s.db.QueryRow(`select dismissed from notifications where id='current-000000'`).Scan(&dismissed); err != nil || !dismissed {
		t.Fatalf("current notification dismissed=%v, err=%v", dismissed, err)
	}
}
