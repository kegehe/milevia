# Streaming Stop Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent stopping one streaming run from terminating a run admitted concurrently to the same conversation.

**Architecture:** Use the existing streaming mutex as the session lifecycle guard. Keep admission, isolation validation, transition to `stopping`, and `AgentSession.Stop` mutually exclusive for a streaming session. A submission arriving during stop cannot enter the old session; it may be rejected while asynchronous session cleanup is still in progress and can then be retried.

**Tech Stack:** Go, `net/http`, SQLite, existing `runnerFunc` and streaming-session test infrastructure.

## Global Constraints

- Preserve manual task dispatch and existing session queue behavior.
- Do not commit changes.
- Add a regression test before changing production code.

---

### Task 1: Make streaming stop admission atomic

**Files:**
- Modify: `apps/control-server/internal/app/app_test.go`
- Modify: `apps/control-server/internal/app/app.go`

**Interfaces:**
- Consumes: `Server.startMessage`, `Server.stopRunByID`, `activeAgentSession.stopping`.
- Produces: A streaming session refuses new sends once a stop operation begins, and stopping cannot cascade to a concurrently admitted run.

- [ ] **Step 1: Write the failing test**

Add a streaming-session test that holds the stop path after it has selected the session, submits a second run, and asserts that the second admission cannot complete until the old session stop completes.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 ./internal/app -run TestStreamingStopSerializesConcurrentAdmission`

Expected: FAIL because `startMessage` can still send a newly committed queued run while `AgentSession.Stop` is in progress.

- [ ] **Step 3: Write minimal implementation**

Guard streaming database admission, session lookup/send, and the streaming stop sequence with the same lifecycle lock. Hold it while verifying no other active runs and calling `Stop()` so a later admission cannot enter the stopped session.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -count=1 ./internal/app -run TestStreamingStopSerializesConcurrentAdmission`

Expected: PASS.

- [ ] **Step 5: Run regression verification**

Run: `go test -race -count=1 ./... && go vet ./... && go build ./cmd/control-server`

Expected: all commands exit successfully.
