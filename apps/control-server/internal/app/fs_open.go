package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// 「打开一个文件」的合成契约。
//
// 桌面端的打开流程是 stat → read 两跳（先拿 isText/mimeType 判断该怎么渲染，再读内容），
// 在手机上那是两次 0.5–1.5 秒的中继往返。这里把它压成一跳：服务端自己先 Stat，
// 再按下面的上限决定给不给内容，客户端只发一条请求、只等一个结果。
//
// 上限不是拍脑袋，是从通道能力反推出来的（见 docs/40 §6）：
//
//   - 云端 Agent WebSocket 的读上限是 512 KiB（cloud-control 的 SetReadLimit）。
//     **方向是 agent → 云端**，所以超限的帧会让云端读失败并**直接掐断整条中继连接** ——
//     一个稍大的文件就能把手机与电脑的通道打断。响应必须远小于它。
//   - 内容还要经 JSON 转义（源码里的引号、换行、非 ASCII 都会膨胀）。
//   - 因此留一条 320 KiB 的查看上限、256 KiB 的可编辑上限。可编辑更低，是因为保存要
//     把内容发回去，那条方向的上限同样是 512 KiB，双向都要留余量。
//
// 刻意**不做截断阅读**：返回半个文件、又允许用户编辑它，等于让用户在不知情的情况下
// 用半截内容覆盖整个文件。要么完整给、要么明确说"太大，去电脑上看"。
const (
	fsRemoteOpenEditableLimit = 256 << 10
	fsRemoteOpenViewLimit     = 320 << 10
	fsRemoteOpenBinaryLimit   = 128 << 10
	// fsRemoteOpenTransmitLimit 是**内容转义之后**允许占用的字节数，是读完之后的硬闸门。
	//
	// 它比 fsRemoteOpenViewLimit 大，是因为两者量的不是同一件事：
	// 前者量"真正发出去的那个字符串"，后者量"文件有多大"（读之前只能知道这个）。
	// 数值来自帧上限：384 KiB（apps/agent 与 cloud-control 的同名常量）减去信封、
	// stat、version 与 requestId 的余量（几 KiB，这里留了 4 KiB）。
	//
	// 为什么非要量转义后的长度：发出去的是 JSON 字符串，而 Go 的 json 编码会把
	// `"` 与 `\` 翻成两个字节、换行翻成两个字节、控制字符翻成 `\u0001` 六个字节，
	// 还会把 `<` `>` `&` 转义成 `\u003c` 这种六字节形式。一个 200 KiB 但引号密集的
	// JSON、或者一个 300 KiB 的 HTML，转义后能到原来的两倍以上。
	//
	// ⚠️ 这正是"图片按编码后长度判断"那份教训在**文本**上的同一份，别以为只有
	// base64 会膨胀。区分标准是"发出去的是不是原始字节"：图片是（base64 已算过，
	// 且 base64 字母表里没有被转义的字符），文本不是。
	fsRemoteOpenTransmitLimit = 380 << 10
)

// 只读原因。客户端只认这几个值来决定"编辑"按钮的文案，不要在客户端另发明一套。
const (
	fsReadOnlyFileTooLarge = "file_too_large"
	fsReadOnlyBinaryFile   = "binary_file"
)

// fsOpenResponse 是 /fs/open 的响应。
//
// Content 用指针而不是 string + omitempty：**键一定在、值可能是 null** 是本项目线协议的
// 既有约定（前端按 `string | null` 钉住类型，见 MEMORY 里那条"线协议字段一律当可能为
// null 处理"）。用 omitempty 会让键时有时无，客户端就得多写一层存在性判断。
type fsOpenResponse struct {
	Stat     FileInfo `json:"stat"`
	Content  *string  `json:"content"`
	Encoding string   `json:"encoding,omitempty"`
	// Version 只有拿到完整内容时才有意义（它是内容的 sha256）。内容被省略时是空串，
	// 客户端也就不可能对它做带版本的保存 —— 这是对的：没见过内容就不该覆盖它。
	Version string `json:"version"`
	// Bytes 是**本次传输出去的内容长度**（图片走 base64 时是编码后的长度），
	// 不是文件原始大小 —— 原始大小在 Stat.Size 里。两个数都要有：前者决定这条响应
	// 离通道上限还有多远，后者是用户看到的"文件多大"。
	Bytes int `json:"bytes"`
	// OmittedReason 是"这次为什么没给内容"的**唯一判据**：空串表示内容就在 Content 里。
	// 取值只有 fsOmittedTooLarge / fsOmittedBinary。
	OmittedReason string `json:"omittedReason,omitempty"`
	// Editable 是文件**本身**能否编辑的唯一判据（太大或二进制为 false）。
	// 它与"AI 正在运行所以编辑被锁定"是两件事：后者是每次写入时才判定的动态条件
	// （workspace lease），生命周期不同，不能合成一个字段。
	Editable       bool   `json:"editable"`
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
}

const (
	fsOmittedTooLarge = "too_large"
	fsOmittedBinary   = "binary"
)

// fsOpenFile 一次返回元信息与（在限额内的）内容。
func (s *Server) fsOpenFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}
	filesystem, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	info, err := filesystem.Stat(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if info.IsDir {
		writeError(w, http.StatusBadRequest, errors.New("路径是目录，不是文件"))
		return
	}

	response := fsOpenResponse{Stat: info}
	// 先按 Stat 的大小决定要不要读，而不是"读了再说"：ReadFile 会把整个文件装进内存，
	// 对一个 8MB 的文件先读再判断上限，白付一次全量读取与内存峰值。
	if reason := fsOmittedByMetadata(info); reason != "" {
		response.Content = nil
		response.OmittedReason = reason
		response.Editable = false
		response.ReadOnlyReason = fsReadOnlyReason(reason)
		writeJSON(w, http.StatusOK, response)
		return
	}

	content, err := filesystem.ReadFile(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Stat 与 Read 之间两件事都可能发生：文件被写大了，或者它本来就是 base64 编码的
	// 二进制（编码后比原始大小多约三分之一）。所以这一道量的是**真正要发出去的长度**，
	// 而不是文件大小 —— 拿原始大小去比，一张 100 KiB 的 PNG 会被误判成"没超限"，
	// 而它编码后是 137 KiB，正是这条响应能不能装进通道的差别。
	//
	// 文本同样要量转义后的长度（见 fsRemoteOpenTransmitLimit）：它的 `"`、换行、
	// `<` 都会在 JSON 里变成更长的一串，原始长度只是**下界**。
	//
	// 两种情形共用 too_large 这个原因：对用户来说事实是同一件（这份内容手机端带不动，
	// 去电脑上看），给他一个更细的机器码只会让界面多一条分支、少一句人话。
	if escapedJSONLength(content.Content) > fsRemoteOpenTransmitLimit {
		response.Content = nil
		response.OmittedReason = fsOmittedTooLarge
		response.Editable = false
		response.ReadOnlyReason = fsReadOnlyFileTooLarge
		writeJSON(w, http.StatusOK, response)
		return
	}

	response.Stat = content.Stat
	response.Content = &content.Content
	response.Encoding = content.Encoding
	response.Version = content.Version
	response.Bytes = len(content.Content)
	// 可编辑的判据同样落在**传输长度**上：保存要把这段内容原样发回去，能读回来
	// 却发不出去的文件不该被标成可编辑。
	response.Editable = content.Stat.IsText && len(content.Content) <= fsRemoteOpenEditableLimit
	if !response.Editable {
		// 原因必须按"到底为什么"分，不能一律说太大：图片和二进制不是"太大"，
		// 说成太大等于给了用户一个改不掉的理由（他没法把它弄小）。
		// SVG 是文本（IsText 为 true），所以它走得到这里且保持可编辑。
		if !content.Stat.IsText {
			response.ReadOnlyReason = fsReadOnlyBinaryFile
		} else {
			response.ReadOnlyReason = fsReadOnlyFileTooLarge
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// fsOmittedByMetadata 只凭元信息判断是否可以省掉这次读取；返回空串表示"可以读"。
//
// 读之前的闸门（省一次全量读取）与读之后的闸门（量传输长度）是两道，不能合并：
// 前者用文件原始大小，后者必须用编码后的长度 —— 图片会 base64 膨胀，
// 两者的差正好在 128 KiB 这一档上翻车。
func fsOmittedByMetadata(info FileInfo) string {
	if !info.IsText {
		if strings.HasPrefix(info.MimeType, "image/") && info.Size <= fsRemoteOpenBinaryLimit {
			return ""
		}
		// 其它二进制一律不给内容：手机端没有下载通道，给了也只是内存里的一块死数据。
		return fsOmittedBinary
	}
	if info.Size > fsRemoteOpenViewLimit {
		return fsOmittedTooLarge
	}
	return ""
}

// escapedJSONLength 返回这段字符串放进 JSON **之后**占用的字节数（不含两端引号）。
//
// 用 json.Marshal 而不是手写一遍转义表：量出来的必须与真正序列化时用的那套规则
// 一致（同一份 encoding/json 的默认行为，HTML 转义开着 —— control-server 的 writeJSON
// 用的就是 json.NewEncoder，两边一致）。自己写一份，早晚会因为某个字符的规则不同
// 而低估，而低估正好是这里最不能犯的错。
//
// 代价是给这段内容再分配一份等长（或更长）的副本。只发生在 ≤320 KiB 的读路径上，
// 换来的是"发出去之前就知道装不装得下"。
func escapedJSONLength(text string) int {
	encoded, err := json.Marshal(text)
	if err != nil {
		// string 的 Marshal 不会失败（utf8 非法也只是替换成 U+FFFD）。
		// 真失败就退回原始长度：那是个**下界**，宁可少判也不能凭空拦下能读的文件。
		return len(text)
	}
	// 减掉两端引号。
	if len(encoded) < 2 {
		return 0
	}
	return len(encoded) - 2
}

func fsReadOnlyReason(omitted string) string {
	if omitted == fsOmittedBinary {
		return fsReadOnlyBinaryFile
	}
	return fsReadOnlyFileTooLarge
}
