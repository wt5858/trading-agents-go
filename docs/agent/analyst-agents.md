# 六角色分析师 Agent

已覆盖 market / fundamentals / news / sentiment / sector / index。保留原研究、交易、风控编排以及 Markdown 报告接口。默认开启；ANALYST_AGENTS_ENABLED=false 并重启可整体回退。兼容第一期 MARKET_AGENT_ENABLED=false，它同样回退全部分析师。直接构造 RuntimeConfig 时需显式 AnalystAgentsEnabled:true。

## 执行契约

每个分析师拥有独立身份、任务、工具白名单、最多3轮工具及4次模型请求。初始提示只带任务和数据目录，工具从本次冻结快照读取；不在分析师扇出阶段回源。

输出为 JSON：status（complete/partial/insufficient）、direction、findings、risks、missing。每条 finding 引用实际返回且非 missing 的 evidence_id。基本面必须引用财报，新闻必须引用资讯，情绪必须引用社交数据，板块和大盘分别必须引用专用背景；个股行情不能替代这些资料。缺失时可返回 neutral/insufficient。校验失败保留原始输出和费用，按现有 tolerant 策略处理，不额外调用旧模式兜底。

引用校验验证出处是否存在，不证明论证是否正确。评测的 passed 指 Agent 契约通过，报告另列 complete/partial/insufficient，不把缺资料但诚实回答误称完整分析。

## 数据与历史时点

行情、日线、财务、新闻和社交数据来自 collect。财务只纳入具有明确披露日且已披露的记录；缺披露日的旧记录不猜测可用时间。新闻和社交发布时间按标的市场交易日截止（CN/HK为UTC+8，US为纽约时区含夏令时）过滤。代码比较使用市场和规范代码，忽略 Raw 输入拼写。

背景数据保存在同一个 MarketDataRepository 的 market_contexts 集合，避免指数代码与个股代码混淆。记录包含覆盖标的 Code、scope=sector/index、参考对象 reference、数据日 as_of、可获得日 available_on、source 和 data。两个日期都不得晚于分析日。

当前支持**带来源的数据导入**，没有假设现有行情供应商具备板块/指数接口，也没有自动编造板块资金流。没有导入实际背景数据时，这两位分析师将返回证据不足。纯读数据为VO；保存与查询使用同一仓储，DTO映射留在DTO文件，无独立读库。

示例文件 fixtures/synthetic-contexts.json 是合成数据，只供校验演示，不应当作真实行情导入生产。

## 命令

在项目根目录使用：

~~~powershell
# 检查样本，不花模型费用
 go run . analyst-eval --input docs/agent/fixtures/synthetic-six-analysts.json --dry-run

# 六角色配对链路测试，重复两轮；输出文件必须尚不存在
 go run . analyst-eval --input docs/agent/fixtures/synthetic-six-analysts.json --mock --compare --repeat 2 --output evaluation.json

# 从.env加载模型配置，调用真实模型；也可直接通过shell设置TA_*变量
 node scripts/run-analyst-eval.cjs --input your-historical-cases.json --compare --repeat 2 --max-requests 48 --max-cost 0.30 --output real-evaluation.json

# 从业务数据库准备资料，独立运行基本面分析师，保存agent_runs轨迹
 go run . analyst-run --code 600519 --market CN --date 2024-03-01 --analyst fundamentals

# 先校验背景资料，再用实际来源文件导入；导入需要业务数据库连接
 go run . market-context-import --input your-contexts.json --dry-run
 go run . market-context-import --input your-contexts.json
~~~

Go CLI本身不会自动加载.env，Compose会通过env_file加载。Node辅助脚本只给子进程加载.env，保留现有shell值且不打印密钥；可用GO_BINARY指定Go路径。

独立运行及导入使用现有依赖装配，会连接配置中的数据库。导入按自然键批量upsert，可重复执行；索引由现有启动索引初始化流程建立。评测仅使用JSON冻结快照和模型配置，无需数据库；不会把测试资料写入业务库。

## 评测样本与输出

输入是 AnalystCase 数组：id、code、market、trade_date、analyst、model（可选）、synthetic 和 snapshot。snapshot 使用 entities.MarketBrief 的JSON结构，参见 fixtures/synthetic-six-analysts.json。code必须规范化；内部Code值包含Symbol/Market/Raw，Raw只是输入拼写。实际历史样本需携带真实来源，不能把合成样本的 synthetic 标记去掉当作真实数据。

CompareAnalyst 两臂看到字节一致、按权限过滤的证据：基线一次内联，实验臂按需调用工具；固定真实模型，关闭完成缓存。报告保存逐样本两臂内容、原始证据、版本、调用、费用和错误；汇总token、存储费用、未知费用调用数、Agent契约通过数、基线失败数，以及整个样本（配对时为两臂合计）的P50/P95耗时。不同方向不是质量得分。

--mock只验证程序链路。真实模型在合成样本上的结果只验证工具调用与契约遵循。历史投资质量需要单独收集有真实来源、可获得时间及人工标注的留出样本，核对事实、证据支持和风险遗漏，再结合既有EvaluationService观察最终决策。没有这些数据时不声称已通过收益验收。

## 并发、预算与失败

每个Agent每轮最多执行3个工具，工具并发2；单个Runtime实例及其克隆共享4个模型请求名额。评测最多并行2个样本，父context取消会传递；单Agent90秒，总评测10分钟。工具只读，无外部写入副作用；失败和取消仍保留已产生用量。

token软限制默认20000，下一请求前检查。Run/RunSolo/RunAnalyst继承引擎MaxCostUSD；CLI通过--max-cost设置整批预算。每个请求预估输入字节和最大输出，原子预留额度，响应后按存储费用结算；并行成员共用预留账本。未知价格在启用美元预算时拒绝调用；厂商未返回计费用量时保留已预留金额，以免重试把预算释放掉。预留金额与实际费用分别记录。

报价、token化、隐藏推理和网络重试可能影响实际账单，因此是估算准入控制，不承诺绝对美元硬上限。另有--max-requests硬性限制逻辑Complete调用次数，不等同于HTTP重试次数。

## 成本与持久化

turns[].model_calls内嵌保存真实模型、厂商、时间、耗时、token、cost_usd、reserved_cost_usd、pricing_version、cost_status及失败状态；agent_artifact保存版本、角色、冻结证据和原始输出。整份AnalysisContext经repo.Save一次原子写入Mongo文档，无tx泄漏到应用层，也不单独保存子记录。决策链接口返回modelCalls和agentArtifact。

所有乘除推导的费用和预留金额在计算现场产生并随轨迹保存，历史查询只读存储值。builtin-v1是原静态报价表的版本标记；estimated表示估价，unknown_price/unknown_usage表示无法确认，不能当作免费。未改变已有报价，未宣称可用于账单对账。

## 验证

自动测试覆盖六角色本职证据、越权/未见引用、历史披露过滤、同标的不同写法、市场截止日、预算并发预留与未知费用保守处理、费用BSON往返、CLI六角色配对及防覆盖。另修复任务状态时间在低分辨率系统时钟上不前进的问题，统一通过状态迁移函数维护。

持续验证命令：go test ./...

## 本次验证记录（2026-10-31，Asia/Shanghai）

- 全仓 go test ./... 在允许本地HTTP测试连接的环境下通过。首次沙箱失败源于本地socket限制，时间戳测试现已通过。
- 六角色mock配对重复两轮：12/12通过，仅验证程序链路。
- 使用现有DeepSeek配置执行真实模型、合成快照配对：Agent契约6/6通过，完整0、部分0、证据不足6，基线失败0。
- 两臂合计20445 token；静态报价表估算0.0104美元，未知费用调用0。实际账单以供应商为准。
- 详细报告：[真实模型合成样本验证](live-smoke-evaluation-network.json)。初次DNS受限的失败报告保留在live-smoke-evaluation.json，便于区分环境失败与模型失败。
- 尚未收集带人工标注的真实历史留出集，因此没有声称投资质量提升。

真实模型在上述六个合成/缺项样本上全部选择了insufficient，说明缺失数据降级路径可用，不能将6/6契约通过解释为6份完整研究报告。根据验证中板块角色首次请求4工具而上限为3的观察，已移除板块的个股财报工具及指数角色的个股指标工具，两者现均不超过3项授权工具，并要求优先取本职证据。
