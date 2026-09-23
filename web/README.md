# web

TradingAgents 的前端，对接同仓库 Go 服务的 `/api/v1`。

## 跑起来

```bash
make deps-up     # 依赖容器（Postgres / Redis / RabbitMQ）
make dev         # 后端，:8080
make web-dev     # 前端，:5173
```

开发期 Vite 把 `/api` 反代到 `:8080`，因此请求形态与生产（同源）一致，不经过 CORS。
生产形态是 `npm run build` 的产物由 Go 二进制用 `go:embed` 托管，单容器交付。

## API 类型

`src/types/api.generated.ts` 是生成物，不手改，也不进版本库。重新生成：

```bash
make web-types   # 先 make swagger 重建 docs/，再生成
```

## 两个 .gitkeep 都别删

`dist/.gitkeep` 是 Go 侧 `//go:embed all:dist` 的编译前提（目录空或不存在就编不过），
所以它进版本库；`public/.gitkeep` 用来在每次构建清空 outDir 之后把它拷回来。
删掉前者，别人 clone 下来 `go build` 失败；删掉后者，每次前端构建都会在
git status 里留一条 deleted。

## 依赖版本上的两个坑

**TypeScript 钉在 5.x，不要升 7。** TS 7 是原生 Go 移植版，不再暴露旧的 `ts.factory`
compiler API，而 `openapi-typescript` 依赖它——升上去 `make web-types` 直接抛
`Cannot read properties of undefined (reading 'createKeywordTypeNode')`。

**`swagger2openapi` 那道转换去不掉。** swag v1 产出 Swagger 2.0，`openapi-typescript`
只吃 OpenAPI 3.x。「升 swag v2 就能直出 3.1」这条路是死的，理由见 Makefile 里
`web-types` 目标的注释——一句话：gin-swagger 没有适配 swag v2 的稳定版。

## 加接口时必须做的一步

`src/api/__typecheck__.ts` 是一份契约对账表，加接口时在里面补一行。

它存在的理由很具体：`request<T>` 是泛型的，api 模块里手写
`Promise<SanitizedProvider[]>` 时 TypeScript 不会去核对后端到底返回什么。
这个坑踩过三次，三次都编译通过、都只在真实数据上才暴露：

| 端点 | 写成 | 实际 | 症状 |
|---|---|---|---|
| `/config/providers` | 裸数组 | 分页信封 | 表格永远空着 |
| `/notifications/unread-count` | `{count}` | `{unread}` | 徽标永远是 0 |
| `/watchlist/groups/{id}/items` | 裸数组 | `{group, items}` | 判空永远不成立 |

对账比的是**顶层键集**而不是类型本身——手写的 `Page<T>` 是 `items: T[]`（必填），
生成的是 `items?:`（可选），严格相等会全线误报。键集比对刚好覆盖
「数组 vs 对象」「字段名写错」「分页 vs 裸数组」这三类，且不受可选性影响。

## 展示层的两条约定

**模型输出一律走 `<Markdown>`。** 报告正文和决策链里每个智能体的陈述都是 Markdown
（`**加粗**`、`- 列表`、`### 标题`、GFM 表格都有），当纯文本渲染会把符号原样摆在页面上。
用的是 `react-markdown`，**它默认不渲染原生 HTML**——正文是大模型生成的不可信输入，
这道保护是选它而不是 marked 的主要理由。不要为了渲染 HTML 去加 `rehype-raw`。

**数字和时间不要原样输出。** 后端刻意不做展示层换算
（`report_handler.go` 里解释了为什么接口层不写 `Confidence*100`：那组数字是
生成时固化的事实，读路径重算会让同一份报告每刷新一次都可能变）。那条约束管的是
「不重新推导业务事实」，不包括「怎么显示」，所以换算在前端做：

| 字段 | 后端 | 显示 | 工具 |
|---|---|---|---|
| confidence | `0.4200`（0-1） | `42%` | `formatConfidence` |
| riskScore | `7.00`（0-10） | `7.0 / 10` | `formatRiskScore` |
| position | `5.00`（0-100） | `5%` | `formatPosition` |
| targetPrice | `4.6140` | `4.61` | `formatPrice` |
| createdAt | `2026-09-24T00:51:49.191+08:00` | `12分钟前`（悬停看完整） | `<TimeText>` |
| durationSeconds | `"49.234"` | `49.2 s` | `formatSeconds` |

时间用原生 `Intl`，没引 dayjs / date-fns——只需要格式化和相对时间两件事，
`Intl.DateTimeFormat` 与 `Intl.RelativeTimeFormat` 都够用。

## 约定

- UI 库只用 Ant Design，不再引第三方；图表另议
- 跨组件传参超过 3 层用 `useContext` + 类型化 hook，不 props-drilling
- 组件超过 ~200 行先拆 hook / 子组件
- 每个页面都要有 loading / empty / error 三态
- 交互元素用 `<button>` / `<a>`，不用 `div + onClick`
- `src/features/` 下的目录与 Go 侧 `internal/bounded_contexts/` 同名，前后端认知对齐
