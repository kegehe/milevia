module github.com/kegehe/milevia/apps/control-server

go 1.26.2

require (
	github.com/go-chi/chi/v5 v5.2.3
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-sqlite3 v1.14.44
	github.com/pkg/sftp v1.13.11
	golang.org/x/crypto v0.54.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/kr/fs v0.1.0 // indirect
	golang.org/x/text v0.40.0
)

// 仅为 GBK→UTF-8 转码使用 x/text 的 simplifiedchinese 包。依赖链
// pkg/sftp → x/crypto v0.54.0 → x/text v0.40.0 把 MVS 最低版本推到 v0.40，但实际上
// 我们只用 v0.36（构建时已验证 GBK 转码所需 API 完整、全部测试通过）。锁到 v0.36
// 以避免拉取不在本机构建缓存里的 v0.40 源码，无需为此改动任何安全相关的一级依赖。
replace golang.org/x/text => golang.org/x/text v0.36.0
