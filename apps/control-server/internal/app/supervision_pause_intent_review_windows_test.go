//go:build windows

package app

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func automaticPauseForIntentReviewTest(t *testing.T, reason string, settled bool) (*Server, supervisionSession) {
	t.Helper()
	ctx := context.Background()
	var s *Server
	var ss supervisionSession
	if reason == "budget_exhausted" {
		s, _, ss = finalReserveForBudgetReviewTest(t)
		if err := s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			return pauseSupervisionForBudgetTx(ctx, tx, current, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		s, _, _, ss, _ = multiWorkflowFixtureForTest(t, defaultSupervisionBudget(), false)
		response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/instructions", "interrupt", fmt.Sprintf(`{"stateVersion":%d,"content":"补充要求，在当前回合结束后处理","immediate":true}`, ss.StateVersion))
		if response.Code != 202 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if settled {
		if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.Control != "pause" || ss.Reason != reason {
		t.Fatal("fixture did not enter the expected automatic pause", ss)
	}
	return s, ss
}

func TestSupervisionExplicitPauseSupersedesAutomaticPauseIntent(t *testing.T) {
	for _, reason := range []string{"amendment_interrupt", "budget_exhausted"} {
		for _, settled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/settled=%v", reason, settled), func(t *testing.T) {
				s, ss := automaticPauseForIntentReviewTest(t, reason, settled)
				path := "/api/supervision/" + ss.ID + "/control"
				body := fmt.Sprintf(`{"stateVersion":%d,"action":"pause"}`, ss.StateVersion)
				response := supervisionRequestForTest(s, "POST", path, "explicit-pause", body)
				if response.Code != 202 {
					t.Fatal(response.Code, response.Body.String())
				}
				current, err := supervisionWorkerForTest(s, nil).maintain(context.Background(), ss.ID)
				if err != nil || current.State != "paused" || current.Control != "pause" || current.Reason != "user_pause" {
					t.Fatal("automatic maintenance ignored the explicit user pause", current, err)
				}
				// Both a replay and a new retry key keep the established user
				// intent without requiring the now-outdated original version.
				for _, key := range []string{"explicit-pause", "pause-retry"} {
					response = supervisionRequestForTest(s, "POST", path, key, body)
					if response.Code != 202 {
						t.Fatal(response.Code, response.Body.String())
					}
				}
				after := supervisionSessionForTest(t, s, ss.ID)
				if after.StateVersion != current.StateVersion || after.Reason != "user_pause" {
					t.Fatal("retry changed an established user pause", current, after)
				}
			})
		}
	}
}

func TestSupervisionStaleExplicitPauseCannotFalselyAcknowledgeAutomaticPause(t *testing.T) {
	for _, reason := range []string{"amendment_interrupt", "budget_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			s, ss := automaticPauseForIntentReviewTest(t, reason, false)
			response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/control", "stale-pause", fmt.Sprintf(`{"stateVersion":%d,"action":"pause"}`, ss.StateVersion-1))
			if response.Code != 409 {
				t.Fatal("stale pause was acknowledged without persisting a user pause", response.Code, response.Body.String())
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			if current.StateVersion != ss.StateVersion || current.Reason != reason {
				t.Fatal("rejected control request changed the automatic pause", current)
			}
		})
	}
}
