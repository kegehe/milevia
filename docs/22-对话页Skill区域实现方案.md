# 对话页 Skill 区域实现方案

> 日期：2026-08-09
> 目标：在对话页左侧「常用命令」之下新增「技能 (Skill)」区域，自动读取当前环境里安装的 Claude Code / Codex 全部 skill，点击后把「技能名 + 描述」填入输入框，便于手写提示词时引用。

## 1. 问题与目标

用户在对话页维护 Claude Code / Codex 会话时，常希望调用已安装的 skill（如 `frontend-design`、`claude-md-improver`、插件市场里的各种技能）。当前没有任何入口能看到"这台机器/这个项目已经装了哪些 skill"，只能靠记忆在提示词里手写 `<name>`。本功能在「常用提示词」「常用命令」两个快捷区之后增加一个 Skill 区，列出当前环境 + 当前会话 agent 对应的全部技能，点击再把一段引用文本填入输入框（**只填入、不发送**，与技能的生产力形态——由模型根据描述决定是否加载——匹配）。

```text
选择某个 skill
  -> 文件系统扫描（当前环境 user/project/plugin 的 skills 目录）
  -> 解析 SKILL.md frontmatter（name / description）
  -> 按会话 agentId 过滤展示在 Skill 区
  -> 点击 -> 技能名 + 描述 填入输入框
```

## 2. 范围与非目标

### 2.1 本阶段实现
- 在对话页常用命令之下显示 Skill 区；
- 读取当前环境（Windows / WSL 项目 / SSH 远端）可用的 Claude Code 与 Codex skill；
- 来源覆盖：用户级、项目级、Claude 插件树（`.claude/plugins` 内 `skills` 目录）；
- 点击 skill → 把「技能名 + 描述」填入输入框并聚焦；
- 按当前会话 `agentId`（claude-code / codex）过滤展示对应 CLI 的技能。

### 2.2 本阶段不做
- 不解析 / 编辑 skill 内容，不提供 skill 的创建、删除、启停、排序；
- 不依赖 `claude skills list` / `codex skills` CLI 子命令（未稳定，见 §4）；
- 不做 skill 搜索、收藏、导入导出；
- 不把 skill 作为系统提示词注入 Claude，也不修改原始会话配置；
- WSL 发行版内用户级 `~/.claude` skill 本期不跨端扫描（见 §5，先示空态）。

## 3. 设计原则

1. **文件系统是唯一真实来源。** skill 的权威信息就是 `<name>/SKILL.md` 的 frontmatter；不依赖任何 CLI 子命令或返回格式。
2. **读"CLI 真正运行的环境"那一侧。** skill 发现复用 `resolveAgentTargetEnv` + `runnerRegistry` 的 Runner 分发：Windows 项目读本机 `%USERPROFILE%\.claude`，SSH 远端项目读远端 `$HOME/.claude` 等，而不是服务端本机路径。
3. **只填入、不发送。** skill 点击仅 `setComposerText`，与快捷方式的 `defaultAction === "fill"` 行为一致；发送与否由用户决定。
4. **空态不报错。** 目录不存在、远端未就绪或解析失败都静默返回空列表，Skill 区显示"未发现 Skill"。
5. **容忍陌生与坏 frontmatter。** 只取 `name`/`description`，忽略一切未知键；`name`/`description` 缺失时回退为目录名。

## 4. Skill 来源与存储（调研结论）

skill 是一个目录 `<name>/`，内含强制文件 `SKILL.md`（frontmatter 为 `---` 分隔的 YAML）。对 UI 只需 `name` 与 `description` 两键。

| CLI | 来源 | 路径（相对 home/project） |
| --- | --- | --- |
| Claude Code | 用户级 | `~/.claude/skills/<name>/SKILL.md` |
| Claude Code | 项目级 | `<project>/.claude/skills/<name>/SKILL.md` |
| Claude Code | 插件树 | `~/.claude/plugins/**/skills/<name>/SKILL.md` |
| Codex | 用户级 | `~/.codex/skills/<name>/SKILL.md` |
| Codex | 项目级 | `<project>/.codex/skills/<name>/SKILL.md` |

本机实测：`%USERPROFILE%\.claude\plugins\` 下有 27 个 SKILL.md（marketplace / external_plugins / cache 等目录形态），验证了插件树形态的真实性。

**优先级**：同名（agent+name）项目级 > 项目级 > 用户级（`skillScanRoot.priority` 递增装配，project 数值最大胜出）。

**明确不做 CLI 子命令**：`claude skills list` 无稳定版本/输出格式，社区至今有第三方 skill 管理工具正是因为在等上游列表化。以文件系统扫描为准，未来若上游稳定再做可选增强。

## 5. 后端设计

### 5.1 `apps/control-server/internal/app/skills.go`

- `type Skill { Name, Description, Agent, Env, Source, BackgroundSource }`
  - `Agent`：`claude-code` | `codex`
  - `Env`：`windows` | `wsl` | `remote-linux`
  - `Source`：`user` | `project` | `plugin`
- `parseSkillFrontmatter(content)`：手写轻量解析（不引入 yaml 依赖），仅取 `name`/`description`，跳过嵌套块，容忍未知键。
- `discoverLocalSkillRoots(target, projectPath)`：按目标环境返回待扫描根目录。
  - windows：用户级 + 插件树 + 项目级（`homeDir()` + `projectPath`）。
  - wsl：仅项目级（服务端 Windows，UNC 路径可直读 `.claude/.codex`；`~/.claude` 跨发行版不可靠，本期项目级先行）。
- `scanLocalSkills` / `scanPluginTree`：`os.ReadDir` / 递归 Walk，收集 `SKILL.md`；插件树跳过 `.git`/`.hg`/`node_modules`。
- `resolveSkillScans`：解析 frontmatter + 按优先级去重 + 按 `(agent, name)` 排序。
- `discoverSkillsForProject(ctx, project, agentID)`：`resolveAgentTargetEnv` 分发：
  - 本机 → `scanLocalSkills` + `resolveSkillScans` + `filterSkillsByAgent`。
  - SSH 远端 → `discoverSkillsRemote`。
- `discoverSkillsRemote`：`sshRunner.client.execCommand` 跑一条 `find` 命令（user/project `.claude`/`.codex` skills + 插件树），用 `marker+路径+内容` 分隔回传，`parseRemoteSkillOutput` 解析。`remoteHome` = `sshRunner.client.execCommand("printf '%s' \"$HOME\"")`。
- `listSkills`（handler）：`getProjectByID` → `discoverSkillsForProject`，`?agentId=` 可选过滤。

### 5.2 路由（`app.go`）
```go
r.Get("/api/projects/{projectID}/skills", s.listSkills)
```
每次请求实时扫描（skill 数量少、开销小），不做缓存，避免远端过期。

### 5.3 测试（`skills_test.go`）
- `parseSkillFrontmatter`：正常 / 引号 / 嵌套块 / 无 frontmatter / 缺 desc / 未知键。
- `scanLocalSkills` + `resolveSkillScans`：temp 目录造 user/project/plugin 树，断言去重优先级、来源、`.git` 不泄漏。
- `filterSkillsByAgent` / `parseRemoteSkillOutput` / `skillAgentForRemotePath` / `skillSourceForRemotePath`。

## 6. 前端设计

### 6.1 `apps/web/src/lib/types.ts`
```ts
export type SkillAgent = "claude-code" | "codex";
export type Skill = { name: string; description: string; agent: SkillAgent; env: "windows"|"wsl"|"remote-linux"; source: "user"|"project"|"plugin" };
```

### 6.2 `apps/web/src/pages/ConversationPage.tsx`
- state：`skills: Skill[]`、`skillsLoading: boolean`。
- 加载 effect：项目 + `conversation.agentId` 确定后 `GET /api/projects/{projectId}/skills?agentId=...` → `setSkills`，失败静默置空。
- `mergeSkillPrompt(skill)`：`description` 与 `name` 不同时返回 `请使用技能 <name>：description`，否则 `请使用技能 <name>`。
- `useSkill(skill)`：`setComposerText(mergeSkillPrompt(skill))` + 聚焦 textarea。
- `renderSkillGroup()`：标题 + 技能标签列表（桌面 `quick-actions-row` 与移动 `quick-actions-mobile` 各渲染一份）；加载中 / 空态（"未发现 Skill"）分别呈现；每个标签 `title` 悬浮显示描述 + 来源。
- 新增 `SkillTagIcon`（三组图标中的第三组）。

### 6.3 `apps/web/src/conversation.css`
- `.skill-tags .quick-tag-heading`（绿青色系 `#1f7a6b`）区分于绿色提示词 / 橙色命令。
- `.skill-tag`（flex 两端对齐：名称 + 来源徽标）、`.skill-source-chip.user|project|plugin` 三种徽标配色、`.skill-empty`/`.skill-loading`。

## 7. 关键风险与取舍
- **SSH 远端**是复杂度主要来源：用一条 `find+cat` 命令 + 本机解析，省往返；需处理 marker 分隔与路径注入，解析失败静默降级为空。
- **WSL 用户级 skill**：本期仅项目级，`~/.claude` 待后续按需跨端；WSL 项目若无技能则示空态，不报错。
- **不依赖 CLI 子命令**：文件系统扫描稳定；`claude skills list` 稳定后再做可选增强。

## 8. 验证
1. `cd apps/control-server && go test ./internal/app/ -run 'Skill' -v`
2. 起 control server，`curl /api/projects/{id}/skills?agentId=claude-code` 返回本机 skill 列表（含插件来源）。
3. 前端：打开项目对话页 → 常用命令下出现 Skill 区 → 点击某 skill → 输入框出现「技能名+描述」→ 正常发送。
4. SSH 项目：Skill 区显示远端 skill 或空态且不报错。
