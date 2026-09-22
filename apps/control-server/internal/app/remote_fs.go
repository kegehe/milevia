package app

import "net/http"

// 手机端能执行的全部**文件**操作。名单类型与合成逻辑在 remote_relay.go。
//
// 这份名单就是这条通道的安全边界：多一个少一个都是边界的移动，所以
// TestFSRemoteOperationWhitelistIsExplicit 把它逐条写死在测试里。
func (s *Server) remoteFSOperations() map[string]remoteOperation {
	return map[string]remoteOperation{
		"fs.tree":          {Method: http.MethodGet, Path: "/fs/tree", Query: true, Handle: s.fsReadDir},
		"fs.open":          {Method: http.MethodGet, Path: "/fs/open", Query: true, Handle: s.fsOpenFile},
		"fs.search":        {Method: http.MethodGet, Path: "/fs/search", Query: true, Handle: s.fsSearch},
		"fs.write":         {Method: http.MethodPut, Path: "/fs/write", Handle: s.fsWriteFile},
		"fs.mkdir":         {Method: http.MethodPost, Path: "/fs/mkdir", Handle: s.fsMkdir},
		"fs.rename":        {Method: http.MethodPost, Path: "/fs/rename", Handle: s.fsRename},
		"fs.remove":        {Method: http.MethodDelete, Path: "/fs/remove", Query: true, Handle: s.fsRemove},
		"fs.sqlite.tables": {Method: http.MethodGet, Path: "/fs/sqlite/tables", Query: true, Handle: s.fsSQLiteTables},
		"fs.sqlite.schema": {Method: http.MethodGet, Path: "/fs/sqlite/schema", Query: true, Handle: s.fsSQLiteSchema},
		"fs.sqlite.rows":   {Method: http.MethodGet, Path: "/fs/sqlite/rows", Query: true, Handle: s.fsSQLiteRows},
	}
}
