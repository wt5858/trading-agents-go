# 受控 Research（research-v1）

Research 是独立、显式启用的研究入口。已有分析流水线和交易流程继续按原入口运行。它复用分析师 Agent 的工具、引用校验、模型路由、美元预算和运行轨迹，不引入另一套框架。

## 流程

1. 规划员把研究问题拆为 1–4 个子问题，按相关性选择分析师；同一角色最多负责一个子问题，验收条件要求检查反例。
2. 最多同时运行 2 个分析师。每人只看到本题、验收条件和自己的授权冻结证据，不读取其他分析师的报告。
3. 集中审查一次：检查问题覆盖、反例、矛盾、资料缺失和失败任务。缺口必须关联真实的子问题 ID。
4. 仅对可补证的缺口追查，最多 2 轮。`search_evidence` 在该分析师授权的冻结文档片段内检索，保留原始文档 ID、来源、时间和分页信息；没有互联网或仓储查询副作用。一次补证没有读到不同内容的片段时，该问题停止继续补证。新引文不等于独立来源。
5. 汇总员只选择已校验的 finding ID，系统按原始论断和原始引用生成报告。汇总员不能创造新论断，也不能清除审查缺口。新材料会列入报告，但审查发现的冲突仍保留“补证结果待复核”，结果为 partial，直到另行人工判断。

这个版本的 `complete` 表示流程完整、每题有事实或推断、最新分析完整且无已知缺口；不表示研究结论为真。原文引用校验也不能证明推理蕴含关系。只有假设、分析失败、缺数据或被截断时不能得到完整状态。所有情况下 Research 都不发起交易。

## 边界与停止条件

默认每份研究最多 24 次逻辑模型请求、50,000 token 预算、300 秒、2 轮补证；规划、审查、汇总和工具循环全部计入。单分析师沿用 90 秒、最多 3 轮工具往返、每轮 3 次工具调用和 2 个工具并发。

token 准入按序列化请求体字节数、最大输出和固定余量保守预留，返回用量后结算；未返回用量时保留预留。它可能在实际 token 尚有余量时拒绝请求。美元预算同样是估算准入控制，未知价格拒绝真实调用；不承诺与厂商最终账单完全一致。请求次数限制统计 Complete 调用，不统计底层 HTTP 重试。

停止原因包括 completed、evidence_unavailable、no_new_evidence、supplement_limit、budget_exhausted、timeout、cancelled、failed。预算或超时中断返回错误，同时输出已校验的部分结论、未解决问题和已发生费用。JSON 格式错误、越权角色、重复问题、虚构 finding ID 都会被拒绝。

## 历史数据与存储

数据库入口优先读取本地归档，缺失时通过数据准备阶段的有界回源补齐并保存，再冻结；准备最多 24 期财务摘要、180 天内最多 100 条新闻和 60 条舆情。历史行情只使用支持分析日期截止的接口，只有“最新行情”的源不会用于填历史快照。冻结时仍逐条按标的、披露日期及市场分析日截止时间过滤；分页和补证不能越过这个边界。工具级证据段上限沿用 256 段，截断会显式标记。输入 JSON 的冻结快照经过相同过滤，且完全离线。

现有归档包含财务结构化摘要和新闻正文，不包含完整年报、审计报告、现金流附注等通用文档库。涉及这些内容的问题必须保留资料缺失；此版本不等于开放互联网 Deep Research，也不能用实时网页补历史日期的空缺。用真实历史样本评估时还需核对归档实际可获得时间；新闻发布时间本身不能证明历史数据库当时已收录它。

Research 状态由 AnalysisContext 聚合根持有并通过根方法修改；子问题、尝试、审查与报告随整个聚合提交给原有仓储，内嵌在同一 Mongo 文档中原子保存。无跨聚合实体共享，跨上下文只传 ID/值对象；事务仅在仓储内部，没有 tx 向上传递。每个执行尝试独立 execution ID，检查点和最终结果替换同一文档，重试保留历史尝试。

`research_json` 保存完整研究状态；各 turn 的 `purpose` 区分 research:plan、research:q1:r0、research:review、research:synthesis。模型调用及存储费用沿用 model_calls，不重算历史费用。研究运行不参与原交易收益评测和交易流水线的成本均值，以免把研究报告当作交易决策样本。

## 运行

以下从仓库根目录执行。Windows 的 go 不在 PATH 时可用 `& 'C:\Program Files\Go\bin\go.exe'` 替换 go。

```powershell
# 单份冻结输入，验证完整链路，不连接数据库、不调用真实模型
go run . research-run --input docs/agent/fixtures/research-single.json --mock --output research-run.json

# 冻结样本评测：契约检查和人工标注的字面匹配分别统计
go run . research-eval --input docs/agent/fixtures/research-v1.json --mock --output research-eval.json

# 仅校验，不创建输出文件
go run . research-eval --input docs/agent/fixtures/research-v1.json --mock --dry-run

# 数据库研究：需已有本地归档、数据库配置和模型凭据
go run . research-run --code 600519 --market CN --date 2024-03-01 --question '利润改善有哪些证据和反例？' --max-cost 0.20 --output research-db.json
```

`research-run --input` 接收一个 ResearchCase 对象，`research-eval --input` 接收 1–20 个对象的数组。字段包含 id、question、code、market、trade_date、model（可选）、synthetic、snapshot、expected（可选）。格式见 fixtures/research-v1.json。重复 ID、未知输入字段和尾随 JSON 会拒绝。输出文件使用独占创建，不覆盖已有结果。

真实调用默认预算为整次命令 0.20 美元，必须大于零。`--max-requests`、`--max-tokens`、`--supplement-rounds`、`--timeout-seconds` 限制每份研究。评测逐份运行，整批共享美元账本。输出包含完整 ResearchTrace、每次发言与模型费用。部分完成/证据不足本身不是程序错误；契约失败、预算中断和标注不匹配返回非零退出码并保存结果。

模型配置通过原有 config.Load 读取，不会自动加载 .env；可使用仓库的 `node scripts/run-research.cjs research-eval ...` 启动器，仅向子进程加载本地 .env，不打印密钥。

## 评测解释

`--mock` 只验证程序链路。样例全部显式标记 synthetic；expected.status、required_claims 和 forbidden_claims 只检查被选中的已引用论断，问题文本和缺口文本不会误算成答案。契约通过率与标注匹配数分列，不把未标注样本计为质量通过。

验证覆盖双问题并发、限定角色补证、两轮上限、无新内容停止、未知用量预留、请求/token/美元预算、非法规划/审查/汇总、未来资料过滤、工具权限、取消后的部分结果、BSON 往返、CLI 防覆盖及标注失败退出码。合成测试通过不能证明投资准确率提升；是否优于现有分析模式仍需同一冻结历史留出集上的配对质量评测。

2026-10-31 验证：全仓 `go test ./...` 通过；CLI mock 评测 3/3 契约通过、3/3 字面标注匹配，结果为两份 complete、一份 insufficient，450 个模拟 token、0 美元。完整轨迹见 [mock 评测结果](research-v1-mock-results.json)。另已验证单个分析师超时仍进入集中审查、引擎和调用方的美元预算同时生效、准备数据失败也保留 Research 轨迹。本次没有进行真实模型调用或投资质量验收。
