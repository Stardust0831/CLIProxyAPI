# modeltrace-guard（中文说明）

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件：通过 [ModelTrace](https://github.com/xqy2006/ModelTrace) 长整数指纹，监视路由情况并检测账号"降智"。

插件以 CGO 动态库进程内运行：通过 usage 钩子记录每个已完成请求，定时用自己的凭证跑 ModelTrace 三挑战长整数探针，把数字输出对照已登记的指纹库评分，并通过 Management API 和浏览器可访问的资源面板给出判定。

## 功能

- **路由记录（usage 钩子）** — 每个已完成请求被脱敏为一条紧凑记录：provider、请求模型、上游响应报告的模型、账号（打码）、延迟/首字延迟、token 计数、失败码。回答"这条请求实际由哪个账号服务，上游说它是什么？"
- **指纹探针（host.model.execute）** — 按计划或按需，每个活跃凭证收到最多 3 个长整数生成挑战（与 ModelTrace 建库完全一致的提示词，1..355 闭区间）。执行通过 `AuthID` + `ForcedProvider` 锁定到精确凭证，探针不会干扰正常路由。
- **闭集归因** — 数字输出用 ModelTrace 流水线评分（去环境方向的 Hellinger 模型中心相似度 + 0.25 × 有序分块数字序列特征，再按库内校准温度 softmax）。top-1 模型与你为该 provider 配置的模型对比：`match` / `mismatch`（降智信号）/ `unlabeled` / `insufficient`。
- **Management API** — `/v0/management/modeltrace-guard/...` 下的认证 JSON 路由 + 无认证的仪表盘资源。

移植保真度：Go 评分器把 ModelTrace 参考语料中 **462/468（98.7%）** 的输出归因回记录的模型（`go test -run TestReferenceAttribution`）。

## 构建

需要 Go 1.26+ 和 CGO（插件是 c-shared 库）。

```bash
# 仓库根目录（本文件夹）
go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.so .

# macOS
CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.dylib .

# Windows
CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o modeltrace-guard.dll .
```

构建同时会生成 C 头文件（`modeltrace-guard.h`），仅供参考，运行时不需要。

`go.mod` 用 `replace` 指向 `./upstream/CLIProxyAPI`（参考克隆）。若想基于已发布版本构建，删除 `replace` 行后执行 `go mod tidy`。

## 安装

1. 把编译好的动态库放进宿主插件目录：

```text
CLIProxyAPI/
└── plugins/
    └── linux/
        └── amd64/
            └── modeltrace-guard.so
```

文档接受 `plugins/<GOOS>/<GOARCH>/`（推荐）或平铺的 `plugins/` 目录。

2. 把 `data/unified_bank.json` 放到宿主可读的位置（默认按宿主工作目录下的 `data/unified_bank.json` 查找），或用 `bank_path` 指向任意绝对路径。本仓库 `data/` 下自带一份，来自 ModelTrace 项目。

3. 在 `config.yaml` 中启用插件：

```yaml
plugins:
  enabled: true
  configs:
    modeltrace-guard:
      enabled: true
      bank_path: "data/unified_bank.json"
      interval_minutes: 60
      probes_per_run: 3
      providers:
        - codex
        - claude
      models:
        codex: gpt-5.6
        claude: claude-opus-5
```

4. 重启宿主并验证注册：

```bash
curl -s -H "Authorization: Bearer <management-key>" http://127.0.0.1:8317/v0/management/plugins | jq '.[] | select(.id=="modeltrace-guard")'
# 期望：registered: true, effective_enabled: true
```

## 配置项

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `bank_path` | `data/unified_bank.json` | ModelTrace 统一指纹库路径。库会缓存，文件变化时自动重载。 |
| `interval_minutes` | `60` | 自动探针间隔（分钟）；`0` 关闭计划探针（手动 `POST .../probe` 仍可用）。低于 15 会被钳到 15 以保护配额。 |
| `probes_per_run` | `3` | 每个凭证每轮探针挑战数（1-3）。越多越准（校准表按 1/2/3 输出分键），但配额消耗越大。 |
| `providers` | *(全部)* | provider 过滤（匹配 `provider` 或 `type`），如 `codex`、`claude`。 |
| `auth_ids` | *(全部)* | 可选的精确凭证过滤（匹配 `id` 或 `auth_index`）。 |
| `models` | *(无)* | provider → 预期模型 id，作为归因基线。不配置则该轮报告为 `unlabeled`（检测仍会展示）。 |
| `environments` | `[1, 5, 6]` | 从哪些套件环境（1-12）抽探针；默认是 clean-transport 的三个（无 system 前缀、无 user 前缀）。 |
| `history_path` | *(关闭)* | 可选 JSONL 文件，追加每条探针记录。 |
| `history_size` | `200` | 内存中探针记录环形缓冲大小。 |
| `routing_size` | `1000` | 内存中路由记录环形缓冲大小。 |
| `entry_protocol` / `exit_protocol` | `openai` | 探针请求应用的协议翻译。 |

运行时也可以通过 `PUT /v0/management/modeltrace-guard/config`（JSON body，字段同上）动态更新。

## 管理路由

认证（management key）JSON 路由：

| 路由 | 说明 |
| --- | --- |
| `GET /v0/management/modeltrace-guard/status` | 插件/指纹库/探针状态摘要。 |
| `GET /v0/management/modeltrace-guard/history?limit=N` | 最近的探针记录（完整详情，不打码）。 |
| `GET /v0/management/modeltrace-guard/routing?limit=N` | 最近的逐请求路由记录。 |
| `GET /v0/management/modeltrace-guard/config` | 当前运行时配置。 |
| `PUT /v0/management/modeltrace-guard/config` | 更新运行时配置字段（JSON body）。 |
| `POST /v0/management/modeltrace-guard/probe` | 立即触发一轮探针（202，异步）。 |
| `GET /v0/management/modeltrace-guard/bank` | 已加载指纹库摘要（模型、校准、哈希）。 |

无认证的浏览器可访问资源：

- `GET /v0/resource/plugins/modeltrace-guard/dashboard` — 服务端渲染仪表盘：指纹库信息、带徽章的最近探针判定、路由概览、最近请求。此页面账号标识已打码；完整记录请用认证路由。

## 判定与注意事项

- `match` — 检测到的 top-1 与该 provider 配置的模型一致。
- `mismatch` — 指纹库中其他模型得分最高：视为降智信号，并与路由记录的 `response_model` 交叉核对。
- `unlabeled` — 未为该 provider 配置 `models` 映射；只展示检测结果。
- `insufficient` — 所有探针被拒或严重截断；ModelTrace 不会为不可用输出评分。
- **system prompt 会显著污染指纹**（ModelTrace README 明确提醒过）。CPA 自身管道也会应用 system prompt；默认探针环境选用 clean-transport 来最小化该偏差，但概率应视为方向性参考，不是绝对值。
- **仅闭集可信** — 未登记模型会被任意归因。如果你路由的模型不在库内（当前 13 个模型），对它的判定无意义。
- **配额** — 探针是真实的生成请求（长输出，每次约 300-700 token）。保持 `interval_minutes` ≥ 60、`probes_per_run` ≤ 3，除非你接受消耗。
- 每轮探针逐个串行执行；进行中的一轮会阻止新的触发。

## 项目结构

```text
main.go            C ABI（cliproxy_plugin_init + call/free/shutdown）与 RPC 分发
config.go          插件配置解析、默认值、运行时更新
challenges.go      ModelTrace 挑战套件移植（12 环境、逐字一致的提示词）
fingerprint.go     评分移植（解析、Hellinger、有序分块、softmax、JS 相似度）
bank.go            统一指纹库加载、缓存、校验、校准查找
prober.go          目标枚举、探针执行、判定分类
usage_recorder.go  usage 钩子 -> 脱敏路由记录
management.go      管理注册 + 认证 JSON 路由
dashboard.go       无认证的服务端渲染仪表盘资源
port_test.go       对 ModelTrace 参考语料的保真度测试
data/unified_bank.json  指纹库（来自 ModelTrace）
```

## 致谢

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) — router-for-me；本项目的插件 ABI 与示例模板来源。
- [ModelTrace](https://github.com/xqy2006/ModelTrace) — xqy2006；本插件移植的指纹库、挑战套件与评分管道。
