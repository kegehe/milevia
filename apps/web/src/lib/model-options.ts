// 「模型目录 / 按模型用量」的取值收敛点。
//
// 为什么需要它：服务端用**空目录**表达"这个工具没有内置模型表"（CodeBuddy），而 Go 的
// nil 切片会序列化成 JSON `null`。界面上这两件事是同一个结果——没有候选项可选。
//
// 踩过的坑（已实测）：底部模型下拉写成 `view?.models.length`，可选链只到 view，
// models 为 null 时仍在 null 上读 .length 抛
//   Cannot read properties of null (reading 'length')
// 而模型下拉是**渲染路径**，一次抛错就让整个工作区面板被错误边界替换成兜底 UI
// （"这个面板出错了"），而 CodeBuddy 会话点开下拉必现。

/** 取响应里的 models 列表；响应缺席、字段缺席、字段为 null 一律当作空数组。 */
export function modelList<T>(value: { models?: T[] | null } | null | undefined): T[] {
  return value?.models ?? [];
}
