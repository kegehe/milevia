# 对话输入历史位置标记设计

## 目标

在对话输入框右上角显示当前输入历史浏览位置，帮助用户确认上下箭头当前翻到了哪一条。输入历史最多保留 100 条，但展示实际已加载的条数。

示例：

- 共有 100 条历史，最近一条显示 `100/100`，继续向上翻显示 `99/100`。
- 共有 3 条历史，最近一条显示 `3/3`。
- 当前回到最新空输入时不显示标记。

## 现有状态

`ConversationPage` 已经通过以下状态实现输入历史浏览：

- `inputHistory: string[]`：从项目输入历史接口加载，最多 100 条。
- `historyIndex: Ref<number | null>`：当前历史索引；`null` 表示最新输入状态。
- `draftBeforeHistory`：首次进入历史浏览前保存的用户草稿。
- `navigateInputHistory`：处理 textarea 的 ArrowUp/ArrowDown，并在离开最后一条历史时恢复草稿。

因此不新增独立的计数状态，避免索引和展示状态不同步。

## 交互与数据流

1. 首次按 ArrowUp 时，现有逻辑将索引设为 `inputHistory.length - 1`。显示分子 `inputHistory.length - historyIndex`，即最近一条为总数。
2. 继续按 ArrowUp 时，索引递减，显示位置同步递减。
3. 按 ArrowDown 到达历史列表末端后，现有逻辑将索引重置为 `null`、恢复 `draftBeforeHistory`；标记不渲染。
4. 当历史列表为空、切换会话、发送消息、清空上下文或恢复草稿导致索引为 `null` 时，标记不渲染。
5. 分母始终使用 `inputHistory.length`，所以少于 100 条时展示实际总数。

## 组件与布局

在现有 `.composer-input-area` 内增加只读的 `.composer-history-position` 元素，放在输入区右上角。输入区设为相对定位，标记使用绝对定位；通过输入区右侧和顶部的内边距/间距保证不遮挡输入文字和移动端快捷操作按钮。

渲染条件为 `historyIndex.current !== null && inputHistory.length > 0`。由于 `historyIndex` 是 ref，键盘处理后需要触发已有的输入状态更新来完成重渲染；若现有状态更新不足，则增加一个最小的展示同步状态，不能改变历史导航的单一数据源。

标记采用小号、低对比度、等宽数字风格，使用 `aria-label` 表达完整含义，例如“输入历史第 99 条，共 100 条”，不增加可操作性，也不参与表单提交。

## 边界与兼容性

- 输入历史接口返回的数组已由现有逻辑限制为最多 100 条，标记不自行截断。
- 文本内容、换行判断、组合输入法和现有 ArrowUp/ArrowDown 行为保持不变。
- 消息时间线上的“上一条我的消息/下一条我的消息”按钮属于另一套导航，不复用此计数。
- 桌面端与移动端共用同一 composer，样式在窄屏下仍保持右上角位置；必要时缩小标记间距，不覆盖快捷操作按钮。

## 验证

- 增加针对页面源码的测试，验证标记条件、`length - index` 计算、实际总数和 `null` 隐藏条件。
- 执行 `pnpm --filter @milevia/web test`。
- 执行 `pnpm --filter @milevia/web build`，确认 TypeScript 和 Vite 构建通过。
