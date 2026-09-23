# TradingAgents Go

Go + DDD 分层，HTTP API，MySQL/GORM + MongoDB + Redis + RabbitMQ，
goose 管理表结构，Wire 做依赖注入，`docker compose up` 一键起。

> 本平台仅用于学习与研究，不构成投资建议。分析结论由大模型生成，可能包含事实性错误。
> 过往表现不代表未来收益，投资有风险，可能损失本金。

## 分层约定

每个限界上下文内部固定六层：

```
cmd/                      cobra 子命令：serve / migrate（入口是根目录 main.go）
internal/di/providers/    Wire 的 provider 函数与 provider set，按限界上下文分文件
internal/di/injectors/    Wire 注入器：container.go 声明，wire_gen.go 生成
internal/bounded_contexts/<context>/
  application/        只放 handler：http_handlers / amqp_handlers / domain_event_handlers
  domain_services/    业务编排服务
  domain_events/      领域事件（Name() / ToJson()）
  entities/           聚合根，全部业务不变式
  repositories/       仓储具体 struct —— 事务只存在于这一层
  repositories/dtos/  DTO + ToDomain()/FromDomainXxx()（不设 mapper 包）
  value_objects/      校验通过、不可变的值类型
internal/domain_kernel/   跨上下文共享内核（值对象、领域事件基类、AmqpBus）
internal/helpers/         constants / custom_errors / response / concurrency / llm / marketdata / logger
internal/{db,di,server}/  连接、组装根、HTTP 装配
pkg/mq/                   自写的 AMQP 客户端（拓扑 / 发布确认 / 批量发布 / 有界重试 / 失败隔离 / 断线自愈）
```

依赖一律取自公开仓库。`pkg/mq` 是自己写的而不是引私有包：一个只在内网托管的依赖，
会让这个项目在任何拿不到那个网络的地方（新机器、干净的 CI 容器）直接构建失败，
而这里需要的能力薄到自己写一份的成本低于长期背着它。

硬约束（越界即缺陷，非风格问题）：

| 规则                          | 落实方式                                                                          |
|-------------------------------|-----------------------------------------------------------------------------------|
| 事务只在仓储层                | 仓储之外不出现 `*gorm.DB`；`repo.Save(root)` 自身原子                             |
| 禁止跨聚合共享事务            | `Batch` / `Task` 各自 Save，靠领域事件 + 幂等处理器收敛                           |
| DTO 不外泄                    | `repositories/dtos` 不被上层引用；读路径返回值对象                                |
| 只有聚合根有仓储              | `Section`、`Quote` 等是值对象，无独立仓储                                         |
| 跨聚合只传 ID/VO              | 上下文之间不互相引用 `entities`                                                   |
| 禁止循环内 RPC / 裸 goroutine | 统一走 `internal/helpers/concurrency`                                             |
| 禁止 TOCTOU                   | 校验与写入合并为一条 SQL 谓词或一个实体方法                                       |
| 乘除派生值必须落库            | 技术指标、进度百分比一次算出并持久化，读路径不得重算                              |
| 统一响应信封                  | `{code, message, data}`，见 `helpers/response`                                    |
| 消息处理器必须幂等            | 投递是至少一次；去重靠带谓词的 UPDATE，不靠「先查一下」                           |
| 缺失的批量能力必须返回哨兵    | 零 IO 返回 `ErrBatchUnsupported`，绝不返回 `(nil, nil)`——空切片会被降级链当成成功 |

`pkg/mq` 提供的能力，每一条都有测试（含对真实 broker 的集成测试）：

| 能力       | 说明                                                                          |
|------------|-------------------------------------------------------------------------------|
| 发布确认   | 发往不存在的交换机会报错，不会静默丢弃                                        |
| 有界重试   | 失败后延迟重投（`<queue>.retry` 的 TTL + 死信回流），次数用尽进 `<queue>.dlq` |
| 失败隔离   | 多处理器时只重投失败的那几个，见下方「领域事件」一节                          |
| 批量发布   | N 条一次压给信道再统一收确认，避免「发一条等一条」把批量提交拖成 N 次往返     |
| 断线自愈   | 重连、重新声明拓扑、重新拉起消费者                                            |
| panic 兜底 | 一条坏消息不会让整条队列停摆                                                  |

## 依赖注入用 Wire

```
providers/   「怎么造一个 X」—— 每个函数造一样东西，按限界上下文分文件
injectors/   「我要一个 Y」   —— 每个入口点列出需要哪些 set，其余交给 Wire
```

改完 `injectors/container.go` 或任何 provider set 之后必须重新生成：

```bash
make wire        # 等价于 go run github.com/google/wire/cmd/wire ./internal/di/injectors
make check       # 提交前：wire + fmt + build + vet + test
```

**为什么不继续手写组装根。** 手写版本有一个会随规模恶化的毛病：依赖顺序是隐式的。
「配置中心必须在 agent 之前装配，否则 LLM 路由读不到库里的供应商」这种约束，
在手写版本里只体现为 `Build` 函数里两行代码的先后，调换了编译照样通过，
故障要到运行时才出现——表现为「管理员改了密钥但一直不生效」。
换成 Wire 之后这类约束由函数签名表达：`NewLLMRouter` 的参数里有 `*ProviderResolver`，
Wire 就必然先造它。

**五个注入器，按入口点切分**，因为不同进程需要的东西不一样：

| 注入器                      | 谁在用 | 产出                         |
|-----------------------------|--------|------------------------------|
| `CreateHTTPServer`          | serve  | `*server.Server`             |
| `CreateUserService`         | serve  | 初始管理员，只装配身份上下文 |
| `CreateAmqpHandlers`        | serve  | 消息队列消费者               |
| `CreateDomainEventHandlers` | serve  | 领域事件订阅者               |
| `CreateWorkerRunners`       | serve  | 兜底巡检 + 调度循环          |

注入器仍然分开，尽管现在都由同一个进程调用：`--no-consumers` 起纯 API 副本时，
只装配 `CreateHTTPServer` 那一支，消费者的依赖（LLM 路由、行情源）根本不会被构造。

跨上下文的 `wire.Bind` 集中在 `providers/core_provider.go`——Wire 要求绑定与
具体类型的 provider 同处一个 set，这条规则顺带把「哪个上下文实现了谁的端口」
收成了一份四行的清单。

## 表结构迁移随服务启动执行

`serve` 启动时会自动把表结构推到最新，没有开关。
理由是「代码和它需要的表结构必须同时到位」只有进程自己清楚——交给一个独立的
运维步骤，就等于引入一个人工同步点：忘了跑、跑错顺序、或在滚动发布中途跑，
都会让新代码撞上旧表结构。

**多副本同时启动由数据库具名锁串行化**（`MigrateOnBoot`）。
注意进程内的 `sync.Once` 对此毫无帮助——它只保证一个进程跑一次，
而三个副本就是三个进程、三个 `sync.Once`，三条 `CREATE TABLE` 照样撞在一起。
拿到锁的副本跑迁移，其余的排队等待，等到之后发现已是最新版本直接往下走。

`migrate` 子命令保留，用于 `down` / `status` / `up-to` 这些需要人确认的操作。

## 定时任务：到点只发消息，消费端才执行

```
调度循环                          消息队列                   消费端
  ClaimDue（CAS 抢占触发）
  └─ 写一条 queued 执行记录  ──┐
     （这是欠条，不是审计）    │
  └─ 发消息 ─────────────────→ queue.scheduled_job_due ──→ ClaimQueued（queued→running 的 CAS）
                               ↑                              └─ 跑运行器
                    <queue>.retry（TTL 后回流）←── 失败：补一条新 attempt 再交给队列重投
                               │
                    <queue>.dlq ←── 次数用尽
```

「抢到一次触发」和「把这次触发跑完」时长差三个数量级，拆开之后：
巡检永远是毫秒级的、重试由队列的延迟重投承担、执行端副本数与调度端无关。

四个正确性要点，每一个都由数据库或 broker 保证，不靠应用层约定：

| 要点                       | 保证来自                                            |
|----------------------------|-----------------------------------------------------|
| 同一次触发只被一个副本抢到 | `UPDATE ... WHERE next_run_at = <期望值>` 命中 1 行 |
| 重复投递不会重复执行       | `UPDATE ... WHERE status='queued'` 命中 1 行        |
| 投递失败不丢触发           | 那条 queued 记录还欠着，恢复巡检补投                |
| 消费者中途死掉不丢触发     | 记录卡在 running，恢复巡检判失败并补一次尝试        |

重试是 **新记录**而不是改写：失败的尝试要原样留在历史里，否则重试三次的任务
只看得到最后一次。一次触发失败只计一笔连续失败——`max_consecutive_failures`
是运维按「触发」设的，让每次重试都计一笔等于用一个配置项悄悄改写另一个的含义。

## 分析任务：提交只落库发消息，消费端认领后才开跑

和定时任务同一套形状，不是另立一种：

```
提交（HTTP）                      消息队列                    消费端
  占并发名额（Redis 槽位）
  └─ 写一条 queued 任务  ──┐
  └─ 发消息 ──────────────→ queue.analysis_task_ready ──→ ClaimTask（queued→running 的 CAS）
                            ↑                               └─ 跑五阶段流水线
                 <queue>.retry（TTL 后回流）←── 认领失败（库不可用）
                            │
                 <queue>.dlq ←── 次数用尽（仅兜底，业务重试走不到这里）
```

**认领是这条链路唯一的互斥点。** 投递是至少一次的：信道断开、broker 重启、消费超时
都会让同一个任务在前一个消费者还在跑的时候被重投。「先查是不是 queued 再改成 running」
挡不住它——两次查询都返回 queued，两个消费者都会开跑， **同一次分析的 LLM 账单付两遍**。
判断必须写进 WHERE：

```sql
UPDATE analysis_tasks
SET status='running',
    attempts=attempts + 1, ...
    WHERE id = ? AND status = 'queued'
```

`RowsAffected == 1` 才算抢到；拿到 0 行的那个确认消息走人，不打日志——重复投递下
这是正常归宿，每条都记一行只会把日志刷满「无事发生」。

**重试次数由聚合决定，不由队列决定。** 业务失败时 handler 一律返回 `nil`（确认），
由 `finishFailed` 按 `Attempts`/`MaxAttempts` 判断还有没有余量，有就自己派发一条 **新**
消息。此时若改成返回错误，同一次重试会有两条消息，而队列的重投计数还会绕过领域的
最大尝试次数——一个必然失败的任务会被重投到死信为止，白烧好几轮 LLM 调用。
队列的 `MaxRetries` 因此刻意设在领域上限 **之上**，只兜「认领时数据库不可用」这类
领域判定之外的卡死。

**认领的代价是需要一个兜底巡检。** 谓词挡住了所有后来者，也包括赢家崩掉之后本该
接手的那个——那一行会永远停在 running。停滞巡检（`RecoverStale`，随服务启动先跑一轮）
按 `state_changed_at` 捞回来：

| 卡法                        | 成因       | 处置                           |
|-----------------------------|------------|--------------------------------|
| `queued` 超 `QueuedGrace`   | 消息没送到 | 补发一条（认领幂等，多发无害） |
| `running` 超 `RunningGrace` | 消费者死了 | 判失败，再交给重试策略         |

`state_changed_at` 是专为它加的列， **不能复用 `started_at`**：后者的语义是「首次启动、
重试不覆盖」，服务于对外报的总耗时。拿它当巡检依据会误判——一个第三次尝试、刚跑了
两分钟的健康任务，`started_at` 可能是一小时前，于是巡检把它判死。杀掉一个正在跑的
付费任务，正是这套机制要避免的事。为防漏写，实体里所有状态迁移收口到一个私有
`setStatus`，两个值同进同出。

**三个超时值必须一起看**，它们量化的是同一件事：

| 值                             | 位置                             | 关系                |
|--------------------------------|----------------------------------|---------------------|
| `constants.AnalysisMaxRuntime` | Go                               | 唯一来源（30 分钟） |
| `consumer_timeout`             | `docker-compose.yml` 的 rabbitmq | 必须等于它          |
| `WorkerConfig.RunningGrace`    | Go                               | 必须**大于**它      |

顺序是有意的：broker 先收回超时未 ack 的消息，巡检随后才介入。反过来的话，巡检刚把
任务改回 queued，那条还没超时的旧消息就可能再认领一次。前两者跨 Go 与 YAML，
靠注释互指不可靠，因此有一个测试直接解析 compose 来断言两边一致。

## 领域事件走消息队列

`domain_services` 只认 `domain_event.Publisher` 接口，实现是 `AmqpBus`：
事件发到 `queue.trading_agents_event`，消费端按事件名找回具体类型再分发。
订阅写法与 margin 一致——`bus.RegisterSubscriber(handler, &SomeEvent{})`，
样例事件同时提供了事件名和反序列化所需的类型。

**消费发生在 serve 进程里。** 早先拆成 api + worker 两个容器时，这里有一条必须
记住的规矩：serve 只发不收，否则同一条事件会随机落到两个进程之一，而它们注册的
处理器并不相同——故障表现是「有时候好使」，没有任何错误日志。合并成一个进程之后，
这条规矩连同它能引发的故障一起没有了。

要按请求量横向扩容就加副本并带 `--no-consumers`：消费者是竞争性的，多一个副本
消费也不会重复执行，任务的认领由数据库的条件更新收口（见 `ClaimTask`）。

### 失败隔离：一个处理器失败，不拖着其他处理器重跑

一条事件常有多个处理器（`task_completed` 既要发通知又要生成报告）。
朴素做法是「任何一个失败就整条消息重投」，于是已经成功的那些被迫再来一遍。
幂等能兜住正确性，但幂等只是「不出错」，不是「没代价」。

这里的做法是： **全部跑完，只把失败的那几个报上去**。传输层把它们的名字写进
`x-retry-scope` 消息头，重投回来时只重跑这几个。

```
第一轮  通知✓  报告✗  结算✓   ->  重投，范围 = [报告]
第二轮         报告✓          ->  确认，结束
```

处理器名由方法值推导（`(*OnTaskCompletedHandler).OnTaskCompleted`），
调用点不用手写——手写的名字迟早会重名，而重名会让其中一个永远不被重试。
名字对不上时（改过名）退回「全部执行」：宁可重复，也不能让谁被永久跳过。

## 五阶段分析流水线

```
数据准备 → 6 分析师（并行·容错） → 多空辩论（串行） → 交易员（严格） → 3 风控视角（并行·容错）+ 风控经理（严格）
```

并行阶段用 `concurrency.Settle`（单个分析师挂掉不影响其余），决策阶段用 `concurrency.Map`（快速失败）。
辩论必须串行——空头要针对多头的具体论点反驳。

## 快速开始

```bash
make up          # 起整套：api + MySQL + Mongo + Redis + RabbitMQ
make logs        # 跟着看 api 的日志
make down        # 停掉（数据卷保留）；make down-v 连数据一起删
```

起来之后：

| 地址                                     | 是什么                                 |
|------------------------------------------|----------------------------------------|
| http://localhost:8080/healthz            | 健康检查                               |
| http://localhost:8080/api/v1             | 接口根路径                             |
| http://localhost:8080/swagger/index.html | 接口文档（Swagger UI），生产环境不挂载 |
| http://localhost:15672                   | RabbitMQ 管理界面（guest/guest）       |

初始管理员 `admin` / `admin12345`， **生产务必修改**。
表结构不需要单独迁移——api 启动时自动执行。

```bash
curl -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin12345"}'
```

### 容器编排的几个决定

**只有一个应用容器。** 曾经拆成 api + worker，理由是「分析耗时以分钟计，
不该跑在 API 进程里」——那条理由在 Go 里不成立：没有请求线程池，一个阻塞在
LLM 调用上的 goroutine 只占几 KB 栈；并发上限也不靠拆进程保护，它在 Redis
的并发闸门里（提交时扣、跨进程生效）。合并还顺带消掉了「api 是新版、worker
是旧版」这类只会在多进程下出现的状态。

**依赖全部用 `condition: service_healthy`。** 容器起来了不等于服务就绪，
MySQL 尤其：它的进程先于端口存在，应用连上去只会拿到 connection refused 然后退出。

**没有单独的迁移容器。** 迁移随服务启动执行，多副本并发由数据库具名锁串行化，
加一个 init 容器只会多一个需要维护的启动顺序约束，而它挡不住任何东西。

**Redis 开了 appendonly。** 分析队列与并发闸门不是缓存：丢掉 processing 有序集合，
正在跑的任务就再也没人回收了。

### 配置：只有 .env，没有 config.yaml

全部配置 = **内置默认值 + `.env`**，一个来源。镜像里不带任何配置文件——
一份烤进镜像的基线配置会变成「到底哪个值生效了」的第二个答案，
而排查配置问题时，有两个来源等于没有来源。

```bash
cp .env.example .env     # 六十多项，每项都有注释说明它影响什么
```

`.env` 里有两类变量：`TA_*` 是应用配置（键名 = 配置路径把点换成下划线），
其余的只给 compose 用（依赖容器密码、端口映射）。

改配置 **不需要重建镜像**：

```bash
vim .env && docker compose up -d --no-deps api
```

供应商列表是个数组，环境变量放不下结构，所以单独用一个 JSON 串：

```bash
TA_LLM_PROVIDERS_JSON='[{"name":"deepseek","kind":"openai_compat","apiKey":"sk-x",...}]'
```

写坏了会让启动直接失败，不会静默忽略——被忽略的表现是「配了密钥但所有分析
都说没有可用模型」，而那时没人会想到去看一个解析错误。

> 有个坑值得记下来：viper 的 `AutomaticEnv` 只为它「已知」的键查环境变量，
> 而没有默认值的键（凭据恰好都是这一类，密码不该有默认值）会被 **静默忽略**——
> `TA_MYSQL_PASSWORD` 曾经什么也不做，服务拿着空密码去连库。
> 现在由 `bindEnvOverrides` 显式绑定全部键，并有测试钉住（`config/config_test.go`）。

### 不用容器跑应用

```bash
make deps-up                       # 只起四个依赖
cp .env.example .env               # 把 mysql/mongo/redis/rabbitmq 改成 127.0.0.1
set -a && source .env && set +a    # 导出到当前 shell
go run . serve                     # HTTP :8080 + 消费者 + 巡检 + 调度循环
```

改代码不想手动重启的话直接用热加载，见下一节。

一个二进制多个子命令，好处不在于少编译几次，而在于它们必然共用同一份装配——
拆成几个 `main` 包之后，「两个进程对同一份配置的理解是否一致」就成了要靠人核对的事。

消息队列是硬依赖，没有 `enabled` 开关：关掉之后服务照常启动，但定时任务不再触发、
领域事件不再送达——一个「起来了但什么都不干」的进程比起不来危险得多。

`pkg/mq` 的集成测试需要 RabbitMQ，没有时自动跳过（单元测试必须能在干净机器上跑）。

## 热加载

改一行 `.go` 自动重新编译并重启，由 [air](https://github.com/air-verse/air) 驱动。
两种跑法，按你平时的习惯挑一种。

**宿主机上跑应用，依赖走容器：**

```bash
make deps-up     # 四个依赖
make dev         # 热加载，Ctrl-C 停
```

**全部在容器里：**

```bash
make dev-up      # 源码挂进容器，api 改跑 air
make dev-logs    # 编译日志也在这里
make dev-down
```

容器版叠的是 `docker-compose.dev.yml`，基础那份不动——`make up` 起出来的
仍然是贴近生产的镜像，两者不会互相污染（连镜像 tag 都是分开的）。

### 几个不显然的点

**`.sql` 和 `.go` 一样会触发重建。** 迁移脚本是 `go:embed` 进二进制的，
不重新编译，跑的还是上一版表结构。

**退出走的是中断信号，不是 SIGKILL**（`send_interrupt = true`）。本服务的退出路径
有实质内容：HTTP 要等在途请求跑完，消费端要摘掉 RabbitMQ 消费者、关干净连接。
被强杀的话这段代码在开发期永远不会执行到，「优雅停机写没写对」只能等上线才发现；
RabbitMQ 那边还会留下一串要等心跳超时才回收的僵尸消费者。

**改完接口注释要手动 `make swagger`。** air 不会自动跑它——swag 在这个仓库上要跑六秒，
挂到每次保存上会让热加载失去意义。生成完 `docs/` 变化会触发一次重启，
`/swagger` 上的文档随之更新。

**air 没有进 `tools.go`，是有意的。** 版本钉在 Makefile 的 `AIR` 变量里，
用 `go run pkg@version` 调。放进 `tools.go` 会把它的依赖闭包并进主模块：
air v1.67 声明 `go 1.26`，`go get` 它会把本模块的 go 指令从 1.23 顶上去，
并顺带升掉 wire、cobra、jwt、x/crypto——一个开发期的文件监听器不该有能力
改动生产二进制的依赖版本。判据是「生成物是否进版本库」：wire 和 swag 的产物要提交、
必须锁版本保证人人一致；air 不产出任何要提交的东西。

**容器版里 `tmp/` 是个匿名卷，不是挂载进去的。** 否则容器编译出的 linux 二进制
和宿主机 `make dev` 编译出的 darwin 二进制会写进同一个 `tmp/api`，
两边同时开着就互相覆盖，症状是其中一个起来就 `exec format error`。

## 日志与链路追踪

一次请求从进来到落库，全链路共用一个 `trace_id`，日志里按它检索就能把整条链拼起来。

**链路怎么串起来的：**

```
HTTP 请求进来
  └ RequestID 中间件：取 X-Request-Id，没有就生成一个
      └ 写进 c.Request.Context()          ← 关键的一步，以前断在这里
          └ 领域服务 / 仓储（拿到的都是标准 context）
              ├ SQL 日志          trace_id=…
              ├ Redis / Mongo 命令 trace_id=…
              ├ 出站 HTTP（大模型、行情）trace_id=…
              └ 发消息 → AMQP 头 x-trace-id
                          └ 消费端还原进 ctx
                              └ 消费端的 SQL 日志 trace_id=… 还是同一个
```

响应头会回显 `X-Request-Id`。调用方带了这个头就沿用它，因此上游服务的链路号能一路穿下来，不会在本服务这里断成两截。

`trace_id` 不需要逐层传参：中间件把它烤进了上下文日志器，任何一层
`logger.FromContext(ctx)` 打出来的日志都自动带上。

### 配置

| 键                         | 环境变量                      | 默认                        | 作用                                   |
|----------------------------|-------------------------------|-----------------------------|----------------------------------------|
| `log.sql`                  | `TA_LOG_SQL`                  | `true`                      | 是否逐条打印 SQL / Redis / Mongo 命令  |
| `log.sql_args`             | `TA_LOG_SQL_ARGS`             | 非生产 `true`，生产 `false` | 参数值留明文还是脱敏                   |
| `queue.max_attempts`       | `TA_QUEUE_MAX_ATTEMPTS`       | `3`                         | 一个分析任务最多尝试几次，由领域层判定 |
| `queue.slot_ttl`           | `TA_QUEUE_SLOT_TTL`           | `6h`                        | 并发凭据存活时长（名额泄漏的自愈机制） |
| `queue.progress_ttl`       | `TA_QUEUE_PROGRESS_TTL`       | `2h`                        | 进度快照在 Redis 里的存活时长          |
| `queue.user_concurrency`   | `TA_QUEUE_USER_CONCURRENCY`   | `3`                         | 单用户并发分析上限，0 不限             |
| `queue.global_concurrency` | `TA_QUEUE_GLOBAL_CONCURRENCY` | `20`                        | 全局并发分析上限，0 不限               |
| `market.eastmoney_rps`     | `TA_MARKET_EASTMONEY_RPS`     | `2`                         | 东财限速；这是正确性不是性能旋钮       |

**两个 TTL 必须大于 `AnalysisMaxRuntime`（30 分钟），有测试守着。** 它们原先共用一个
为 Redis 队列可见性超时而设的 `visibility_timeout`（15 分钟）——比一次分析还短。
后果没有任何报错：进度 TTL 偏短会让长任务跑到一半进度条变空，凭据 TTL 偏短会让名额
提前放出去、实际并发悄悄超过配置上限。两个都只在任务跑得久时才发作，
也就是只在最该稳的时候发作。

**`log.sql` 是独立开关，不是复用 `log.level`。** 想看 SQL 就把全局降到 debug 的话，
会同时打开每个第三方库的调试输出，日志量大到没法看——于是下一步就是整个关掉，
然后再也不打开。

**关掉 `log.sql` 只让成功的命令闭嘴。** 慢查询（>200ms）和执行失败无论如何都打。
一个会把故障一起藏起来的开关是陷阱：真出事时「日志里什么都没有」会被读成「这里没问题」。

**`log.sql_args` 关掉后参数变回 `?` 占位符。** GORM 打出来的 SQL 是参数内联之后的，
明文意味着邮箱、口令哈希、持仓金额原样进日志——而日志的留存期限和访问范围
跟数据库不是一回事。脱敏发生在值和 SQL 拼起来 **之前**（走 GORM 的 `ParamsFilter`），
不是事后拿正则去猜哪一段是值。

它盖不住两处，别当成安全边界：调用方自己拼进 SQL 字符串的字面量
（`Raw("... WHERE email = 'a@b'")`、`fmt.Sprintf` 出来的条件），以及 `Row()` / `Rows()`
那条路——GORM 在那里换成自己的 recorder，不回调 `ParamsFilter`。
避开的办法只有一条：条件一律用 `?` 传参。

### 加新的基础设施适配器时

日志器一律用 `logger.FromContextOr(ctx, fallback)` 取，不要用全局那个。
`FromContext` 的退路只认全局日志器，而 `logger.Init` 之前全局是 Nop——
启动期的日志会一条不剩地消失，且没有任何报错。

## 数据库迁移

表结构由 goose 管理，脚本在 `internal/db/migrations/`，用 `go:embed` 编译进二进制——
镜像里只有一个可执行文件，迁移脚本和跑它的代码永远来自同一次构建。

`up` 随服务启动自动执行（见上一节），下面这些子命令供需要人确认的场合使用：

```bash
go run . migrate up               # 应用全部未执行的迁移（通常不用手动跑）
go run . migrate down             # 回滚最近一次
go run . migrate status           # 各脚本应用情况
go run . migrate version          # 当前版本号
go run . migrate up-to 20260916000010
```

`migrate` 只连 MySQL，不走完整装配：让一次表结构变更平白多出 Mongo、Redis、
消息队列三个可能失败的依赖，大概是运维最不想在凌晨看到的事。

不用 GORM 的 `AutoMigrate`：它只加列、不删列也不改类型，表结构会在「代码以为的样子」
和「数据库实际的样子」之间静默漂移，而漂移只有在某天读到 NULL 或被截断的数据时才暴露。
显式迁移脚本还带来两样东西：可评审的变更历史，以及可回滚的 `Down`。

未配置任何 LLM 密钥时分析会失败；未配置行情密钥时自动退回确定性 mock 数据源
（`market.enable_mock`），可在零密钥下跑通全链路，但结论不具参考价值。

首次启动会按 `auth.bootstrap_admin` 创建管理员（默认 `admin` / `admin12345`， **生产务必修改**）。

环境变量以 `TA_` 前缀覆盖配置，例如 `TA_MYSQL_PASSWORD`、`TA_AUTH_JWT_SECRET`。

## 接口

全部挂在 `/api/v1`，响应统一为 `{"code":0,"message":"ok","data":{...}}`。
共 91 个端点，按上下文分成十三组。

**完整清单以 Swagger 为准**，非生产环境起来后直接打开：

| 地址                                     | 是什么                           |
|------------------------------------------|----------------------------------|
| http://localhost:8080/swagger/index.html | Swagger UI，可直接试调           |
| http://localhost:8080/swagger/doc.json   | OpenAPI 2.0 规范，喂给代码生成器 |

仓库里的 `docs/swagger.json` 与 `docs/swagger.yaml` 是同一份东西的离线副本，
由 `make swagger` 从处理器上的注释生成，随代码一起提交。

这一节以前是一张手写的端点表。删掉它不是因为表格不好看，而是因为它已经错了——
自选股那几行写的还是 `/watchlist/groups/:id/stocks`，而代码里早就改成了 `/items`。
一份需要人记得同步的接口清单，最终一定会停在某个历史版本上，
而它比没有清单更贵：调用方会信它。

几条从签名上看不出来的约定：

- 只有 `/stocks/**` 与 `/agents/**` 免登录，其余都要 `Authorization: Bearer {accessToken}`。
- 报告没有创建接口：它由 `OnTaskCompleted` 事件生成，开放直写等于允许绕过分析结论伪造报告。
- 配置中心的任何响应都不返回明文密钥，只返回掩码（`sk-…wxyz`）。
- 访问不属于自己的资源一律返回 404 而不是 403。403 会确认这个 ID 存在，
  等于把 ID 空间变成一个可枚举的探测通道。文档里因此看不到 403，这是有意的。
- `GET /analysis/tasks/:id/progress` 是 SSE，不是 JSON。但**每一帧的负载仍然是统一信封**——
  正常帧 `event: progress`，data 为 `{"code":0,...,"data":{progressView}}`；出错帧
  `event: error`，信封 data 为 null。调用方要剥一层才拿得到进度。
  空闲时服务端发 `: keep-alive` 注释行保活，客户端应忽略。

## 前端

`web/` 是一个独立的 npm 工程（React + TypeScript + Vite + Ant Design），
对接的就是上面那套 `/api/v1`。详见 [web/README.md](web/README.md)。

```bash
make web-install   # 装依赖（npm ci）
make web-types     # 从 docs/swagger.json 生成 TS 类型
make web-dev       # Vite :5173，/api 反代到 :8080；后端另开一个 make dev
make web-build     # 产出 web/dist
```

**生产形态是单容器。** `npm run build` 的产物由 Go 二进制通过 `go:embed` 托管
（`web/embed.go`），没有第二个容器，也没有跨域——前端和接口同源。
Dockerfile 里的 `web-builder` 阶段会自己跑一遍前端构建，本地不必先 `make web-build`。

页面覆盖十个上下文：股票、自选股、选股筛选、分析任务（含 SSE 实时进度与决策链）、
报告、模拟交易、定时任务、通知、系统配置、用户管理。后两个仅管理员可见——
但那只是前端藏菜单，**权限判定在后端**，别因为前端有门禁就以为服务端可以少判一次。

几条从代码上看不出来的约定：

- **加接口要在 `web/src/api/__typecheck__.ts` 补一行。** `request<T>` 是泛型的，
  手写返回类型时 TypeScript 不核对后端实际返回什么。这个坑踩过三次
  （分页信封写成裸数组、字段名写错、对象写成数组），三次都编译通过、
  只在真实数据上才暴露。那份对账表把同类错误变成编译错误。
- **嵌入前端的 Go 文件只能放在 `web/`。** `go:embed` 的路径模式不允许出现 `..`，
  放进 `internal/server` 再写 `../../web/dist` 是编译期错误。
- **`web/dist/.gitkeep` 与 `web/public/.gitkeep` 都不能删。** 产物不进版本库，
  而 `go:embed` 要求目录非空，前者是为此提交的占位；Vite 每次构建会清空 `dist`
  把它删掉，后者负责再拷回来。
- **前端没构建也能 `go build`。** 此时二进制里没有 `index.html`，访问 `/`
  会返回一句提示而不是白页，启动日志里也会有一条 warn。
- **未匹配路由的分流在 `internal/server/static.go`。** `/api/`、`/mcp`、`/swagger/`、
  `/healthz` 下面的仍返回 JSON 信封 404，其余兜底到 `index.html`（前端用 history 路由，
  不兜底的话 `/analysis/tasks/xxx` 刷新就打不开）。

## 定时任务与数据同步

调度器随 `serve` 启动（`--no-scheduler` 可关）， **支持多副本**：同一次触发只会有一个副本执行，
靠的是 `UPDATE ... WHERE id=? AND next_run_at=?` 的条件更新——`RowsAffected==1` 才算抢到。
不用分布式锁，因为这个保证直接来自数据库对同一行 UPDATE 的串行化。

任务种类由 `internal/di/runners.go` 注册，调度上下文本身不认识行情同步与分析任务：

| JobKind              | payload                                  | 行为                                 |
|----------------------|------------------------------------------|--------------------------------------|
| `market_sync`        | `{"kind":"quotes","market":"CN"}`        | 在消费端跑完整轮同步                 |
| `scheduled_analysis` | `{"code":"600519","userId":1,"depth":3}` | 只把分析排进它自己的队列，不等它跑完 |

两者都在消息消费端执行，调度循环本身只投递。`scheduled_analysis`
额外再转一次队列（`queue.analysis_task_ready`），是因为一次分析要十几分钟——
占住一个定时任务消费者名额那么久，会让同一个队列上的其他定时任务全部排在它后面。

同步作业的可靠性设计：

- 同类同市场只允许一个实例在跑——靠 `running_key` 唯一索引（终态写 NULL，让唯一索引只对运行中的行生效），不是先查后插
- 分片落库 + 断点续传：崩溃后从 `cursor` 之后继续，而不是整轮重来
- 被强杀留下的 `running` 记录会在服务启动时判失败并释放唯一索引占位，否则同类同步再也起不来
- 成功率是乘除派生值，算一次落库，读路径只读存量

### 批量端点是这里唯一重要的性能事实

逐标的取数的外部调用数 **等于标的数**（A 股 5917 只）。而东财必须限速到约 2 次/秒
（超了它不报错，直接掐 TCP，速率是正确性不是调优），一次全量同步下限就是 **49 分钟**，
且随市场扩容线性变长。所以端口上专门开了两个批量方法，能用批量就绝不逐标的：

| 能力        | 批量端点                          | 谁提供                         | 调用数                                             |
|-------------|-----------------------------------|--------------------------------|----------------------------------------------------|
| 行情快照    | `FetchQuotes`（整表）             | 东财 `clist`                   | 约 60 次（每页 100）                               |
| 日线        | `FetchKlinesByDate`（某天全市场） | tushare `daily` 不传 `ts_code` | 约 262 次（回看 365 天扣掉周末），**与标的数无关** |
| 财务 / 资讯 | 无                                | —                              | 逐标的，5917 次                                    |

没有这个能力的源必须 **零 IO 立即**返回包着 `ErrBatchUnsupported` 的哨兵，绝不能返回
`(nil, nil)`：空切片会被降级链当成成功，同步报告「成功 0 条」——最难排查的那种失败。
上层据哨兵退回逐标的路径，两条路径的统计口径必须完全一致（分母都取本地标的全集），
否则「今天成功率 100%、昨天 82%」反映的只是走了哪条路。

**按交易日拉必须翻页。** tushare 的 `daily` 单次返回有上限，而 5917 只就贴着那个数。
赌一次拉完的后果不是报错，是「某天莫名少了几百只票的日线」，要等有人对着某只票的图
发现断档才暴露。

**`FanOutLimit` 调大并不会更快。** 令牌桶是全局的，8 个 goroutine 全排在同一个
2 RPS 的限速器后面。它只是内存与连接数的上限，真正的节流在 `marketdata/throttle.go`。

### 数据源能力矩阵

|                       | 股票列表 | 行情快照        | 日线    | 按日批量    | 财务 | 资讯          |
|-----------------------|----------|-----------------|---------|-------------|------|---------------|
| tushare（CN）         | ✓       | ✗ 需 2000 积分 | ✓      | **✓**      | ✓   | ✗ 需更高积分 |
| eastmoney（CN/HK/US） | ✓       | ✓              | ✓      | ✗ 只能逐只 | ✗   | **✓ 仅 CN**  |
| finnhub（US）         | ✓       | ✗ 无整表端点   | ✓ 付费 | ✗          | ✓   | ✓            |

降级链顺序由 `TA_MARKET_PROVIDERS` 决定。当前是 `eastmoney,tushare`——东财在前是因为
它的 `clist` 一次拿全市场快照，而 K 线走批量端点时哨兵会自动把它让给 tushare。

**东财资讯走的是站内搜索（`search-api-web`），和行情完全是另一套接口**，三个坑：
只接受 JSONP（不带 `cb` 直接 400，必须剥壳）；默认往标题正文里注入 `<em>` 高亮标签
（把 `preTag`/`postTag` 传空串关掉，比拿回来再洗可靠）；默认按相关度排序会把几个月前
的旧文顶到前面，要显式 `sort:"time"`。它没有日期过滤参数，回看窗口只能在本地裁。

**出站重试分两类，别混。** 连接被掐（东财限流的表现，Go 侧是 EOF）和 HTTP 5xx 是
两条路径：前者一直有重试，后者以前完全没有——而批量行情只有东财一个源，一次转瞬即逝
的 502 就是整个 quotes 同步彻底失败。现在两者都重试（3 次，800ms 起指数退避加抖动），
但只对幂等方法，且 4xx 一律不重试（参数错、没权限、找不到，重试一百次还是一样的结果）。

## 开发

```bash
make dev         # 开发时开着：改代码自动重编译重启（见「热加载」）
make check       # 提交前跑这一套：wire → swagger → fmt → build → vet → test
```

拆开也行：

```bash
make wire        # 改了 provider set 之后必须跑
make swagger     # 改了处理器上的接口注释之后必须跑
go build ./... && go vet ./... && go test ./...
gofmt -l .
```

### 接口注释怎么写

`docs/` 由 swag 从处理器的注释生成，不要手改。两条容易踩的规则：

**`@name` 必须写成右花括号后的行尾注释，写在结构体上方的文档注释里会被静默忽略。**
swag 读的是 `ast.TypeSpec.Comment`（行尾注释），不是 `Doc`。写错的代价是它不报错——
只是定义名悄悄退回 `http_handlers.xxx`，而本项目十三个上下文的处理器包 **全都叫**
`http_handlers`，于是同名类型撞在一起，swag 会改用带全路径的名字，
生成出 `internal_bounded__contexts_stock_application_http__handlers.listRequest` 这种东西。

```go
type stockView struct {
Code string `json:"code"`
} // @name stock.StockView
```

**`@Router` 的路径不带 `/api/v1`**，basePath 由 swag 补；但 gin 的 group 前缀要算进去，
`:code` 写成 `{code}`：

```go
// @Summary  查询标的主数据
// @Tags     股票行情
// @Produce  json
// @Param    code path     string true "标的代码，如 600519.SH"
// @Success  200  {object} response.Envelope{data=stockView}
// @Failure  404  {object} response.Envelope "标的不存在"
// @Router   /stocks/{code} [get]
```

分页响应写 `response.Envelope{data=response.PageData{items=[]stockView}}`。
`@Failure` 只写这个处理器真能返回的状态码——照着 `custom_errors` 的 Code 对
`response.classify` 查一遍，别凭印象写。

响应体不要用 `gin.H`：map 在 OpenAPI 里没有形状，那个响应就只能靠人手写文档。
现在处理器里已经一个 `gin.H` 响应都没有了，保持住。
