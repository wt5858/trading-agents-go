package value_objects

import "time"

// TurnRecord 是一位成员一次发言的完整轨迹：谁、在哪一阶段、看了多长的提示词、
// 花了多久、烧了多少 token、说了什么、成没成。
//
// # 为什么需要它
//
// 在它之前，一次分析跑完只留下两样东西：一张 kind -> 报告正文的 map，
// 和一份全局汇总的 Usage。这两样东西回答不了任何一个排查问题——
// 哪位成员最慢、哪位最贵、失败的那位是在第几轮工具调用上挂的、
// 同一只票昨天和今天的结论不同是因为提示词变了还是模型抖了。
// 这些问题的共同前提是「按成员、按时序记账」，而那正是本值对象承载的东西。
//
// # 为什么是值对象而不是子实体
//
// 一条发言记录一旦写下就不再变化，也没有独立于本次分析的身份：
// 它的标识就是「第几次发言」。没有生命周期、没有可变状态，
// 因此它是值对象，由 AnalysisContext 这个聚合根持有并追加，
// 外部拿到的永远是副本。
//
// # 为什么提示词只存摘要
//
// 提示词里内联了完整的行情素材，单条轻易上万字符，十四位成员存全文
// 会让一次分析的轨迹比它的报告本身还大一个数量级，而这些字节的信息量极低——
// 它们是由同一份 MarketBrief 渲染出来的，本来就可以重建。
// 真正有排查价值的是「两次调用的输入到底是不是同一份」，
// 这个问题一个摘要就能回答，所以这里只留长度与指纹。
type TurnRecord struct {
	// Seq 是本次分析内的发言序号，从 1 起，由聚合根在写锁内分配。
	//
	// 并行阶段的完成顺序是不确定的，而排查时需要的恰恰是真实完成顺序
	// （谁先出结果、谁拖住了整个阶段），所以序号按「提交时刻」而不是
	// 按计划顺序分配。
	Seq int

	Kind  AgentKind
	Phase Phase

	// StartedAt / Duration 覆盖的是 Act 的全过程：前置条件校验、提示词渲染、
	// 工具调用循环、模型应答，全都算在内。
	// 只记模型调用耗时会漏掉工具循环里的数据库时间，而那正是慢的时候最可疑的一段。
	StartedAt time.Time
	Duration  time.Duration

	// Model 是这次发言实际落到的模型名（路由解析之后的，不是请求里写的那个）。
	// 请求里可以是空串或一个别名，落库要的是真正计费的那一个。
	Model string
	// PromptChars 是渲染后提示词的字符数，PromptDigest 是它的短指纹。
	PromptChars  int
	PromptDigest string

	// Content 是这位成员的最终发言。
	//
	// 与 analysis 上下文 Result.Reports 里的正文是同一份字节，这是刻意的冗余：
	// Reports 是给用户看的成品，只收成功成员的产出、不带顺序也不带耗时；
	// 而轨迹要能独立回放一次分析，包括失败成员在挂掉之前写下的半截内容。
	// 两个限界上下文各存各的副本，好过让回放路径去跨库拼装。
	Content string

	Usage      Usage
	ToolRounds int
	// Truncated 表示工具循环撞到了轮数上限，这份产出可能不完整。
	Truncated bool
	// CacheHit 表示这次发言直接取自缓存，没有真的调模型。
	//
	// 它必须显式记下来，因为命中时 Usage 是零：没有这个标记，
	// 轨迹里就会出现一条「零 token、零耗时、却有完整报告」的记录，
	// 看起来像是计费漏记了。有了它，「省了多少次调用」也才数得出来。
	CacheHit bool

	// Failed / FailReason 记录失败。失败的发言同样进轨迹且同样计消耗：
	// 撞上下文长度上限的那次调用是真花了钱的。
	Failed     bool
	FailReason string
}

// Failing 返回标记为失败的副本。
//
// 做成「先拼完整记录、再标失败」而不是给失败单开一个构造函数，
// 是因为失败的那条记录与成功的那条要记的字段完全一样——
// 耗时、消耗、提示词摘要、已经写了一半的内容，一样都不能少。
func (r TurnRecord) Failing(reason string) TurnRecord {
	r.Failed = true
	r.FailReason = reason
	return r
}

// DurationMS 返回耗时毫秒数，落库与展示都用它——
// time.Duration 是纳秒整数，直接存会得到一串没人读得懂的数字。
func (r TurnRecord) DurationMS() int64 { return r.Duration.Milliseconds() }
