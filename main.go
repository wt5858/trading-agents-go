package main

import "github.com/wt5858/trading-agents-go/cmd"

// swag 要求「总述」与生成的 docs 包在同一棵解析树的根上，而 main 包是唯一
// 不可能被别人 import 的位置——把它放在 server 包里，任何人 import server
// 都会连带把一份文档描述拖进去。
//
// @title                      Trading Agents API
// @version                    1.0
// @description                多智能体股票分析平台 HTTP 接口。
// @description
// @description                所有响应共用同一个信封 {"code":0,"message":"ok","data":{...}}，
// @description                code 为 0 表示成功，非 0 时 data 恒为 null，具体取值见 response 包。
// @description
// @description                健康检查 GET /healthz 不在 /api/v1 之下，因此未收录于本文档。
// @description
// @description                免责声明：本平台仅用于学习与研究，不构成投资建议。分析结论由大模型生成，
// @description                可能包含事实性错误。过往表现不代表未来收益，投资有风险，可能损失本金。
//
// @BasePath                   /api/v1
//
// 标签顺序即 Swagger UI 里的分组顺序，按「先登录、再看行情、再下单分析、最后管理」
// 的使用路径排，而不是按限界上下文的字母序——后者对读文档的人没有任何意义。
//
// @tag.name                   认证
// @tag.description            登录、刷新令牌、查询当前身份。除标的行情外的接口都要先拿到这里的令牌。
// @tag.name                   用户
// @tag.description            用户管理。列表与创建、停用需要管理员权限，改自己的资料与口令不需要。
// @tag.name                   股票行情
// @tag.description            标的主数据、行情、K 线、财务与新闻。唯一一组免登录的接口。
// @tag.name                   自选股
// @tag.description            自选分组与分组内标的的增删改排序。
// @tag.name                   选股
// @tag.description            条件选股与选股模板。先取 /screening/fields 拿字段字典，再据此构造条件。
// @tag.name                   智能体
// @tag.description            智能体阵容与技术指标快照。
// @tag.name                   分析任务
// @tag.description            提交与追踪多智能体分析。任务异步执行，进度经 SSE 推送。
// @tag.name                   分析报告
// @tag.description            分析任务产出的结构化报告。
// @tag.name                   模拟交易
// @tag.description            模拟账户、下单与持仓。全部为模拟撮合，不涉及任何真实资金或真实委托。
// @tag.name                   通知
// @tag.description            站内通知的查询与已读状态。
// @tag.name                   定时任务
// @tag.description            定时任务的增删改与手动触发。cron 为五段式（分 时 日 月 周）。
// @tag.name                   数据同步
// @tag.description            行情与主数据的同步作业触发与历史。
// @tag.name                   系统配置
// @tag.description            模型供应商与系统设置。全部需要管理员权限；返回的 API Key 一律是掩码。
//
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                填入 "Bearer {accessToken}"，令牌由 POST /auth/login 获取。
func main() {
	cmd.Execute()
}
