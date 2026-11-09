package targetexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestEventPageValidationRejectsGapsAlterationAndMissingTail(t *testing.T) {
	s, control, spec, now := fixture(t)
	if _, err := s.Prepare(spec, now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, fmt.Sprintf("frame-%d", i), "assistant", json.RawMessage(`{"text":"same"}`)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.EventsAfter(control.SessionID, 0, 2)
	if err != nil || ValidateEventPage(page, 0, 2) != nil {
		t.Fatal(page, err)
	}
	for _, kind := range []string{"gap", "checksum", "tail", "duplicate", "empty", "future"} {
		copy := page
		copy.Events = append([]Event(nil), page.Events...)
		switch kind {
		case "gap":
			copy.Events[0].Sequence++
		case "checksum":
			copy.Events[0].Payload = json.RawMessage(`{"text":"changed"}`)
		case "tail":
			copy.More = false
		case "duplicate":
			copy.Events[1] = copy.Events[0]
			copy.Events[1].Sequence = 2
		case "empty":
			copy.Events = nil
		case "future":
			copy.Events[1].Sequence = copy.NextSequence
		}
		if err := ValidateEventPage(copy, 0, 2); !errors.Is(err, ErrProtocol) {
			t.Fatal("invalid page accepted", kind, err)
		}
	}
	if err := ValidateEventPage(page, 4, 2); !errors.Is(err, ErrCursor) {
		t.Fatal("unavailable cursor accepted", err)
	}
}

func TestEventJournalSurvivesRestartAndRejectsReplaySubstitution(t *testing.T) {
	s, control, spec, now := fixture(t)
	if _, err := s.Prepare(spec, now); err != nil {
		t.Fatal(err)
	}
	first, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-1", "assistant", json.RawMessage(`{"text":"hello"}`))
	if err != nil || first.Sequence != 1 {
		t.Fatal(first, err)
	}
	first.Payload[0] = 'x'
	if repeated, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-1", "assistant", json.RawMessage(`{"text":"hello"}`)); err != nil || repeated.Sequence != 1 || !json.Valid(repeated.Payload) {
		t.Fatal(repeated, err)
	}
	if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-1", "result", json.RawMessage(`{"text":"hello"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("event kind was substituted", err)
	}
	if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-2", "assistant", json.RawMessage(`{"text":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.EventsAfter(control.SessionID, 0, 1)
	if err != nil || len(page.Events) != 1 || !page.More || page.NextSequence != 3 {
		t.Fatal(page, err)
	}
	page.Events[0].Payload[0] = 'x'
	page, err = reopened.EventsAfter(control.SessionID, 1, 100)
	if err != nil || len(page.Events) != 1 || page.More || page.Events[0].Sequence != 2 {
		t.Fatal(page, err)
	}
}

func TestEventJournalPruningReportsExplicitGapAndRetainsDeduplication(t *testing.T) {
	s, control, spec, now := fixture(t)
	if _, err := s.Prepare(spec, now); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 240<<10) + `"}`)
	for i := 1; i <= 20; i++ {
		if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, fmt.Sprintf("frame-%d", i), "assistant", payload); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.EventsAfter(control.SessionID, 0, 100)
	if !errors.Is(err, ErrCursor) || page.FirstSequence <= 1 || page.NextSequence != 21 {
		t.Fatal("pruned prefix presented as complete", page, err)
	}
	if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-1", "assistant", payload); !errors.Is(err, ErrCursor) {
		t.Fatal("pruned event replayed", err)
	}
	page, err = s.EventsAfter(control.SessionID, page.FirstSequence-1, 100)
	if err != nil || len(page.Events) != 1 || !page.More {
		t.Fatal("page byte budget not enforced", page, err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.EventsAfter(control.SessionID, 21, 10); !errors.Is(err, ErrCursor) {
		t.Fatal("future cursor accepted", err)
	}
}

func TestEventJournalAccountsForDurableJSONEncodingAcrossRestart(t *testing.T) {
	s, control, spec, now := fixture(t)
	if _, err := s.Prepare(spec, now); err != nil {
		t.Fatal(err)
	}
	event, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-spaced", "assistant", json.RawMessage(" { \"text\" : \"<tag>\" } \n"))
	if err != nil || string(event.Payload) != `{"text":"\u003ctag\u003e"}` {
		t.Fatal("not measured as durable bytes", string(event.Payload), err)
	}
	if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "frame-expanded", "assistant", json.RawMessage(`{"text":"`+strings.Repeat("<", maxEventBytes/2)+`"}`)); !errors.Is(err, ErrLimit) {
		t.Fatal("unreadable escaped event entered journal", err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal("valid event no longer readable after JSON compaction", err)
	}
	defer reopened.Close()
	page, err := reopened.EventsAfter(control.SessionID, 0, 100)
	if err != nil || len(page.Events) != 1 || page.NextSequence != 2 || string(page.Events[0].Payload) != string(event.Payload) {
		t.Fatal("event replay changed after restart", page, err)
	}
}
