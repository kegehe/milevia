package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func supervisionRequestForTest(s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func TestSupervisionRequestsCreateAndInstructionRetriesCommitOnce(t *testing.T) {
	s, p, taskID, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	body := fmt.Sprintf(`{"taskIds":[%q],"schedulerAgentId":"claude-code"}`, taskID)
	path := "/api/projects/" + p.ID + "/supervision"
	first := supervisionRequestForTest(s, "POST", path, "create-once", body)
	second := supervisionRequestForTest(s, "POST", path, "create-once", body)
	if first.Code != 201 || second.Code != 201 || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("create retry: %d %d %s", first.Code, second.Code, second.Body.String())
	}
	var a, b supervisionSession
	json.Unmarshal(first.Body.Bytes(), &a)
	json.Unmarshal(second.Body.Bytes(), &b)
	if a.ID == "" || a.ID != b.ID {
		t.Fatal("retry created a second session")
	}
	path = "/api/supervision/" + a.ID + "/instructions"
	body = `{"stateVersion":1,"content":"保留已有改动"}`
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- supervisionRequestForTest(s, "POST", path, "instruction-once", body).Code
		}()
	}
	wg.Wait()
	close(results)
	for status := range results {
		if status != 202 {
			t.Fatalf("concurrent retry status=%d", status)
		}
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_amendments where session_id=?`, a.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate amendments=%d %v", count, err)
	}
	if response := supervisionRequestForTest(s, "POST", path, "instruction-once", `{"stateVersion":1,"content":"不同内容"}`); response.Code != 409 {
		t.Fatalf("key reuse status=%d", response.Code)
	}
}

func TestSupervisionRequestFailedMutationCanBeRetriedAndPutReplays(t *testing.T) {
	s, _, _, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	path := "/api/supervision/" + ss.ID + "/roles"
	body := `{"stateVersion":2,"roles":{"review":{"model":"sonnet"}}}`
	if response := supervisionRequestForTest(s, "PUT", path, "roles-once", body); response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, 1, supervisionRoleConfigs{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if response := supervisionRequestForTest(s, "PUT", path, "roles-once", body); response.Code != 204 || response.Body.Len() != 0 {
			t.Fatalf("roles replay: %d %s", response.Code, response.Body.String())
		}
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.StateVersion != 3 {
		t.Fatalf("retry mutated state twice: %d", got.StateVersion)
	}
	if response := supervisionRequestForTest(s, "PUT", path, strings.Repeat("x", 129), body); response.Code != 400 {
		t.Fatalf("bad key: %d", response.Code)
	}
}

func TestSupervisionRequestStopReplayDoesNotChangeVersion(t *testing.T) {
	s, _, _, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	path := "/api/supervision/" + ss.ID + "/control"
	body := `{"stateVersion":1,"action":"stop"}`
	for i := 0; i < 2; i++ {
		if response := supervisionRequestForTest(s, "POST", path, "stop-once", body); response.Code != 202 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "stopped" || got.StateVersion != 2 {
		t.Fatalf("stop retry mutated state: %+v", got)
	}
}

func TestSupervisionQuestionLateAnswersRemainHistoryAndNeverResume(t *testing.T) {
	for _, scenario := range []string{"paused", "stopped", "requirements_changed", "pending_instruction"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			ctx := context.Background()
			snapshot := fmt.Sprintf(`{%q:1}`, taskID)
			if _, err := s.db.Exec(`insert into supervision_questions(id,session_id,task_id,question,requirements_snapshot,created_at) values ('versioned',?,?,?, ?,?)`, ss.ID, taskID, "确认验收", snapshot, now); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "paused":
				startSupervisionForTest(t, s, ss.ID, now)
				ss = supervisionSessionForTest(t, s, ss.ID)
				if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", now); err != nil {
					t.Fatal(err)
				}
				if err := s.settleSupervisionControl(ctx, ss.ID, now); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				if err := s.requestSupervisionControl(ctx, ss.ID, 1, "stop", now); err != nil {
					t.Fatal(err)
				}
			case "requirements_changed":
				if _, err := s.db.Exec(`update supervision_tasks set requirements_revision=2 where session_id=?`, ss.ID); err != nil {
					t.Fatal(err)
				}
			case "pending_instruction":
				if _, err := s.db.Exec(`insert into supervision_amendments(id,session_id,content,state,created_at) values ('new',?,'新增要求','pending',?)`, ss.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			before := supervisionSessionForTest(t, s, ss.ID)
			response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/questions/versioned/answer", "answer-once", `{"answer":"确认"}`)
			if response.Code != 202 {
				t.Fatal(response.Code, response.Body.String())
			}
			after := supervisionSessionForTest(t, s, ss.ID)
			if after.State != before.State || after.Control != before.Control {
				t.Fatalf("answer changed control: %+v => %+v", before, after)
			}
			var amendments int
			if err := s.db.QueryRow(`select count(*) from supervision_amendments where session_id=? and content like '问题：%'`, ss.ID).Scan(&amendments); err != nil {
				t.Fatal(err)
			}
			if scenario == "paused" {
				if amendments != 1 {
					t.Fatal("paused answer was not retained for explicit continuation")
				}
			} else if amendments != 0 {
				t.Fatal("late answer created executable requirements")
			}
			var answered time.Time
			var answer string
			if err := s.db.QueryRow(`select answer,answered_at from supervision_questions where id='versioned'`).Scan(&answer, &answered); err != nil || answer != "确认" {
				t.Fatal("answer history lost", err)
			}
		})
	}
}

func TestSupervisionNewRequirementsExpireQuestionsWithoutOverridingPause(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint(paused), func(t *testing.T) {
			s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			ctx := context.Background()
			startSupervisionForTest(t, s, ss.ID, now)
			state, control, reason := "needs_input", "", "user_question"
			if paused {
				state, control, reason = "paused", "pause", "user_pause"
			}
			if _, err := s.db.Exec(`update supervision_sessions set state=?,control=?,reason=? where id=?;insert into supervision_questions(id,session_id,task_id,question,created_at) values ('obsolete',?,'','原问题',?);insert into supervision_amendments(id,session_id,content,state,created_at) values ('replacement',?,'根据新要求执行','pending',?)`, state, control, reason, ss.ID, ss.ID, now, ss.ID, now); err != nil {
				t.Fatal(err)
			}
			if err := s.applySupervisionAmendments(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			var expired bool
			if err := s.db.QueryRow(`select expired from supervision_questions where id='obsolete'`).Scan(&expired); err != nil || !expired {
				t.Fatalf("old question still blocks: %v %v", expired, err)
			}
			got := supervisionSessionForTest(t, s, ss.ID)
			if paused {
				if got.State != "paused" || got.Control != "pause" {
					t.Fatal("new requirements overrode user pause")
				}
			} else if got.State != "running" || got.Reason != "" {
				t.Fatalf("expired global question still stalls scheduler: %+v", got)
			}
		})
	}
}
