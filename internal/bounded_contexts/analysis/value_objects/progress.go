package value_objects

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// Step 是进度中的一个步骤。
type Step struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Failed   bool   `json:"failed"`
	Detail   string `json:"detail,omitempty"`
	ElapsedS int64  `json:"elapsed_seconds"`
}

// Progress 是任务进度值对象。步骤集合在任务创建时按深度与分析师集合确定，
// 这样前端在排队阶段就能画出完整流程图。
//
// # 不可变
//
// Advance / FailStep / MarkDone 全部返回新的 Progress，而不是原地修改。
// 理由不只是「VO 应该不可变」这条教条——进度快照会被并发读取（SSE 订阅者）、
// 被序列化落库、被塞进事件里。原地修改意味着这些读者可能看到半个状态：
// 步骤已标 Done 但 Percent 还没重算。返回新值让每个快照天然自洽。
//
// # 派生量随快照一起持久化
//
// DoneSteps / TotalSteps / Percent / ETASeconds 都是从 Steps 推出来的派生量
// （Percent = 完成数 ÷ 总数 × 100，ETASeconds = 已耗时 ÷ 完成数 × 剩余数）。
// 它们是**字段而不是方法**，并且跟着快照一起写进 analysis_tasks.progress 列。
//
// 为什么不在读路径上算：
//   - Percent 的分母是「当时」的步骤集合。引擎升级后新任务的步骤会变多，
//     读路径重算会让历史任务的百分比凭空变化，用户看到的数字对不上当时的截图。
//   - ETASeconds 依赖 time.Since(StartedAt)。在读路径上算意味着同一条已完成的
//     记录每刷新一次就给出一个更大的 ETA——一个已经结束的任务不该还有「剩余时间」。
//   - 落库的数值与推送给 SSE 的数值必须字节一致，否则页面刷新前后进度条会跳。
//
// 因此：写路径（每次状态迁移）计算一次并固化，读路径只准直接读字段。
//
// 代价是每次推进都要复制一遍步骤数组。这是可接受的：步骤数是十级别，
// 而推进频率是「每个智能体一次」，不在任何热路径上。
type Progress struct {
	Steps      []Step          `json:"steps"`
	CurrentIdx int             `json:"current_index"`
	TotalSteps int             `json:"total_steps"`
	DoneSteps  int             `json:"done_steps"`
	Percent    decimal.Decimal `json:"percent"`
	ETASeconds int64           `json:"eta_seconds"`
	Message    string          `json:"message"`
	StartedAt  time.Time       `json:"started_at"`
	UpdatedAt  time.Time       `json:"updated_at"`

	// Final 表示这是任务终局的那一份快照，不会再有后续更新。
	//
	// 它与「DoneSteps >= TotalSteps」不是一回事，这正是它存在的理由：成功收尾时
	// 两者同时成立，但**失败与取消同样终局，步骤却没跑完**。SSE 处理器原先拿步数
	// 当收流判据，于是失败/取消时流永不收口，前端状态永远停在「运行中」。
	//
	// omitempty 兼容已落库的旧快照：它们反序列化后为 false，而那些记录要么本就在跑，
	// 要么是已终结的老任务（订阅时走 sub.Terminal 判定）。
	Final bool `json:"final,omitempty"`
}

// NewProgress 依据请求推导完整步骤列表。
func NewProgress(req Request) Progress {
	steps := []Step{{Key: "prepare", Name: "准备数据"}}
	for _, id := range req.Analysts {
		steps = append(steps, Step{Key: "analyst:" + id, Name: analystDisplayName(id)})
	}
	if req.Depth.IncludesDebate() {
		steps = append(steps,
			Step{Key: "debate:bull", Name: "多头研究员论证"},
			Step{Key: "debate:bear", Name: "空头研究员论证"},
			Step{Key: "debate:manager", Name: "研究经理裁决"},
		)
	}
	steps = append(steps, Step{Key: "trade", Name: "交易员制定方案"})
	if req.Depth.IncludesRiskPhase() {
		steps = append(steps,
			Step{Key: "risk:aggressive", Name: "激进风控评估"},
			Step{Key: "risk:conservative", Name: "保守风控评估"},
			Step{Key: "risk:neutral", Name: "中性风控评估"},
			Step{Key: "risk:manager", Name: "风控经理终裁"},
		)
	}
	steps = append(steps, Step{Key: "report", Name: "生成分析报告"})

	now := time.Now()
	p := Progress{Steps: steps, StartedAt: now, Message: "排队中"}
	return p.settled(now)
}

func (p Progress) IsZero() bool { return len(p.Steps) == 0 && p.StartedAt.IsZero() }

// Advance 返回「指定步骤已完成」的新进度。未知 key 原样返回，
// 这样引擎新增步骤时不会让旧任务的进度崩掉。
func (p Progress) Advance(key, detail string) Progress {
	idx := p.indexOf(key)
	if idx < 0 {
		return p
	}
	now := time.Now()
	out := p.clone()
	out.Steps[idx].Done = true
	out.Steps[idx].Failed = false
	out.Steps[idx].Detail = detail
	out.Steps[idx].ElapsedS = int64(now.Sub(out.StartedAt).Seconds())
	// CurrentIdx 只前进不后退：分析师阶段是并发跑的，先完成的步骤可能排在后面，
	// 让游标回退会导致前端进度条来回跳。
	if idx >= out.CurrentIdx {
		out.CurrentIdx = idx + 1
	}
	out.Message = out.Steps[idx].Name
	return out.settled(now)
}

// FailStep 返回「指定步骤失败」的新进度。
// 它刻意不改变任务状态：单个分析师失败是否导致整个任务失败，是 Task 聚合的决定。
func (p Progress) FailStep(key, detail string) Progress {
	idx := p.indexOf(key)
	if idx < 0 {
		return p
	}
	out := p.clone()
	out.Steps[idx].Failed = true
	out.Steps[idx].Detail = detail
	// 失败步骤不计入完成数，但派生量仍要重新固化一次：
	// 漏掉这一步会让 UpdatedAt 停在上一帧，订阅者据此判活会误判为卡死。
	return out.settled(time.Now())
}

// MarkDone 返回全部步骤置完成的新进度，供任务成功收尾时使用。
func (p Progress) MarkDone() Progress {
	out := p.clone()
	for i := range out.Steps {
		out.Steps[i].Done = true
	}
	out.CurrentIdx = len(out.Steps)
	out.Message = "分析完成"
	out.Final = true
	return out.settled(time.Now())
}

// MarkFinal 标记终局但**不**把步骤置完成，供失败与取消使用。
//
// 不复用 MarkDone：那会让步骤条全部变绿，而任务实际是在某一步倒下的——
// 把失败画成成功比不画更糟。
func (p Progress) MarkFinal(msg string) Progress {
	out := p.clone()
	out.Message = msg
	out.Final = true
	return out.settled(time.Now())
}

// WithMessage 返回只替换了提示文案的新进度，用于「排队中 -> 分析中」这类纯展示变更。
func (p Progress) WithMessage(msg string) Progress {
	out := p.clone()
	out.Message = msg
	return out.settled(time.Now())
}

// clone 深复制步骤数组。
// 只复制结构体是不够的：Steps 的切片头共享同一段底层数组，
// 改「副本」的元素会同时改掉原值，不可变性会在这里悄悄漏掉。
func (p Progress) clone() Progress {
	out := p
	out.Steps = append([]Step(nil), p.Steps...)
	return out
}

// settled 是唯一计算派生量的地方：完成数、总数、百分比、剩余时间一次性固化。
//
// 全部集中在这里，是为了让「派生量必须与 Steps 自洽」这条约束只有一个执行点——
// 任何新增的变更方法只要走 settled 收尾，就不可能漏算某一个派生字段。
func (p Progress) settled(now time.Time) Progress {
	total := len(p.Steps)
	done := 0
	for _, s := range p.Steps {
		if s.Done {
			done++
		}
	}
	p.TotalSteps = total
	p.DoneSteps = done
	if total == 0 {
		p.Percent = decimal.Zero
	} else {
		p.Percent = decimalx.RoundPercent(
			decimal.NewFromInt(int64(done)).
				Div(decimal.NewFromInt(int64(total))).
				Mul(decimal.NewFromInt(100)),
		)
	}
	// ETA 用「已完成步骤的平均耗时」线性外推剩余步骤。
	// done == 0 时没有可外推的样本；done == total 时已无剩余，两种情况都固化成 0。
	if done == 0 || done >= total {
		p.ETASeconds = 0
	} else {
		// ETA 走整数秒运算：它是时间外推而不是金额，结果本来就要取整成 int64，
		// 引入 decimal 只会多一层构造与取整，换不来任何精度上的收益。
		elapsed := int64(now.Sub(p.StartedAt) / time.Second)
		if elapsed < 0 {
			elapsed = 0
		}
		p.ETASeconds = elapsed * int64(total-done) / int64(done)
	}
	p.UpdatedAt = now
	return p
}

func (p Progress) indexOf(key string) int {
	for i := range p.Steps {
		if p.Steps[i].Key == key {
			return i
		}
	}
	return -1
}

func analystDisplayName(id string) string {
	switch id {
	case "market":
		return "市场技术面分析"
	case "fundamentals":
		return "基本面分析"
	case "news":
		return "新闻面分析"
	case "sentiment":
		return "市场情绪分析"
	case "sector":
		return "板块轮动分析"
	case "index":
		return "大盘环境分析"
	}
	return id + " 分析"
}
