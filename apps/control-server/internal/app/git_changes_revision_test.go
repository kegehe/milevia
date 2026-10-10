package app

import "testing"

// 手机端那个"只取 summary"的后台探针靠这个摘要判断"变更清单还是不是刚才那份"。
//
// 为什么要专门测它：判据一开始只比 worktree 的**计数**，而"一个文件恢复干净、另一个文件
// 同时被改"会让计数一模一样、清单却换了人 —— 探针于是每次判"没变"、永远早退，
// 手机端就一直显示那份已经过期的清单：用户对着一条恢复干净的行点暂存，
// 服务端按 stateToken 判"仓库状态已经变化"，而真正改动的文件根本不在列表里。
func TestGitChangesRevisionDistinguishesSameCountDifferentSet(t *testing.T) {
	modified := func(path string) GitChange { return GitChange{Path: path, Modified: true} }
	fingerprintsFor := func(paths ...string) map[string]string {
		result := make(map[string]string, len(paths))
		for _, path := range paths {
			result[path] = "100644:5:634000000000000000:1699999999"
		}
		return result
	}

	// ① 计数相同、文件换了人 —— 必须算出不同的摘要（否则探针永远看不见这次变化）
	first := gitChangesRevision([]GitChange{modified("apps/web/src/git.css")}, fingerprintsFor("apps/web/src/git.css"))
	second := gitChangesRevision([]GitChange{modified("notes/other.md")}, fingerprintsFor("notes/other.md"))
	if first == second {
		t.Fatal("换了文件却算出同一个摘要：探针会判成「没变」而让手机端一直显示过期清单")
	}

	// ② 同一份清单重复算必须稳定（不然每次 probe 都会当成"变了"，白取一趟 10KB）
	same := gitChangesRevision([]GitChange{modified("apps/web/src/git.css")}, fingerprintsFor("apps/web/src/git.css"))
	if first != same {
		t.Fatalf("同一份输入两次算出不同摘要：%q vs %q", first, same)
	}

	// ③ 路径没变但指纹变了（同一个文件又被改了两行）—— 摘要必须跟着变
	contentChanged := map[string]string{"apps/web/src/git.css": "100644:9:634000000000000001:1699999999"}
	if gitChangesRevision([]GitChange{modified("apps/web/src/git.css")}, contentChanged) == first {
		t.Fatal("文件内容变了而摘要没变：探针看不见这次改动")
	}

	// ④ 标记位变了（同一个文件从不暂存变成暂存）—— 也要变
	staged := GitChange{Path: "apps/web/src/git.css", Staged: true, Modified: true}
	if gitChangesRevision([]GitChange{staged}, fingerprintsFor("apps/web/src/git.css")) == first {
		t.Fatal("暂存状态变了而摘要没变")
	}

	// ⑤ 长度前缀：靠分隔符拼接的实现无法区分这两组（"a|b" 的两种切法）
	if gitChangesRevision([]GitChange{modified("a"), modified("b")}, fingerprintsFor("a", "b")) ==
		gitChangesRevision([]GitChange{modified("a|b")}, fingerprintsFor("a|b")) {
		t.Fatal("带分隔符的路径造出了同一摘要")
	}

	// ⑥ 空清单也要有稳定的摘要（全新克隆的仓库就是这一档）
	empty := gitChangesRevision(nil, nil)
	if empty == "" || empty != gitChangesRevision([]GitChange{}, map[string]string{}) {
		t.Fatalf("空清单的摘要不稳定：%q", empty)
	}
}
