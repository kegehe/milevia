package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSupervisionStorageCountsUTF8AndRollsBackOversizedWrites(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	before, err := supervisionStoredBytes(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`insert into supervision_amendments(id,session_id,content,state,created_at) values ('utf8',?,?,'pending',?)`, ss.ID, "中文", now); err != nil {
		t.Fatal(err)
	}
	after, err := supervisionStoredBytes(ctx, s.db, ss.ID)
	if err != nil || after-before != int64(len("中文")) {
		t.Fatalf("UTF-8 bytes delta=%d err=%v", after-before, err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	// A small existing budget models an upgraded database already close to its limit.
	budget := defaultSupervisionBudget()
	budget.MaxStorageBytes = 16 << 20
	if _, err := s.db.Exec(`update supervision_sessions set budget=? where id=?`, string(mustJSON(budget)), ss.ID); err != nil {
		t.Fatal(err)
	}
	err = s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		_, err := tx.Exec(`insert into supervision_amendments(id,session_id,content,state,created_at) values ('oversized',?,?,'pending',?)`, ss.ID, strings.Repeat("x", 17<<20), now)
		return err
	})
	if !errors.Is(err, errSupervisionStorage) {
		t.Fatalf("storage admission: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_amendments where id='oversized'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("oversized write committed: %d %v", count, err)
	}
	got := supervisionSessionForTest(t, s, ss.ID)
	if got.Control != "pause" || got.Reason != "storage_limit" {
		t.Fatalf("storage failure did not pause: %+v", got)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, got.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionDiskFreeAndBudgetCompatibility(t *testing.T) {
	if available, err := supervisionDiskFree(t.TempDir()); err != nil || available == 0 {
		t.Fatalf("disk free=%d %v", available, err)
	}
	legacy := defaultSupervisionBudget()
	legacy.MaxStorageBytes = 0
	if err := legacy.validate(); err != nil || legacy.storageLimit() != supervisionStorageLimit {
		t.Fatalf("legacy budget: %+v %v", legacy, err)
	}
	for _, size := range []int64{-1, 1, 65 << 30} {
		b := legacy
		b.MaxStorageBytes = size
		if b.validate() == nil {
			t.Fatalf("invalid budget %d accepted", size)
		}
	}
}
