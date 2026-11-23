# daemonclient

`daemonclient` 是 **bb-daemon** 的 Go HTTP 客户端：通过 **JSON-RPC 2.0** 调用 `POST /v1`，并提供 `Live`、`Ready`、`Health` 三种健康检查。一台 daemon 用 `NewClient`；多台用 `NewPool`（见下文）。协议字段与 **`pkg/protocol`**（包名 `protocol`）中的类型、方法名一致，与仓库根目录 `AGENTS.md` 中描述的守护进程行为对齐（完整 CDP target id 作为 tab id、全局单调 `seq`、观测类接口的 `cursor` 等）。

## 适用场景

- 需要类型安全地调用守护进程时，使用本包的封装方法，并导入 **`github.com/yiplee/go-bb-browser/pkg/protocol`** 获取 `Params` / `Result` 与方法名常量；该路径**可被模块外项目正常 `import`**。
- 若不想依赖 `protocol` 类型，仍可使用 `Call`，自行定义与 JSON 响应形状一致的 `result` 结构体，并把 `params` 设为 `map[string]any` 或可 `json.Marshal` 的任意值。

## 安装与导入

```go
import (
    "github.com/yiplee/go-bb-browser/pkg/daemonclient"
    "github.com/yiplee/go-bb-browser/pkg/protocol"
)
```

守护进程根地址示例：`http://127.0.0.1:8080`（不要带尾部路径；客户端会自行拼接 `/live`、`/ready`、`/health` 与 `/v1`）。

## 构造客户端

```go
c := daemonclient.NewClient("http://127.0.0.1:8080")
```

daemon 配置 API token 白名单后，每个客户端用 `daemonclient.WithAPIToken(token)` 发送其中一个 token，等价于 `WithHeader("Authorization", "Bearer "+token)`。本包不自动读取环境变量；调用方提供 token。鉴权失败返回标准 HTTP 401（正文 `unauthorized\n`，非 JSON-RPC），可用 `errors.As` 检查 `*HTTPError` 的 `StatusCode`。失败请求不会执行 RPC 或写 `rpc.jsonl`；未配置白名单时行为不变，`/live`、`/ready`、`/health` 免 token（客户端仍会发送已配置的 headers）。

```go
c := daemonclient.NewClient("http://127.0.0.1:8080", daemonclient.WithAPIToken("<caller-token>"))
```

`NewClient` 会去掉首尾空白，并去掉 `BaseURL` 末尾的 `/`。第二个及之后的参数为可选的 `ClientOption`，例如为所有请求附加 HTTP 头（鉴权、自定义网关等）：

```go
import "net/http"

c := daemonclient.NewClient("http://127.0.0.1:8080",
    daemonclient.WithHeaders(http.Header{
        "Authorization": []string{"Bearer token"},
    }),
    daemonclient.WithHeader("X-Request-Id", "abc"),
)
```

`WithHeader` / `WithHeaders` 以及多个同类 option 之间均为**合并**（内部使用 `Header.Add`），不会彼此覆盖。也可在构造后直接操作 `c.Headers`（与 `HTTP` 字段一样，由调用方自行维护该 map）。

可选字段：

| 字段 | 含义 |
|------|------|
| `BaseURL` | 守护进程 HTTP 根 URL |
| `HTTP` | 若非 `nil`，用于所有请求；否则使用 `http.DefaultClient` |
| `Headers` | 若非空，发往 `Live` / `Ready` / `Health` / `Call` 时用 `Header.Set` 写入（同一键在 `Headers` 中存多条时，取 slice 中最后一项）；`Call` 随后会 `Set` `Content-Type: application/json`，因此调用方无法覆盖 JSON-RPC 所需的 Content-Type |

`WithHeader(k, v)` 与 `WithHeaders(h)` 把条目合并进 `Headers`（`h` 为空或 `len(h)==0` 时不做任何事）。

每个 `Client` 内部使用单调递增的 JSON-RPC `id`（`uint64` 序列化），与单次调用的业务 `seq` 无关。

## 多 daemon：`Pool`

若干独立的 `bb-daemon`（各自 Chrome / `--debugger-url`）不能共享 tab。`Pool` 在客户端做负载分担，并把 **tab 钉在创建它的那一个 daemon 上**。

**Tab id**：daemon 的 tab id 是**完整 CDP target id**（不透明字符串，原样精确匹配；旧版短 id 已移除，见仓库 [README](../../README.md)）。单 `Client` 的 `TabNew` / `TabList` / `TabFocus` 直接返回它，后续调用原样回传即可；未知或过期的 id 得到 `*RPCError`（`unknown tab id`），不会回退到其他 tab。

**Pool 对外 tab id**（仅 Pool 表面）格式为：

```text
<backendKey>:<targetId>
```

其中 `targetId` 即该 daemon 返回的完整 CDP target id。

- `backendKey` 默认是 `NewPool` 参数顺序的 **0-based 下标**（`"0"`、`"1"`、…）。也可用 `NewPoolBackends` 配稳定名字（不可含 `:`，不可重复），例如 `"west:E3B0C44298FC1C149AFBF4C8996FB924"`。
- 调用方把 `TabNew` / `TabList` / `TabFocus` 返回的 id 原样传回后续 tab 操作。Pool **按前缀路由**到对应后端，发给 daemon 时去掉前缀、只带完整 target id。Pool 对 id 逐字节精确匹配（不去空白、不折叠大小写、不做部分匹配），`TabList` / `TabFocus` 观察到的 id 也按返回值原样绑定；**裸 target id 不被接受**——没有前缀就无法确定后端，Pool 不会猜。
- 不同 daemon 即使返回相同的 target id，在 Pool API 边界上也不会混淆。格式错误或未知 `backendKey` 会返回 `*InvalidTabIDError`；格式正确但不是本池创建或列出（或已关闭 / `ForgetTab`）的 id 仍为 `*UnknownTabError`。

```go
pool, err := daemonclient.NewPool(
    daemonclient.NewClient("https://daemon-a.example",
        daemonclient.WithAPIToken("<daemon-a-token>"),
        daemonclient.WithHeader("CF-Access-Client-Id", idA),
        daemonclient.WithHeader("CF-Access-Client-Secret", secretA),
    ),
    daemonclient.NewClient("https://daemon-b.example",
        daemonclient.WithAPIToken("<daemon-b-token>"),
        daemonclient.WithHeader("CF-Access-Client-Id", idB),
        daemonclient.WithHeader("CF-Access-Client-Secret", secretB),
    ),
)
if err != nil {
    return err
}

out, err := pool.TabNew(ctx, protocol.TabNewParams{URL: "https://example.com"})
// out.Tab 形如 "0:E3B0C44298FC1C149AFBF4C8996FB924"（<backendKey>:<完整 CDP target id>）；
// Eval / Goto / TabClose / … 必须原样带这个 id
_, err = pool.Eval(ctx, protocol.EvalParams{Tab: out.Tab, Script: "document.title"})
```

`NewClient` 的单 daemon API 不变。`Pool` 提供同一套 `Health` / `Call` / 类型化 RPC 方法。

| 策略 | 行为 |
|------|------|
| **启动** | `NewPool` **不**探测后端。部分 daemon 当时不可用也可以构造成功。 |
| **无 tab 的请求** | `Health` / `Ready` / `Live`、`tab_new`：选 **in-flight 最少**的后端（并列时 round-robin），失败则试下一个；全部失败返回 `*AllFailedError`。`tab_new` 的 failover 只表示「新 tab 开在另一个 daemon」，**不会**把已有 tab 挪走。 |
| **tab_new 均摊** | `tab_new` 的轮询游标独立于 `Health` / `Ready` / `Live` 及其它无 tab 调用，「先探活再开 tab」不会让 tab 集中到同一个 daemon。后端按**每次尝试**选取：在本请求尚未试过的后端中取 in-flight 最少者，并列时从游标起按环序取第一个；每次尝试（无论成败）都把游标移到所选后端之后，每个请求对每个后端最多尝试一次。因此故障后端的份额在其余健康后端间均分（顺序调用时差值 ≤1），并发请求在故障转移时也不会重复占用或跳过其它请求尚未用到的健康后端；后端恢复后直接回到轮询，不补发。选取只在一把短锁内完成，HTTP 调用不持锁。长时间挂起（不返回错误）的后端、长期不均的 in-flight 负载不在此保证之内。 |
| **tab_list** | **并发查询每一个后端**，把各端 tab id 加上前缀后按 **构造顺序**（而非应答先后）合并。某端失败则 **跳过**（该端 tab 不会出现在结果里），其余端仍返回；全部失败才是 `*AllFailedError`。空列表算成功。成功端的 `tabs` 数组用于对账，旧绑定的回收规则见下文「绑定生命周期」。Pool 没有跨 daemon 的焦点：`tab` / `focus` 取构造顺序上第一个非空值。`seq` 为成功端中的最大值。 |
| **tab_focus** | 并发查询所有后端，取构造顺序上 **第一个成功**的后端（跳过失败端；不是最快应答者），返回前缀化的 `tab` / `focus` 并绑定。排在它前面的后端都已应答后立即返回，不等后面的后端。局部观察不会回收其它绑定。不是池级焦点。全部失败返回 `*AllFailedError`。 |
| **tab_list / tab_focus 超时** | 这两个聚合查询最多等待 **2 秒**（调用方 deadline 更短则以其为准；即使调用方 deadline 更长或没有 deadline 也是 2 秒）。到时仍未应答的后端按失败 **跳过** 并取消其请求，已收到的成功结果照常返回，所以一个接受连接但不应答的 daemon 只会让调用变慢到该上限，不会让它失败或永久阻塞。没有任何成功结果时：调用方 deadline 已到则返回其 context 错误，否则返回 `*AllFailedError`（超时端记为 `context.DeadlineExceeded`）。调用前 context 已结束则不发请求；调用中 **主动取消** context 会立即返回 `context.Canceled`，丢弃已收到的结果、不写入映射、也不改动调用方传入的 `result`：前缀化结果与整批绑定变更（含回收）先暂存（结果在锁外解码到一个新值），最后一次取消检查在池的 tab 映射锁内、紧挨着提交之前进行，因此在并发查询、结果准备或等锁期间发生的取消都生效；只有晚于这次检查的取消来不及生效（调用按成功返回并已绑定）。deadline 到达不算主动取消，预算内的成功结果照常提交。成功时暂存值整体替换 `*result`（不与原值合并）。其它 Pool 方法和单 `Client` 没有内置超时。 |
| **有 tab 的请求** | **硬性**：必须打到 **该前缀对应的同一个 daemon**（含 `tab_select`）。池对外 id 为 `<backendKey>:<targetId>`（`TabNew` / `TabList` / `TabFocus` 成功时写入映射；`TabClose` 成功、`ForgetTab`、或该端之后成功的 `TabList` 不再列出时删除）；发给 daemon 时去掉前缀。对端失败 **不会**改道到其它 daemon，也不做跨 daemon 转发。未知（未创建/未列出/已关闭/已被列表对账移除）tab 返回 `*UnknownTabError`；该 tab 在池外被关闭（直接调 daemon、idle 清理、在浏览器里关掉）而绑定尚未回收时，请求仍只发往它所属的 daemon，daemon 明确答复的 `unknown tab id` 同样以 `*UnknownTabError` 返回（`Tab` 为池对外 id，`errors.As` 仍可取到底层 `*RPCError`；其它 RPC 错误原样返回，绑定的回收仍按下文规则；daemon 因自己查询 target 列表失败/超时而答复的 `failed to list targets` 属于这一类，不表示 tab 不存在，绑定保留，可稍后重试）。格式错误或未知前缀返回 `*InvalidTabIDError`。省略 `tab` 会得到 `ErrTabRequired`。 |

`Pool.Clients()` 可拿到各 `*Client`（每端独立 URL / `Headers`）。`TabClose` 成功后解除亲和；daemon 空闲关 tab 时可用 `ForgetTab`。`BackendKeys()` 返回构造顺序下的前缀。

### 绑定生命周期

映射不是只增不减：池外关闭的 tab（空闲回收、用户手动关闭、其它客户端 `tab_close`）会在该后端下一次成功的 `TabList` 时被移除，之后对旧 id 的调用在本地返回 `*UnknownTabError`，不再发 RPC。

- **权威集合只有 `tabs` 数组**。列表结果里的 `tab` / `focus` 元数据既不新增绑定，也不能让已不在 `tabs` 中的 tab 继续存活；只有显式的空数组 `"tabs": []` 才清空该后端的旧绑定。成功响应里 `tabs` 缺失、为 `null` 或不是数组时，该端按**失败**处理：不对账、不并入结果，全部后端如此则返回 `*AllFailedError`。
- **失败不对账**：某端 `tab_list` 失败时保留该端全部绑定；各后端互不影响。`TabFocus` 永不移除绑定。
- **请求代次**：Pool 维护池内逻辑时钟（只在短锁内读写，锁从不跨网络 I/O）。每次观察（`tab_list` / `tab_focus`）在**发出时**取得代次，它报告的内容无论响应何时到达，都只算作该代次的证据；每个绑定记录其最新证据的代次（`TabNew` 为绑定完成的时刻）。
  - 每个后端记住已采纳的**最新列表代次**。比它更早发出的列表 / 焦点晚到时整体忽略：既不能保留或新增较新列表已不再列出的 tab（包括从未绑定过的 id），也不移除任何绑定。
  - 采纳一次列表时，只移除该端「最新证据早于该列表代次」的绑定。列表在途期间由 `TabNew` 建立、或由**之后发出**的 `TabList` / `TabFocus` 确认的绑定不会被误删；即使较旧列表先到并移除了某 tab，较新观察到达后也会把仍存活的 tab 重新绑定，返回的 id 立即可用。
  - `TabClose` 成功与 `ForgetTab` 以**生效时刻**为准：此前已发出、仍在途的观察不会把该 id 绑回，之后发出的观察可以。为此保留的移除标记只在该后端存在「早于该移除」的在途观察时存在；每个观察返回时回收早于「最早在途观察」的标记，全部返回后清空，因此持续交叠的轮询（每个请求本身有限）也不会累积标记。显式移除与列表对账互不混用：列表对账不产生标记。
- **限制**：先后按本池「发出请求」的顺序判定，而不是 daemon 取快照的顺序（不使用 daemon `seq`，避免重启后计数重置的影响）；两个交叠请求若被 daemon 反序处理，仍以后发出的为准。池外关闭的 tab、以及列表持续失败的后端上的 tab，会一直保留到该端下一次成功列表，期间对这类 id 的调用会发到 daemon 并得到 daemon 的 RPC 错误。被忽略的晚到旧列表仍会原样返回给它的调用方，其中可能含有 Pool 未绑定的 id（调用得到 `*UnknownTabError`）。单个观察在途期间，其后的显式移除标记会保留到观察结束。聚合调用返回时即结束本次所有观察，即使超时或未选中的后端仍在处理请求。
- `ForgetTab` 之后若 daemon 仍列出该 tab，**之后发起**的 `TabList` / `TabFocus` 会把它重新绑定。

每个 backend 的 `WithAPIToken` 独立生效，Pool 不共享 token；后续 tab 操作仍发送到原 backend。

各 daemon 的 tab id 互相独立。Pool 用前缀把它们变成互不混淆的对外 id，**不再**靠先到先得的 close-on-collide 作为主策略（`TabCollisionError` 仅作防御）。`Call(ctx, "tab_new", params, nil)` 与非 `protocol.TabNewResult` 的 result 指针只要能解码 JSON 对象，成功时同样写入映射并把 `tab` 改写成前缀形式；无法解码的 result 会在发请求前被拒绝。`tab_list` 合并所有后端并前缀化；`tab_focus` 返回第一个健康后端的前缀化焦点。

## 健康检查

```go
if err := c.Live(ctx); err != nil { // 进程 liveness，不访问 CDP
    return err
}
if err := c.Ready(ctx); err != nil { // 接流量前的 CDP readiness
    // 非 200：*daemonclient.HTTPError
}
```

- `Live` 请求 `/live`，只接受 `{"status":"ok"}`，适合进程 supervisor。
- `Ready` / `ReadyResult` 请求 `/ready`；`suspect` 或 `failed` 时为 HTTP 503，适合流量 readiness。
- `Health` / `HealthResult` 请求兼容的 `/health`；`suspect` 仍为 connected/200，confirmed failed 才为 disconnected/503。
- `ReadyResult` / `HealthResult` 成功时解析并返回 `protocol.HealthResult`；非 200 返回包含响应体的 `*daemonclient.HTTPError`。

## 通用调用：`Call`

```go
err := c.Call(ctx, method, params, resultPtr)
```

| 参数 | 说明 |
|------|------|
| `method` | JSON-RPC `method` 字符串，与 `protocol.Method*` 常量一致（如 `protocol.MethodTabList`） |
| `params` | 任意可 `json.Marshal` 的值；`nil` 时等价于 `{}` |
| `result` | 成功时解码 `result` 字段的目标指针；若为 `nil`，在响应无 `error` 时忽略 `result`（若仍缺少 `result` 会报错，见下） |

行为要点：

- 请求：`POST {BaseURL}/v1`，先用 `Header.Set` 写入 `Client.Headers` 中的项，再设置 `Content-Type: application/json`，体为单对象 JSON-RPC 请求（`jsonrpc`、`method`、`params`、`id`）。
- HTTP 层非 200：返回 `*HTTPError`（含状态码与响应体文本）。
- HTTP 200 且 JSON-RPC 含 `error`：返回 `*RPCError`。
- HTTP 200、`error` 为空但 `result` 缺失且调用方需要解码：返回 `fmt.Errorf("json-rpc: missing result")`。

## 已封装方法（类型化 RPC）

以下方法均为 `Call` 的薄封装，方法名与 `pkg/protocol/jsonrpc.go` 中的常量一致。参数与返回值含义以该文件中的注释为准。

### Tab 与导航

| 客户端方法 | JSON-RPC `method` | 参数类型 | 结果类型 |
|------------|-------------------|----------|----------|
| `TabList` | `tab_list` | `TabListParams` | `TabListResult` |
| `TabFocus` | `tab_focus` | `TabFocusParams` | `TabFocusResult` |
| `TabSelect` | `tab_select` | `TabSelectParams` | `TabSelectResult` |
| `TabNew` | `tab_new` | `TabNewParams`（可选 `url`、`silent`） | `TabNewResult` |
| `TabClose` | `tab_close` | `TabCloseParams` | `TabCloseResult` |
| `Goto` | `goto` | `GotoParams` | `GotoResult` |
| `Reload` | `reload` | `ReloadParams` | `ReloadResult` |

### 页面操作与脚本

| 客户端方法 | JSON-RPC `method` | 参数类型 | 结果类型 |
|------------|-------------------|----------|----------|
| `Screenshot` | `screenshot` | `ScreenshotParams` | `ScreenshotResult`（`data` 为 base64） |
| `Eval` | `eval` | `EvalParams` | `EvalResult`（`result` 为 `json.RawMessage`） |
| `Click` | `click` | `ClickParams`（可选 `ref` 与 snapshot 联动） | `ClickResult` |
| `Fill` | `fill` | `FillParams` | `FillResult` |
| `Snapshot` | `snapshot` | `SnapshotParams` | `SnapshotResult`（含 `refs` 映射） |
| `Fetch` | `fetch` | `FetchParams` | `FetchResult` |

### 观测：网络 / 控制台 / 错误

| 客户端方法 | JSON-RPC `method` | 参数类型 | 结果类型 |
|------------|-------------------|----------|----------|
| `Network` | `network` | `ObsQueryParams`（`tab` + 可选 `since`） | `ObsQueryResult` |
| `Console` | `console` | 同上 | 同上 |
| `Errors` | `errors` | 同上 | 同上 |

`ObsQueryResult` 含 `events`、`cursor`、可选 `dropped`，与 [AGENTS.md](../../AGENTS.md#design-invariants-ported-from-bb-browser) 的 INV-2 及 [daemon-jsonrpc.md](../../skills/bb-browser/references/daemon-jsonrpc.md#观测类语义) 的 `since` / `cursor` 增量观测语义一致。

### 观测缓冲清理与网络拦截

| 客户端方法 | JSON-RPC `method` | 参数类型 | 结果类型 |
|------------|-------------------|----------|----------|
| `NetworkClear` | `network_clear` | `NetworkClearParams` | `NetworkClearResult` |
| `ConsoleClear` | `console_clear` | `ConsoleClearParams` | `ConsoleClearResult` |
| `ErrorsClear` | `errors_clear` | `ErrorsClearParams` | `ErrorsClearResult` |
| `NetworkRoute` | `network_route` | `NetworkRouteParams` | `NetworkRouteResult` |
| `NetworkUnroute` | `network_unroute` | `NetworkUnrouteParams` | `NetworkUnrouteResult` |

## 错误处理

### `*RPCError`（HTTP 200，业务或协议错误）

```go
var re *daemonclient.RPCError
if errors.As(err, &re) {
    // re.Code, re.Message, re.Data (json.RawMessage)
    var data protocol.ErrData
    if err := re.UnmarshalData(&data); err == nil {
        // data.Error, data.Hint, data.Method
    }
}
```

常用 JSON-RPC 错误码在 `protocol` 中定义，例如：`CodeMethodNotFound`（-32601）、`CodeInvalidParams`（-32602）等。

`UnmarshalData` 在 `data` 为空或为 JSON `null` 时返回错误。

### `*HTTPError`（非 2xx）

含 `StatusCode` 与 `Body`。`Error()` 在正文过长时会截断显示。

### 其它错误

网络失败、JSON 编解码失败、`marshal params` 等会以普通 `error` 链形式返回，可用 `errors.Unwrap` 追溯。

## 与本仓库其它部分的关系

- **协议真相源**：`pkg/protocol/jsonrpc.go`（方法名、params/result 形状、错误码、`ErrData`）。
- **守护进程**：`cmd/bb-daemon`；HTTP 与 JSON-RPC 分发在 `internal/daemon` 等包中实现。
- **测试**：见 `client_test.go`、`pool_test.go`（`httptest` 模拟 `/health` 与 `/v1`）。

## 简短示例

```go
ctx := context.Background()
c := daemonclient.NewClient("http://127.0.0.1:8080")
if err := c.Health(ctx); err != nil {
    return err
}

tabs, err := c.TabList(ctx, protocol.TabListParams{})
if err != nil {
    return err
}
_ = tabs.Seq   // 全局单调 seq
_ = tabs.Focus // 当前焦点 tab（若有）

out, err := c.TabNew(ctx, protocol.TabNewParams{URL: "about:blank"})
if err != nil {
    return err
}
tabID := out.Tab

_, err = c.Goto(ctx, protocol.GotoParams{Tab: tabID, URL: "https://example.com"})
if err != nil {
    return err
}
```

使用 `errors.As` 区分 `RPCError` 与 `HTTPError` 便于在 CLI 或自动化脚本中给出可读输出。
