# Conversation Rail Layout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make conversation shortcuts vertical without horizontal scrolling and reduce the desktop canvas's external left margin while preserving the 300px left rail.

**Architecture:** The JSX already expresses prompt and command groups inside one shortcut-row wrapper, so this change is CSS-only. A small Node source-level regression test protects the required desktop layout declarations; existing Vite build verifies TypeScript and production bundling.

**Tech Stack:** React 19, TypeScript, Vite 8, CSS, Node.js built-in test runner.

## Global Constraints

- Keep the desktop left rail approximately 300px wide; do not reduce shortcut or task-queue content width.
- Keep shortcut send, edit, variable, disable, task-queue, and responsive task-drawer behavior unchanged.
- Use vertical shortcut groups and item lists at every breakpoint; neither the groups nor their item lists may create horizontal scrollbars.
- Align the desktop timeline and composer to the same smaller left page inset while reserving the right side for future content.

---

### Task 1: Add Layout Regression Coverage

**Files:**
- Create: `apps/web/src/conversation-layout.test.mjs`

**Interfaces:**
- Consumes: `apps/web/src/conversation.css` as UTF-8 source text.
- Produces: Node test cases that require a 300px desktop rail, vertical shortcut lists, and no shortcut-row horizontal scrolling.

- [ ] **Step 1: Write the failing test**

```js
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const stylesheet = await readFile(new URL("./conversation.css", import.meta.url), "utf8");

test("desktop canvas preserves the rail width while using a small left inset", () => {
  assert.match(stylesheet, /\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*300px\s+minmax\(0,\s*1fr\)[^}]*margin:\s*0\s+0\s+0\s+24px;/s);
  assert.doesNotMatch(stylesheet, /@media \(max-width: 1080px\)\s*\{\s*\.conversation-canvas\s*\{[^}]*grid-template-columns:\s*250px/s);
});

test("shortcut groups and their items remain vertical without horizontal scrolling", () => {
  assert.match(stylesheet, /\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*1fr;/s);
  assert.match(stylesheet, /\.quick-actions-row \.quick-tag-list\s*\{[^}]*display:\s*grid;[^}]*overflow-x:\s*visible;/s);
  assert.doesNotMatch(stylesheet, /\.quick-actions-row\s*\{[^}]*overflow-x:\s*auto/s);
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `node --test apps/web/src/conversation-layout.test.mjs`

Expected: two assertion failures because the current desktop canvas is centered and shortcut lists use `display: flex` with `overflow-x: auto`.

- [ ] **Step 3: Commit the test-only change**

```bash
git add apps/web/src/conversation-layout.test.mjs
git commit -m "test: cover conversation rail layout"
```

### Task 2: Apply the Conversation-Rail CSS Layout

**Files:**
- Modify: `apps/web/src/conversation.css:128-143`

**Interfaces:**
- Consumes: `.conversation-canvas`, `.quick-actions-row`, `.quick-tag-list`, and `.chat .composer` selectors used by the existing chat JSX.
- Produces: a left-aligned desktop canvas, full-width vertical shortcut groups and item lists, and responsive rules with no shortcut horizontal scrolling.

- [ ] **Step 1: Replace the desktop shortcut-row and canvas declarations**

```css
.conversation-canvas { width: min(1280px, calc(100% - 48px)); grid-template-columns: 300px minmax(0, 1fr); gap: 30px; margin: 0 0 0 24px; }
.quick-actions-row { display: grid; grid-template-columns: 1fr; gap: 14px; }
.quick-actions-row .quick-tag-list { display: grid; gap: 7px; overflow-x: visible; padding-bottom: 0; }
.quick-actions-row .quick-tag { max-width: none; }
.quick-actions-row .quick-tag > button:first-child, .quick-actions-row .quick-tag-empty { min-width: 0; }
```

- [ ] **Step 2: Update responsive overrides to preserve vertical lists**

```css
@media (max-width: 820px) {
  .quick-actions-row { display: grid; width: 100%; min-width: 0; gap: 12px; overflow-x: visible; padding-bottom: 0; }
  .quick-actions-row .quick-tag-group { min-width: 0; }
  .quick-actions-row .quick-tag-list { display: grid; overflow-x: visible; }
}
```

- [ ] **Step 3: Align the desktop composer with the left-aligned canvas**

```css
@media (max-width: 1080px) {
  .conversation-canvas { width: calc(100% - 48px); grid-template-columns: 300px minmax(0, 1fr); gap: 20px; }
}
@media (min-width: 1081px) { .chat .composer { padding-right: 30px; padding-left: 354px; } }
@media (min-width: 821px) and (max-width: 1080px) { .chat .composer { padding-right: 24px; padding-left: 344px; } }
```

- [ ] **Step 4: Run the layout test to verify it passes**

Run: `node --test apps/web/src/conversation-layout.test.mjs`

Expected: `2` passing tests and `0` failing tests.

- [ ] **Step 5: Commit the implementation**

```bash
git add apps/web/src/conversation.css
git commit -m "fix: align conversation rail and stack shortcuts"
```

### Task 3: Verify Production Build

**Files:**
- Verify: `apps/web/src/conversation-layout.test.mjs`
- Verify: `apps/web/src/conversation.css`

**Interfaces:**
- Consumes: the final source and Vite build configuration.
- Produces: evidence that the regression test and frontend production build succeed.

- [ ] **Step 1: Run the full layout regression test**

Run: `node --test apps/web/src/conversation-layout.test.mjs`

Expected: `2` passing tests and `0` failing tests.

- [ ] **Step 2: Run the frontend production build**

Run: `pnpm --filter @milevia/web build`

Expected: command exits with code `0` after `tsc -b` and Vite finish successfully.
