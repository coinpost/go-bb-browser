# Snapshot 与 @ref（go-bb-browser）

## 目的

用短引用 **`@1` `@2`…** 代替长 CSS/XPath，减少 Agent 上下文体积；**`click` / `fill`** 可直接消费这些 ref。

## 工作流程

1. **`bb-browser snapshot`**（推荐 **`-i`** 仅可交互元素）。
2. 输出中含多行：`@N [tag ...] "可见文本"`。
3. **`bb-browser click @N`** / **`bb-browser fill @N "文本"`**。
4. **导航或 DOM 大改后重新 snapshot**，ref 与 DOM 绑定，旧 ref 可能失效。
5. **只使用最近一次 snapshot 的 ref**：每次 snapshot 都会让此前所有 ref 失效（见下）。

## 实现要点

- `snapshot` 在页面内为节点设置属性 **`__bb_snap_ref`**（值为数字字符串）。
- Daemon 侧 **`click` / `fill`** 支持 **`ref`** 字段；CLI 传入 **`@N`** 会展开为选择器：`[__bb_snap_ref="N"]`。
- 仍可使用原始 **`selector`**（CSS）而不走 snapshot。

## ref 的生命周期

- 每次返回结果的 `snapshot`（包括 `--scope` 未匹配、页面无 `body`、过滤后为空等空结果）都会先清除此前所有标记：最新结果中的每个 `@N` 在文档内恰好对应一个节点，旧 ref 不再匹配任何节点，`click` / `fill` 会报错而不是落到别的元素上。快照期间脱离文档、之后又被插回的节点同样不会带回旧 ref。
- `--scope` 的选择器在清除前解析，因此可以直接传上一次结果里的 ref 选择器（如 `-s '[__bb_snap_ref="12"]'`）。选择器非法时 `snapshot` 报错且不改动页面，上一次的 ref 继续有效。
- 编号仍是纯数字，但在同一文档内**不复用**：第一次从 `@1` 开始，之后的 snapshot 接着往上编；导航或刷新产生新文档后重新从 `@1` 开始。不要假设编号从 1 连续。
- 页面内只保留一个计数器和最近一次 snapshot 标记过的节点，不累积历史。
- 边界：snapshot 之后页面自己 `cloneNode` 带标记的节点、或 iframe / shadow DOM 内的节点不在追踪范围内；DOM 变化后请重新 snapshot。

## CLI 选项（节选）

```bash
bb-browser snapshot -i          # 仅交互元素相关子树
bb-browser snapshot -c          # 尝试省略空壳结构
bb-browser snapshot -d 5        # 限制深度
bb-browser snapshot -s ".main"  # 限定根节点（CSS）
bb-browser snapshot --json      # JSON-RPC 原始结果
```

## JSON-RPC

方法名：**`snapshot`**。`result` 含 `text`（整段可读树）、`refs`（map：`"1"` → CSS 选择器字符串）、`title`、`url`、`tab`、`seq`。
