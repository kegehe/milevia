# Shortcut Shared Rows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align wide-screen prompt and command shortcuts row by row, while preserving the existing narrow-screen order of all prompts followed by all commands.

**Architecture:** On wide screens, CSS uses `display: contents` to make both group headings and shortcut cells participate in the same two-column parent grid. Explicit column placement gives each matching shortcut index the same grid row. The narrow-screen media query restores the original block containers and document order.

**Tech Stack:** React 19, CSS, Node.js built-in test runner, TypeScript, Vite.

## Global Constraints

- Do not alter shortcut data, sending, editing, variables, disabled states, or task queue behavior.
- Wide screens use one shared two-column grid with aligned heading and shortcut rows.
- Empty cells remain empty; an additional command occupies only the next right-column row.
- At `820px` and below, show all prompt shortcuts followed by all command shortcuts with no horizontal scrolling.

---

### Task 1: Test and Implement Shared Grid Rows

**Files:**
- Modify: `apps/web/src/conversation-layout.test.mjs:12-17`
- Modify: `apps/web/src/conversation.css:130-140`

**Interfaces:**
- Consumes: existing `.quick-actions-row`, `.quick-tag-group`, `.quick-tag-heading`, and `.quick-tag-list` markup in `apps/web/src/App.tsx`.
- Produces: shared wide-screen grid tracks without changing JSX or event handlers.

- [ ] **Step 1: Add failing source assertions**

```js
assert.match(stylesheet, /\.quick-actions-row \.quick-tag-group\s*\{[^}]*display:\s*contents;/s);
assert.match(stylesheet, /\.quick-tag-group:first-child \.quick-tag-heading,[^}]*\.quick-tag-group:first-child \.quick-tag,[^}]*\.quick-tag-group:first-child \.quick-tag-empty\s*\{[^}]*grid-column:\s*1;/s);
assert.match(stylesheet, /\.command-tags \.quick-tag-heading,[^}]*\.command-tags \.quick-tag,[^}]*\.command-tags \.quick-tag-empty\s*\{[^}]*grid-column:\s*2;/s);
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `node --test apps/web/src/conversation-layout.test.mjs`

Expected: shared-group assertions fail because groups and lists are currently nested independent grids.

- [ ] **Step 3: Implement the shared-grid and narrow-screen reset**

```css
.quick-actions-row .quick-tag-group,
.quick-actions-row .quick-tag-list { display: contents; }
.quick-tag-group:first-child .quick-tag-heading,
.quick-tag-group:first-child .quick-tag,
.quick-tag-group:first-child .quick-tag-empty { grid-column: 1; }
.command-tags .quick-tag-heading,
.command-tags .quick-tag,
.command-tags .quick-tag-empty { grid-column: 2; }
```

In `@media (max-width: 820px)`, restore `.quick-tag-group` and `.quick-tag-list` to `display: grid`, reset `grid-column` and `grid-row` to `auto`, and retain the existing one-column parent grid.

- [ ] **Step 4: Run the regression test and production build**

Run: `node --test apps/web/src/conversation-layout.test.mjs && pnpm --filter @milevia/web build`

Expected: `2` passing tests, `0` failing tests, and a successful Vite production build.

- [ ] **Step 5: Commit the focused implementation**

```bash
git add apps/web/src/conversation.css apps/web/src/conversation-layout.test.mjs
git commit -m "fix: align shortcut rows"
```
