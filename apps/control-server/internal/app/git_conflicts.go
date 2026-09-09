package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
)

// errGitPathNotInConflict 表示目标路径当前不在未合并状态（已解决/被外部处理）。
var errGitPathNotInConflict = errors.New("Git path is not currently in conflict")

// Git 冲突解决相关领域类型与 gitCLIRunner 实现。
//
// 设计要点：合并（merge/pull）与变基/挑选（rebase/cherry-pick）的"ours/theirs"
// 语义相反——rebase/cherry-pick 时 HEAD 游离，ours 是新基底（目标）、theirs 是
// 正在重放的提交。因此两侧的显示名一律由后端按操作类型换算好再下发给前端，
// 前端只展示“当前/传入”按钮，不做任何反转推断。

// GitConflictOperationType 标识仓库当前进行中的 git 操作类型。
type GitConflictOperationType string

const (
	GitConflictOperationMerge      GitConflictOperationType = "merge"
	GitConflictOperationRebase     GitConflictOperationType = "rebase"
	GitConflictOperationCherryPick GitConflictOperationType = "cherry-pick"
	GitConflictOperationRevert     GitConflictOperationType = "revert"
	GitConflictOperationNone       GitConflictOperationType = "none"
)

// GitConflictContext 描述冲突发生的操作上下文与两侧标签。
type GitConflictContext struct {
	OperationType GitConflictOperationType `json:"operationType"`
	OursLabel     string                   `json:"oursLabel"`   // “当前/左侧”侧名称（语义已按操作类型换算）
	TheirsLabel   string                   `json:"theirsLabel"` // “传入/右侧”侧名称
	CanAbort      bool                     `json:"canAbort"`
	CanFinish     bool                     `json:"canFinish"`
}

// GitConflictFile 描述一个冲突文件及其语义类型。
type GitConflictFile struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"` // content / add-add / modify-delete / delete-modify / both-deleted
	OursDeleted   bool   `json:"oursDeleted"`
	TheirsDeleted bool   `json:"theirsDeleted"`
}

// GitConflictOverview 是冲突状态总览：操作上下文 + 冲突文件清单。
type GitConflictOverview struct {
	Context GitConflictContext `json:"context"`
	Files   []GitConflictFile  `json:"files"`
}

// GitConflictContent 是单个冲突文件的可解决数据：三方内容与当前工作区内容。
// binary/oversized 时文本字段为空，界面只提供整文件“采用当前/传入/删除”操作。
type GitConflictContent struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	Binary        bool   `json:"binary"`
	Oversized     bool   `json:"oversized"`
	OursDeleted   bool   `json:"oursDeleted"`
	TheirsDeleted bool   `json:"theirsDeleted"`
	Base          string `json:"base,omitempty"`
	Ours          string `json:"ours,omitempty"`
	Theirs        string `json:"theirs,omitempty"`
	Working       string `json:"working,omitempty"`
}

// ConflictOverview 返回冲突操作上下文与冲突文件清单。
func (runner *gitCLIRunner) ConflictOverview(ctx context.Context, repo string) (GitConflictOverview, error) {
	files, err := runner.listConflictedFiles(ctx, repo)
	if err != nil {
		return GitConflictOverview{}, err
	}
	contextInfo, err := runner.buildConflictContext(ctx, repo)
	if err != nil {
		return GitConflictOverview{}, err
	}
	return GitConflictOverview{Context: contextInfo, Files: files}, nil
}

// ConflictContent 读取单个冲突文件的三方内容与当前工作区内容。
func (runner *gitCLIRunner) ConflictContent(ctx context.Context, repo, path string) (GitConflictContent, error) {
	if err := validateGitPath(path); err != nil {
		return GitConflictContent{}, err
	}
	entries, err := runner.unmergedStagesForPath(ctx, repo, path)
	if err != nil {
		return GitConflictContent{}, err
	}
	if len(entries) == 0 {
		return GitConflictContent{}, errGitPathNotInConflict
	}
	content := GitConflictContent{Path: path}
	content.Kind, content.OursDeleted, content.TheirsDeleted = gitConflictKindFromStages(entries)
	for stage, oid := range entries {
		text, binary, oversized, err := runner.readBlobText(ctx, repo, oid)
		if err != nil {
			// 单个 blob 读取失败不应让整个文件不可解决：按“超限不可展示”降级。
			content.Oversized = true
			continue
		}
		if binary {
			content.Binary = true
			continue
		}
		if oversized {
			content.Oversized = true
			continue
		}
		switch stage {
		case 1:
			content.Base = text
		case 2:
			content.Ours = text
		case 3:
			content.Theirs = text
		}
	}
	// 工作区文件：删除型冲突时文件可能已不存在，读取失败按空处理。
	mode, size, _, _, statErr := runner.lstat(repo, path)
	if statErr == nil && mode.IsRegular() && size <= gitOutputLimit {
		if raw, readErr := runner.readFile(repo, path); readErr == nil {
			if bytes.IndexByte(raw, 0) >= 0 {
				content.Binary = true
			} else {
				content.Working = string(raw)
			}
		}
	} else if statErr == nil && size > gitOutputLimit {
		content.Oversized = true
	}
	if content.Binary {
		// 二进制文件不携带任何文本，保留工作区原始字节的读取提示由整文件操作承载。
		content.Base, content.Ours, content.Theirs, content.Working = "", "", "", ""
	}
	return content, nil
}

// ResolveConflict 把 path 标记为已解决。action 取值：
//   - "ours"/"theirs"：整文件采用对应侧；对侧为删除时等价于删除该文件；
//   - "delete"：将该文件以删除收场（git rm）；
//   - "working"：用 content 覆写工作区（content 非空时）后 git add，即“手工编辑后标记已解决”。
func (runner *gitCLIRunner) ResolveConflict(ctx context.Context, repo, path, action string, content []byte) error {
	if err := validateGitPath(path); err != nil {
		return err
	}
	switch action {
	case "ours", "theirs", "delete", "working":
	default:
		return errors.New("unsupported conflict resolve action")
	}
	entries, err := runner.unmergedStagesForPath(ctx, repo, path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errGitPathNotInConflict
	}
	oursDeleted := entries[2] == ""
	theirsDeleted := entries[3] == ""
	switch action {
	case "ours":
		if oursDeleted {
			return runner.resolveAsDelete(ctx, repo, path)
		}
		if err := runner.checkoutSideThenStage(ctx, repo, path, "ours"); err != nil {
			return err
		}
		return nil
	case "theirs":
		if theirsDeleted {
			return runner.resolveAsDelete(ctx, repo, path)
		}
		if err := runner.checkoutSideThenStage(ctx, repo, path, "theirs"); err != nil {
			return err
		}
		return nil
	case "delete":
		return runner.resolveAsDelete(ctx, repo, path)
	case "working":
		if content != nil {
			if len(content) > gitOutputLimit {
				return errors.New("resolved content exceeds the size limit")
			}
			if err := runner.backend.writeFile(repo, path, content); err != nil {
				return err
			}
		}
		_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "add"}, []string{path})
		return err
	}
	return errors.New("unsupported conflict resolve action")
}

// AbortConflict 中止当前进行中的合并/变基/cherry-pick/revert。
func (runner *gitCLIRunner) AbortConflict(ctx context.Context, repo string) error {
	operation, err := runner.detectConflictOperation(ctx, repo)
	if err != nil {
		return err
	}
	switch operation {
	case GitConflictOperationMerge:
		_, err = runner.backend.runGit(ctx, repo, "merge", "--abort")
	case GitConflictOperationRebase:
		_, err = runner.backend.runGit(ctx, repo, "rebase", "--abort")
	case GitConflictOperationCherryPick:
		_, err = runner.backend.runGit(ctx, repo, "cherry-pick", "--abort")
	case GitConflictOperationRevert:
		_, err = runner.backend.runGit(ctx, repo, "revert", "--abort")
	default:
		return errors.New("没有可中止的合并/变基操作")
	}
	return err
}

// FinishConflict 在所有冲突解决后完成当前操作：merge 直接以默认信息提交，
// rebase/cherry-pick/revert 执行 --continue（core.editor=true 避免弹编辑器）。
func (runner *gitCLIRunner) FinishConflict(ctx context.Context, repo string) error {
	operation, err := runner.detectConflictOperation(ctx, repo)
	if err != nil {
		return err
	}
	switch operation {
	case GitConflictOperationMerge:
		_, err = runner.backend.runGit(ctx, repo, "commit", "--no-edit")
	case GitConflictOperationRebase:
		_, err = runner.backend.runGit(ctx, repo, "-c", "core.editor=true", "rebase", "--continue")
	case GitConflictOperationCherryPick:
		_, err = runner.backend.runGit(ctx, repo, "-c", "core.editor=true", "cherry-pick", "--continue")
	case GitConflictOperationRevert:
		_, err = runner.backend.runGit(ctx, repo, "-c", "core.editor=true", "revert", "--continue")
	default:
		return errors.New("没有进行中的可完成操作")
	}
	return err
}

// buildConflictContext 探测操作类型并把两侧名称换算为“ours=当前侧 / theirs=传入侧”。
func (runner *gitCLIRunner) buildConflictContext(ctx context.Context, repo string) (GitConflictContext, error) {
	operation, err := runner.detectConflictOperation(ctx, repo)
	if err != nil {
		return GitConflictContext{}, err
	}
	head := runner.shortRefName(ctx, repo, "HEAD")
	if head == "" {
		head = runner.shortOID(ctx, repo, "HEAD")
	}
	contextInfo := GitConflictContext{OperationType: operation, OursLabel: head}
	switch operation {
	case GitConflictOperationMerge:
		contextInfo.TheirsLabel = runner.nameRef(ctx, repo, "MERGE_HEAD")
		contextInfo.CanAbort, contextInfo.CanFinish = true, true
	case GitConflictOperationRebase:
		// rebase 时 ours=目标基底（HEAD 游离在新基底上），theirs=正在重放的提交。
		contextInfo.OursLabel = runner.nameRef(ctx, repo, "HEAD")
		if contextInfo.OursLabel == "" {
			contextInfo.OursLabel = head
		}
		contextInfo.TheirsLabel = runner.nameRef(ctx, repo, "REBASE_HEAD")
		contextInfo.CanAbort, contextInfo.CanFinish = true, true
	case GitConflictOperationCherryPick:
		contextInfo.TheirsLabel = runner.nameRef(ctx, repo, "CHERRY_PICK_HEAD")
		contextInfo.CanAbort, contextInfo.CanFinish = true, true
	case GitConflictOperationRevert:
		contextInfo.TheirsLabel = runner.nameRef(ctx, repo, "REVERT_HEAD")
		contextInfo.CanAbort, contextInfo.CanFinish = true, true
	default:
		contextInfo.CanAbort, contextInfo.CanFinish = false, false
	}
	if contextInfo.OursLabel == "" {
		contextInfo.OursLabel = "HEAD"
	}
	if contextInfo.TheirsLabel == "" {
		contextInfo.TheirsLabel = "传入改动"
	}
	return contextInfo, nil
}

// detectConflictOperation 通过伪引用判断当前进行中的操作。注意判定顺序：
// rebase 自身不会留下 MERGE_HEAD，但可能残留 CHERRY_PICK_HEAD 之类，故先查 REBASE_HEAD。
func (runner *gitCLIRunner) detectConflictOperation(ctx context.Context, repo string) (GitConflictOperationType, error) {
	if runner.revParseVerify(ctx, repo, "REBASE_HEAD") {
		return GitConflictOperationRebase, nil
	}
	if runner.revParseVerify(ctx, repo, "MERGE_HEAD") {
		return GitConflictOperationMerge, nil
	}
	if runner.revParseVerify(ctx, repo, "CHERRY_PICK_HEAD") {
		return GitConflictOperationCherryPick, nil
	}
	if runner.revParseVerify(ctx, repo, "REVERT_HEAD") {
		return GitConflictOperationRevert, nil
	}
	return GitConflictOperationNone, nil
}

func (runner *gitCLIRunner) revParseVerify(ctx context.Context, repo, ref string) bool {
	_, err := runner.backend.runGit(ctx, repo, "rev-parse", "-q", "--verify", ref)
	return err == nil
}

// shortRefName 返回当前分支短名；游离/未命名时返回空。
func (runner *gitCLIRunner) shortRefName(ctx context.Context, repo, ref string) string {
	out, err := runner.backend.runGit(ctx, repo, "symbolic-ref", "--quiet", "--short", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (runner *gitCLIRunner) shortOID(ctx context.Context, repo, ref string) string {
	out, err := runner.backend.runGit(ctx, repo, "rev-parse", "--short", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// nameRef 用 name-rev 把引用解析为便于阅读的名字（分支名/分支~N）；失败时回退短 OID。
func (runner *gitCLIRunner) nameRef(ctx context.Context, repo, ref string) string {
	if name := runner.nameRev(ctx, repo, ref); name != "" {
		return name
	}
	return runner.shortOID(ctx, repo, ref)
}

func (runner *gitCLIRunner) nameRev(ctx context.Context, repo, ref string) string {
	out, err := runner.backend.runGit(ctx, repo, "name-rev", "--name-only", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// listConflictedFiles 通过 git ls-files -u 列出冲突文件及其语义类型。
func (runner *gitCLIRunner) listConflictedFiles(ctx context.Context, repo string) ([]GitConflictFile, error) {
	entries, err := runner.collectUnmergedStages(ctx, repo)
	if err != nil {
		return nil, err
	}
	files := make([]GitConflictFile, 0, len(entries))
	for path, stages := range entries {
		if len(stages) == 0 {
			continue
		}
		kind, oursDeleted, theirsDeleted := gitConflictKindFromStages(stages)
		files = append(files, GitConflictFile{Path: path, Kind: kind, OursDeleted: oursDeleted, TheirsDeleted: theirsDeleted})
	}
	return files, nil
}

// collectUnmergedStages 返回 path → stage → blob oid。
func (runner *gitCLIRunner) collectUnmergedStages(ctx context.Context, repo string) (map[string]map[int]string, error) {
	raw, err := runner.backend.runGit(ctx, repo, "ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	return parseUnmergedLSFiles(raw), nil
}

func (runner *gitCLIRunner) unmergedStagesForPath(ctx context.Context, repo, path string) (map[int]string, error) {
	entries, err := runner.collectUnmergedStages(ctx, repo)
	if err != nil {
		return nil, err
	}
	return entries[path], nil
}

// parseUnmergedLSFiles 解析 `git ls-files -u -z`：每条记录形如
// "<mode> <oid> <stage>\t<path>\0"。
func parseUnmergedLSFiles(raw []byte) map[string]map[int]string {
	result := map[string]map[int]string{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, path, found := bytes.Cut(record, []byte{'\t'})
		if !found {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			continue
		}
		stage, err := parseInt(fields[2])
		if err != nil || stage < 1 || stage > 3 {
			continue
		}
		if !isFullGitObjectID(fields[1]) {
			continue
		}
		key := string(path)
		if result[key] == nil {
			result[key] = map[int]string{}
		}
		result[key][stage] = fields[1]
	}
	return result
}

// gitConflictKindFromStages 由 stage 存在情况推导语义类型。
func gitConflictKindFromStages(stages map[int]string) (kind string, oursDeleted, theirsDeleted bool) {
	_, hasBase := stages[1]
	_, hasOurs := stages[2]
	_, hasTheirs := stages[3]
	switch {
	case hasOurs && hasTheirs:
		if hasBase {
			return "content", false, false
		}
		return "add-add", false, false
	case hasOurs:
		return "modify-delete", false, true
	case hasTheirs:
		return "delete-modify", true, false
	default:
		return "both-deleted", true, true
	}
}

// readBlobText 读取 blob 内容。含 NUL 视为二进制；超过输出上限标记 oversized。
func (runner *gitCLIRunner) readBlobText(ctx context.Context, repo, oid string) (text string, binary, oversized bool, err error) {
	raw, err := runner.backend.runGit(ctx, repo, "cat-file", "blob", oid)
	if err != nil {
		if errors.Is(err, errGitOutputTooLarge) {
			return "", false, true, nil
		}
		return "", false, false, err
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", true, false, nil
	}
	return string(raw), false, false, nil
}

func (runner *gitCLIRunner) checkoutSideThenStage(ctx context.Context, repo, path, side string) error {
	if _, err := runner.backend.runGit(ctx, repo, "checkout", "--"+side, "--", path); err != nil {
		return err
	}
	_, err := runner.backend.runGitPaths(ctx, repo, []string{"--literal-pathspecs", "add"}, []string{path})
	return err
}

func (runner *gitCLIRunner) resolveAsDelete(ctx context.Context, repo, path string) error {
	_, err := runner.backend.runGit(ctx, repo, "rm", "--", path)
	return err
}

// parseInt 用于解析小整数；避免为解析 git 输出引入 strconv 直接依赖判断。
func parseInt(value string) (int, error) {
	var result int
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		result = result*10 + int(r-'0')
		if result > 1<<20 {
			return 0, errors.New("number too large")
		}
	}
	return result, nil
}
