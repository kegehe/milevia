# Shortcut Group Row Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Place prompt and command shortcut groups side by side on wide screens while retaining vertical, non-scrolling shortcut lists.

**Architecture:** CSS controls group and item directions independently. The Node source test asserts a two-column desktop grid and a one-column mobile override.

**Tech Stack:** CSS, Node.js built-in test runner, TypeScript, Vite.

## Global Constraints

- Preserve the `300px` desktop left rail and smaller external left inset.
- Keep every `.quick-tag-list` vertical with `display: grid` and `overflow-x: visible`.
- Use two equal-width group columns above `820px`, and one group column at `820px` and below.

---

### Task 1: Cover and Implement the Group Direction

**Files:**
- Modify: `apps/web/src/conversation-layout.test.mjs:12-16`
- Modify: `apps/web/src/conversation.css:130-140`

**Interfaces:**
- Consumes: `.quick-actions-row` and `.quick-actions-row .quick-tag-list` selectors.
- Produces: horizontal shortcut groups on wide screens and vertical shortcut items at every breakpoint.

- [ ] **Step 1: Change the failing layout expectation**

```js
assert.match(stylesheet, /\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/s);
assert.match(stylesheet, /@media \(max-width: 820px\)\s*\{[^}]*\.quick-actions-row\s*\{[^}]*grid-template-columns:\s*1fr;/s);
```

- [ ] **Step 2: Verify the test fails**

Run: `node --test apps/web/src/conversation-layout.test.mjs`

Expected: the wide-screen group-direction assertion fails because the current rule uses `grid-template-columns: 1fr`.

- [ ] **Step 3: Apply the CSS change**

```css
.quick-actions-row { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
```

- [ ] **Step 4: Verify the regression test and production build**

Run: `node --test apps/web/src/conversation-layout.test.mjs && pnpm --filter @auto/web build`

Expected: `2` passing tests, `0` failing tests, and a successful Vite build.
