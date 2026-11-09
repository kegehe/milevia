package targetexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const maxPacketBytes = 2 << 20

// Transport is a single authenticated owner channel (local child pipes or SSH
// exec). It deliberately exposes no TCP listener and accepts no shell commands.
// Losing the channel revokes grants; durable identities survive reconnection.
type Transport struct {
	// A restricted endpoint has no execution store/supervisor and dispatches
	// only its own extension methods. It cannot acquire execution authority.
	EndpointIdentity string
	Store            *Store
	Supervisor       *Supervisor
	// Optional target filesystem/CLI handlers. The transport validates version
	// and physical target binding before reaching either callback.
	Extension           func(context.Context, Request) (json.RawMessage, error)
	ValidateControl     func(Control) error
	ValidateCredentials func(ExecutionSpec, *RuntimeCredentials) error
	AfterRelease        func(Control) error
	Diagnostic          func(error) string
}

type Request struct {
	Protocol             int                 `json:"protocol"`
	ID                   string              `json:"id"`
	Method               string              `json:"method"`
	TargetID             string              `json:"targetId,omitempty"`
	SessionID            string              `json:"sessionId,omitempty"`
	ExecutionID          string              `json:"executionId,omitempty"`
	Control              *Control            `json:"control,omitempty"`
	Spec                 *ExecutionSpec      `json:"spec,omitempty"`
	Input                *Input              `json:"input,omitempty"`
	Credentials          *RuntimeCredentials `json:"credentials,omitempty"`
	OwnerEpoch           int64               `json:"ownerEpoch,omitempty"`
	AuthorizationVersion int64               `json:"authorizationVersion,omitempty"`
	Until                time.Time           `json:"until,omitempty"`
	After                int64               `json:"after,omitempty"`
	Limit                int                 `json:"limit,omitempty"`
	AgentID              string              `json:"agentId,omitempty"`
	Path                 string              `json:"path,omitempty"`
	WorkspaceID          string              `json:"workspaceId,omitempty"`
	MutationID           string              `json:"mutationId,omitempty"`
	Parameters           json.RawMessage     `json:"parameters,omitempty"`
}

type Response struct {
	Protocol   int             `json:"protocol"`
	ID         string          `json:"id"`
	TargetID   string          `json:"targetId,omitempty"`
	TargetTime time.Time       `json:"targetTime"`
	Error      string          `json:"error,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Session    *Session        `json:"session,omitempty"`
	Execution  *Execution      `json:"execution,omitempty"`
	Events     *EventPage      `json:"events,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

func wireError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrProtocol):
		return "protocol"
	case errors.Is(err, ErrAuthority):
		return "authority"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrNotQuiet):
		return "not_quiet"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.Is(err, ErrCursor):
		return "cursor_unavailable"
	case errors.Is(err, ErrLimit):
		return "payload_too_large"
	default:
		return "target_failure"
	}
}

func responseError(code string) error {
	switch code {
	case "":
		return nil
	case "protocol":
		return ErrProtocol
	case "authority":
		return ErrAuthority
	case "conflict":
		return ErrConflict
	case "not_quiet":
		return ErrNotQuiet
	case "closed":
		return ErrClosed
	case "cursor_unavailable":
		return ErrCursor
	case "payload_too_large":
		return ErrLimit
	default:
		return errors.New("target execution operation failed")
	}
}

func strictPacket(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrProtocol
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return ErrProtocol
	}
	return nil
}

func (t *Transport) handle(ctx context.Context, request Request) Response {
	response := Response{Protocol: ProtocolVersion, ID: request.ID, TargetTime: time.Now().UTC()}
	if request.Protocol != ProtocolVersion || !validID(request.ID) {
		response.Error = "protocol"
		return response
	}
	targetID := t.EndpointIdentity
	var err error
	if t.Store != nil {
		targetID, err = t.Store.Identity()
	}
	if err != nil {
		response.Error = wireError(err)
		return response
	}
	response.TargetID = targetID
	if request.Method == "hello" {
		return response
	}
	if request.TargetID != targetID {
		response.Error = "authority"
		return response
	}
	if t.Store == nil {
		response.Data, err = t.Extension(ctx, request)
		response.Error = wireError(err)
		return response
	}
	now := time.Now().UTC() // All grant/deadline checks use the target clock.
	switch request.Method {
	case "control":
		if request.Control == nil || request.Control.TargetID != request.TargetID || request.Control.SessionID != request.SessionID {
			err = ErrConflict
			break
		}
		if t.ValidateControl != nil {
			if err = t.ValidateControl(*request.Control); err != nil {
				break
			}
		}
		err = t.Supervisor.ApplyControl(*request.Control, now)
	case "start":
		if request.Spec == nil || request.Input == nil || request.Spec.TargetID != request.TargetID || request.Spec.SessionID != request.SessionID || request.Spec.ExecutionID != request.ExecutionID {
			err = ErrConflict
			break
		}
		if t.ValidateCredentials != nil {
			if err = t.ValidateCredentials(*request.Spec, request.Credentials); err != nil {
				break
			}
		} else if request.Credentials != nil || request.Spec.ProfileRevisionID != "" || request.Spec.ProfileSHA256 != "" {
			err = ErrProtocol
			break
		}
		var execution Execution
		execution, err = t.Supervisor.StartWithCredentials(*request.Spec, *request.Input, request.Credentials, now)
		if err == nil {
			response.Execution = &execution
		}
	case "query":
		session, queryErr := t.Store.Session(request.SessionID)
		if queryErr != nil {
			err = queryErr
			break
		}
		if request.ExecutionID != "" {
			execution, ok := session.Executions[request.ExecutionID]
			if !ok {
				err = ErrConflict
				break
			}
			response.Execution = &execution
		} else {
			response.Session = &session
		}
	case "renew":
		err = t.Store.RenewAuthorization(request.SessionID, request.OwnerEpoch, request.AuthorizationVersion, request.Until, now)
		if err == nil && request.ExecutionID != "" {
			err = t.Store.Renew(request.SessionID, request.ExecutionID, request.OwnerEpoch, request.AuthorizationVersion, request.Until, now)
		}
	case "release":
		err = t.Store.Release(request.SessionID, request.OwnerEpoch, request.AuthorizationVersion)
		if err == nil && t.AfterRelease != nil {
			var session Session
			session, err = t.Store.Session(request.SessionID)
			if err == nil {
				err = t.AfterRelease(session.Control)
			}
		}
	case "events":
		var page EventPage
		page, err = t.Store.EventsAfter(request.SessionID, request.After, request.Limit)
		response.Events = &page // A cursor gap still returns the retained watermark.
	default:
		if t.Extension == nil {
			err = ErrProtocol
		} else {
			response.Data, err = t.Extension(ctx, request)
		}
	}
	response.Error = wireError(err)
	if err != nil && t.Diagnostic != nil {
		reason := t.Diagnostic(err)
		if len(reason) > 2048 {
			reason = reason[:2048]
		}
		response.Reason = reason
	}
	return response
}

// Serve owns the supervisor for this connection. A write failure, EOF, invalid
// frame or cancellation follows the same revoke/stop/recovery path.
func (t *Transport) Serve(ctx context.Context, channel io.ReadWriteCloser) (resultErr error) {
	if channel == nil || (t.Store == nil || t.Supervisor == nil) && (t.Store != nil || t.Supervisor != nil || t.EndpointIdentity == "" || t.Extension == nil) {
		return ErrClosed
	}
	if t.Supervisor != nil {
		defer func() { resultErr = errors.Join(resultErr, t.Supervisor.Close()) }()
	}
	defer channel.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			channel.Close()
		case <-done:
		}
	}()
	scanner := bufio.NewScanner(channel)
	scanner.Buffer(make([]byte, 64<<10), maxPacketBytes)
	encoder := json.NewEncoder(channel)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var request Request
		if strictPacket(scanner.Bytes(), &request) != nil {
			return ErrProtocol
		}
		response := t.handle(ctx, request)
		raw, err := json.Marshal(response)
		if err != nil {
			return ErrProtocol
		}
		if len(raw) >= maxPacketBytes {
			// A valid but large observation is not a broken owner channel. Keep
			// the connection and its revocation path usable; the controller can
			// query a specific execution or request smaller source/event pages.
			response = Response{Protocol: ProtocolVersion, ID: request.ID, TargetID: response.TargetID, TargetTime: response.TargetTime, Error: wireError(ErrLimit)}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return scanner.Err()
}

type Client struct {
	mu           sync.Mutex
	clockMu      sync.Mutex
	callGate     chan struct{}
	channel      io.ReadWriteCloser
	scanner      *bufio.Scanner
	targetID     string
	closed       bool
	disconnected atomic.Bool
	clockSample  time.Time
	clockSent    time.Time
}

func NewClient(channel io.ReadWriteCloser) *Client {
	scanner := bufio.NewScanner(channel)
	scanner.Buffer(make([]byte, 64<<10), maxPacketBytes)
	return &Client{channel: channel, scanner: scanner, callGate: make(chan struct{}, 1)}
}

// IsClosed never waits for a blocked response. A connection pool can retire a
// failed owner before opening a replacement without probing or replaying work.
func (c *Client) IsClosed() bool { return c.disconnected.Load() }

// Call serializes framing. Cancellation closes the owner channel rather than
// leaving a read goroutine that might consume a later response or retry a start.
// The caller queries the original execution ID after reconnecting.
func (c *Client) Call(ctx context.Context, request Request) (Response, error) {
	// Cancellation while queued must not wait on another operation's response,
	// and must not close that operation's owner stream.
	select {
	case c.callGate <- struct{}{}:
		defer func() { <-c.callGate }()
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Response{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	request.Protocol = ProtocolVersion
	if !validID(request.ID) {
		return Response{}, ErrConflict
	}
	if request.Method != "hello" {
		if c.targetID == "" || request.TargetID != "" && request.TargetID != c.targetID {
			return Response{}, ErrAuthority
		}
		request.TargetID = c.targetID
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) >= maxPacketBytes {
		return Response{}, ErrConflict
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			c.channel.Close()
		case <-done:
		}
	}()
	defer func() { close(done); <-stopped }()
	fail := func(err error) (Response, error) {
		c.closed = true
		c.disconnected.Store(true)
		c.channel.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return Response{}, err
	}
	sent := time.Now()
	if _, err := c.channel.Write(append(raw, '\n')); err != nil {
		return fail(err)
	}
	if !c.scanner.Scan() {
		err := c.scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fail(err)
	}
	var response Response
	if strictPacket(c.scanner.Bytes(), &response) != nil || response.Protocol != ProtocolVersion || response.ID != request.ID || !validID(response.TargetID) || response.TargetTime.IsZero() || c.targetID != "" && c.targetID != response.TargetID {
		return fail(ErrProtocol)
	}
	if request.Method == "hello" && response.Error == "" {
		c.targetID = response.TargetID
	}
	c.clockMu.Lock()
	c.clockSample, c.clockSent = response.TargetTime, sent
	c.clockMu.Unlock()
	err = responseError(response.Error)
	if request.Method == "events" && err == nil {
		if response.Events == nil {
			return fail(ErrProtocol)
		}
		if err := ValidateEventPage(*response.Events, request.After, request.Limit); err != nil {
			return fail(err)
		}
	}
	if err != nil && response.Reason != "" {
		err = fmt.Errorf("%w: %s", err, response.Reason)
	}
	return response, err
}

// TargetDeadline uses the target's clock, subtracting the entire sample age
// (including the round trip) conservatively. Controller wall-clock skew never
// enlarges a grant. An old sample must be refreshed instead of extending a lease.
func (c *Client) TargetDeadline(ttl time.Duration) (time.Time, error) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	if c.IsClosed() {
		return time.Time{}, ErrClosed
	}
	if c.clockSample.IsZero() || ttl <= 0 || ttl > 30*time.Second {
		return time.Time{}, ErrAuthority
	}
	remaining := ttl - time.Since(c.clockSent)
	if remaining <= 0 {
		return time.Time{}, ErrAuthority
	}
	return c.clockSample.Add(remaining), nil
}

// TargetExecutionWindow maps the remaining portion of an already-started
// controller turn to the target clock. Unlike a grant, a turn can be longer
// than 30 seconds; callers must supply its remaining budget, never a fresh full
// budget on reconnect. A fresh sample is required independently of that length.
func (c *Client) TargetExecutionWindow(remaining time.Duration) (time.Time, time.Time, error) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	if c.IsClosed() {
		return time.Time{}, time.Time{}, ErrClosed
	}
	age := time.Since(c.clockSent)
	if c.clockSample.IsZero() || age < 0 || age >= 30*time.Second || remaining <= age || remaining > 30*time.Minute {
		return time.Time{}, time.Time{}, ErrAuthority
	}
	return c.clockSample, c.clockSample.Add(remaining - age), nil
}

func (c *Client) Close() error {
	// Close must interrupt an in-flight Call, which holds mu while reading.
	c.disconnected.Store(true)
	err := c.channel.Close()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return err
}

func (c *Client) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("targetexec(%s)", c.targetID)
}
