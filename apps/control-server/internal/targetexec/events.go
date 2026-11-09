package targetexec

import (
	"encoding/json"
	"errors"
)

const (
	maxEventBytes     = 1 << 20
	maxEventPageBytes = 256 << 10
	maxEventLogBytes  = 4 << 20
	maxEventKeys      = 10000
)

// ValidateEventPage checks the complete page before a controller commits any
// transcript or advances its durable cursor. Sequence continuity is separate
// from the adapter's idempotency key: the latter deliberately excludes sequence.
func ValidateEventPage(page EventPage, after int64, limit int) error {
	if after < 0 || limit < 1 || limit > 100 || page.FirstSequence < 1 || page.NextSequence < page.FirstSequence || len(page.Events) > limit {
		return ErrProtocol
	}
	if after < page.FirstSequence-1 || after >= page.NextSequence {
		return ErrCursor
	}
	next := after + 1
	keys := map[string]bool{}
	for _, event := range page.Events {
		if event.Sequence != next || event.Sequence >= page.NextSequence || !validID(event.ExecutionID) || !validID(event.Key) || !validID(event.Type) || !json.Valid(event.Payload) || len(event.Payload) > maxEventBytes || keys[event.Key] {
			return ErrProtocol
		}
		copy := event
		copy.Sequence, copy.SHA256 = 0, ""
		raw, _ := json.Marshal(copy)
		if hashBytes(raw) != event.SHA256 {
			return ErrProtocol
		}
		keys[event.Key] = true
		next++
	}
	if page.More != (next < page.NextSequence) || page.More && len(page.Events) == 0 {
		return ErrProtocol
	}
	return nil
}

// AppendEvent uses a durable adapter event key, not text equality. A retry of
// the same frame is idempotent, while identical text in two real frames remains
// two events. Keys survive pruning, so reconnects cannot resurrect old output.
func (s *Store) AppendEvent(sessionID, executionID, key, kind string, payload json.RawMessage) (Event, error) {
	if !validID(key) || !validID(kind) || !json.Valid(payload) {
		return Event{}, ErrConflict
	}
	if len(payload) > maxEventBytes {
		return Event{}, ErrLimit
	}
	// Measure the exact JSON that persistence and framing use. Whitespace and
	// HTML escaping otherwise change byte accounting after reopening the log,
	// and an apparently bounded payload can expand beyond a transport frame.
	canonical, err := json.Marshal(payload)
	if err != nil {
		return Event{}, ErrConflict
	}
	if len(canonical) > maxEventBytes {
		return Event{}, ErrLimit
	}
	payload = canonical
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Event{}, ErrClosed
	}
	session, exists := s.facts.Sessions[sessionID]
	if _, ok := session.Executions[executionID]; !exists || !ok {
		return Event{}, ErrConflict
	}
	if s.facts.Events == nil {
		s.facts.Events = map[string]EventLog{}
	}
	log := s.facts.Events[sessionID]
	if log.Keys == nil {
		log.Keys = map[string]string{}
		log.NextSequence = 1
		log.FirstSequence = 1
	}
	// Bind the key to all semantic fields, not only payload bytes.
	event := Event{ExecutionID: executionID, Key: key, Type: kind, Payload: append(json.RawMessage(nil), payload...)}
	raw, _ := json.Marshal(event)
	digest := hashBytes(raw)
	if prior, ok := log.Keys[key]; ok {
		if prior != digest {
			return Event{}, ErrConflict
		}
		for _, item := range log.Events {
			if item.Key == key {
				return cloneEvent(item), nil
			}
		}
		return Event{}, ErrCursor
	}
	if session.Released || len(log.Keys) >= maxEventKeys {
		return Event{}, errors.New("target event journal is closed or full")
	}
	event.Sequence, event.SHA256 = log.NextSequence, digest
	log.NextSequence++
	log.Keys[key] = digest
	log.Events = append(log.Events, event)
	log.Bytes += int64(len(payload))
	for log.Bytes > maxEventLogBytes && len(log.Events) > 1 {
		log.Bytes -= int64(len(log.Events[0].Payload))
		log.Events = log.Events[1:]
	}
	log.FirstSequence = log.Events[0].Sequence
	s.facts.Events[sessionID] = log
	if err := s.save(); err != nil {
		return Event{}, err
	}
	return cloneEvent(event), nil
}

func cloneEvent(event Event) Event {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	return event
}

// Event cleanup is explicit, after the controller has consumed the result and
// released the session. Keep the watermark so old cursors report a gap, and
// retain execution/control facts so late starts cannot replay cleared history.
func (s *Store) ClearReleasedEvents(sessionID string, epoch, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	if !ok || !session.Released || session.Control.State != "stopped" || session.Control.OwnerEpoch != epoch || session.Control.Version != version {
		return ErrAuthority
	}
	log, exists := s.facts.Events[sessionID]
	if !exists || len(log.Keys) == 0 && len(log.Events) == 0 {
		return nil
	}
	log.Events, log.Keys, log.Bytes = []Event{}, map[string]string{}, 0
	log.FirstSequence = log.NextSequence
	s.facts.Events[sessionID] = log
	return s.saveControl()
}

// EventsAfter returns an explicit cursor error after pruning. A caller must not
// interpret a missing prefix as a complete transcript or acceptance evidence.
func (s *Store) EventsAfter(sessionID string, after int64, limit int) (EventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return EventPage{}, ErrClosed
	}
	if _, ok := s.facts.Sessions[sessionID]; !ok {
		return EventPage{}, ErrConflict
	}
	if after < 0 || limit < 1 || limit > 100 {
		return EventPage{}, ErrConflict
	}
	log, exists := s.facts.Events[sessionID]
	if !exists {
		if after != 0 {
			return EventPage{}, ErrCursor
		}
		return EventPage{Events: []Event{}, FirstSequence: 1, NextSequence: 1}, nil
	}
	page := EventPage{Events: []Event{}, FirstSequence: log.FirstSequence, NextSequence: log.NextSequence}
	if after < log.FirstSequence-1 || after >= log.NextSequence {
		return page, ErrCursor
	}
	bytes := 0
	for _, event := range log.Events {
		if event.Sequence <= after {
			continue
		}
		if len(page.Events) >= limit || bytes+len(event.Payload) > maxEventPageBytes && len(page.Events) > 0 {
			page.More = true
			break
		}
		page.Events = append(page.Events, cloneEvent(event))
		bytes += len(event.Payload)
	}
	return page, nil
}

func validateEventLogs(facts Facts) error {
	for sessionID, log := range facts.Events {
		session, ok := facts.Sessions[sessionID]
		if !ok || log.Keys == nil || len(log.Keys) > maxEventKeys || log.NextSequence < 1 || log.FirstSequence < 1 || log.FirstSequence > log.NextSequence {
			return ErrProtocol
		}
		var bytes int64
		next := log.FirstSequence
		for _, event := range log.Events {
			if _, ok := session.Executions[event.ExecutionID]; !ok {
				return ErrProtocol
			}
			if event.Sequence != next || !validID(event.Key) || !validID(event.Type) || !json.Valid(event.Payload) || len(event.Payload) > maxEventBytes {
				return ErrProtocol
			}
			copy := event
			copy.Sequence = 0
			copy.SHA256 = ""
			raw, _ := json.Marshal(copy)
			if hashBytes(raw) != event.SHA256 || log.Keys[event.Key] != event.SHA256 {
				return ErrProtocol
			}
			bytes += int64(len(event.Payload))
			next++
		}
		if bytes != log.Bytes || bytes > maxEventLogBytes || next != log.NextSequence {
			return ErrProtocol
		}
		for key, digest := range log.Keys {
			if !validID(key) || !validHash(digest) {
				return ErrProtocol
			}
		}
	}
	return nil
}
