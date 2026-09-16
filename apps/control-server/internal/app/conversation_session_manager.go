package app

import (
	"errors"
	"sort"
	"time"
)

// conversationSessionManager bounds native streaming processes without
// changing the durable conversation history. Server.mu protects every field it
// inspects; callers that change admission state must also hold streamMu first.
type conversationSessionManager struct {
	server *Server
	now    func() time.Time
}

// conversationSessionConfig contains every launch-time input that cannot be
// changed inside an already-running native CLI process.
type conversationSessionConfig struct {
	runnerID          string
	agentID           string
	projectPath       string
	permissionMode    string
	profileRevisionID string
	// model 是进程启动参数里的 --model / -c model=。它一旦变化，长驻进程必须
	// 退役重启（下一条消息带新模型 --resume），否则会话会一直用旧模型。
	model string
}

func newConversationSessionConfig(runnerID string, conversation Conversation, profile *AgentRuntimeProfile, projectPath string) conversationSessionConfig {
	profileRevisionID := conversation.AgentProfileRevisionID
	if profile != nil {
		profileRevisionID = profile.RevisionID
	}
	return conversationSessionConfig{
		runnerID:          runnerID,
		agentID:           conversation.AgentID,
		projectPath:       projectPath,
		permissionMode:    conversation.executionPolicy(),
		profileRevisionID: profileRevisionID,
		model:             runModel(conversation.ModelOverride, profile),
	}
}

func newConversationSessionManager(server *Server) *conversationSessionManager {
	return &conversationSessionManager{server: server, now: func() time.Time { return time.Now().UTC() }}
}

func (m *conversationSessionManager) idleTTL() time.Duration {
	if m.server.config.ConversationSessionIdleTTL <= 0 {
		return defaultConversationSessionIdleTTL
	}
	return m.server.config.ConversationSessionIdleTTL
}

func (m *conversationSessionManager) sessionsPerRunner() int {
	if m.server.config.ConversationSessionsPerRunner <= 0 {
		return defaultConversationSessionsPerRunner
	}
	return m.server.config.ConversationSessionsPerRunner
}

// exitGrace 是会话被要求停止之后，仍愿意等它真正退出的上限。
func (m *conversationSessionManager) exitGrace() time.Duration {
	if m.server.config.ConversationSessionExitGrace <= 0 {
		return defaultConversationSessionExitGrace
	}
	return m.server.config.ConversationSessionExitGrace
}

func (m *conversationSessionManager) start() {
	interval := m.idleTTL() / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	m.server.runWG.Add(1)
	go func() {
		defer m.server.runWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.server.runtimeCtx.Done():
				return
			case <-ticker.C:
				m.reapExpired()
			}
		}
	}()
}

// ensureCapacity is called while streamMu is held, before a new Run is
// committed. It never removes a Session directly: watcher ownership keeps map
// deletion and Run cleanup coupled to AgentSession.Done().
func (m *conversationSessionManager) ensureCapacity(conversationID, runnerID string) error {
	s := m.server
	s.mu.Lock()
	if existing := s.sessions[conversationID]; existing != nil {
		stopping := existing.stopping
		stoppingSince := existing.stoppingSince
		s.mu.Unlock()
		if stopping {
			logStoppingRejection("conversation admission", conversationID, stoppingSince)
			return errors.New("conversation is stopping")
		}
		return nil
	}

	count := 0
	stopping := false
	for _, session := range s.sessions {
		if session.runnerID == runnerID {
			count++
			stopping = stopping || session.stopping
		}
	}
	if count < m.sessionsPerRunner() {
		s.mu.Unlock()
		return nil
	}
	if stopping {
		s.mu.Unlock()
		return errors.New("agent session capacity is full while a previous session is stopping")
	}

	candidates := m.idleCandidatesLocked(runnerID)
	if len(candidates) == 0 {
		s.mu.Unlock()
		return errors.New("agent session capacity is full; all sessions are running or awaiting approval")
	}
	agents := m.markStoppingLocked(candidates[:1])
	s.mu.Unlock()
	m.stopAsync(agents)
	return errors.New("agent session capacity is being reclaimed; retry shortly")
}

// ensureConfiguration rejects reuse of a native process started with a stale
// workspace, runner, policy, or profile revision. A process is only retired
// when it is fully idle; active work must be stopped through the normal Run
// lifecycle before the caller retries with the new configuration.
func (m *conversationSessionManager) ensureConfiguration(conversationID string, desired conversationSessionConfig) error {
	s := m.server
	s.mu.Lock()
	session := s.sessions[conversationID]
	if session == nil || !session.configSet || session.config == desired {
		s.mu.Unlock()
		return nil
	}
	if session.stopping {
		s.mu.Unlock()
		return errors.New("conversation is stopping for a configuration change")
	}
	if !m.idleLocked(conversationID, session) {
		s.mu.Unlock()
		return errors.New("conversation configuration changed while a native agent session is active; stop it before retrying")
	}
	session.markStopping()
	agent := session.agent
	s.mu.Unlock()
	go agent.Stop()
	return errors.New("native agent session is stopping for a configuration change; retry shortly")
}

func (m *conversationSessionManager) reapExpired() {
	now := m.now()
	s := m.server
	s.streamMu.Lock()
	s.mu.Lock()
	candidates := make([]sessionCandidate, 0)
	for conversationID, session := range s.sessions {
		if !m.idleLocked(conversationID, session) || now.Sub(session.lastUsedAt) < m.idleTTL() {
			continue
		}
		candidates = append(candidates, sessionCandidate{conversationID: conversationID, session: session})
	}
	agents := m.markStoppingLocked(candidates)
	s.mu.Unlock()
	s.streamMu.Unlock()
	m.stopAsync(agents)
}

func (m *conversationSessionManager) noteActivity(session *activeAgentSession) {
	if session != nil {
		session.lastUsedAt = m.now()
	}
}

// retireForConfiguration prevents a new turn from joining a Session whose
// launch-time configuration is about to change. The caller retries its durable
// update after watcher cleanup confirms the native process has exited.
func (m *conversationSessionManager) retireForConfiguration(conversationID string) bool {
	s := m.server
	s.streamMu.Lock()
	s.mu.Lock()
	session := s.sessions[conversationID]
	if session == nil {
		s.mu.Unlock()
		s.streamMu.Unlock()
		return true
	}
	if session.stopping || !m.idleLocked(conversationID, session) {
		s.mu.Unlock()
		s.streamMu.Unlock()
		return false
	}
	session.markStopping()
	agent := session.agent
	s.mu.Unlock()
	s.streamMu.Unlock()
	go agent.Stop()
	return false
}

// retireForProfileRevision stops every native Session bound to a revoked
// profile revision. It intentionally includes active Sessions: the caller has
// already cancelled their Run contexts, and no future turn may retain revoked
// credentials in a long-lived child process.
func (m *conversationSessionManager) retireForProfileRevision(conversationIDs map[string]struct{}) int {
	if len(conversationIDs) == 0 {
		return 0
	}
	s := m.server
	s.streamMu.Lock()
	s.mu.Lock()
	agents := make([]AgentSession, 0)
	for conversationID, session := range s.sessions {
		if _, selected := conversationIDs[conversationID]; !selected || session.stopping {
			continue
		}
		session.markStopping()
		agents = append(agents, session.agent)
	}
	s.mu.Unlock()
	s.streamMu.Unlock()
	m.stopAsync(agents)
	return len(agents)
}

type sessionCandidate struct {
	conversationID string
	session        *activeAgentSession
}

func (m *conversationSessionManager) idleCandidatesLocked(runnerID string) []sessionCandidate {
	candidates := make([]sessionCandidate, 0)
	for conversationID, session := range m.server.sessions {
		if session.runnerID == runnerID && m.idleLocked(conversationID, session) {
			candidates = append(candidates, sessionCandidate{conversationID: conversationID, session: session})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.session.lastUsedAt.Equal(right.session.lastUsedAt) {
			return left.conversationID < right.conversationID
		}
		return left.session.lastUsedAt.Before(right.session.lastUsedAt)
	})
	return candidates
}

func (m *conversationSessionManager) idleLocked(conversationID string, session *activeAgentSession) bool {
	if session.stopping || session.activeRunID != "" || len(session.runIDs) != 0 {
		return false
	}
	for _, approval := range m.server.approvals {
		if approval.conversationID == conversationID {
			return false
		}
	}
	return true
}

func (m *conversationSessionManager) markStoppingLocked(candidates []sessionCandidate) []AgentSession {
	agents := make([]AgentSession, 0, len(candidates))
	for _, candidate := range candidates {
		current := m.server.sessions[candidate.conversationID]
		if current != candidate.session || !m.idleLocked(candidate.conversationID, current) {
			continue
		}
		current.markStopping()
		agents = append(agents, current.agent)
	}
	return agents
}

func (m *conversationSessionManager) stopAsync(agents []AgentSession) {
	for _, agent := range agents {
		go agent.Stop()
	}
}

func sessionRunnerID(conversation Conversation) string {
	if conversation.AgentRuntimeID != "" {
		return conversation.AgentRuntimeID
	}
	return conversation.AgentID
}
