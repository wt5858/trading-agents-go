package value_objects

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
)

// eq 断言解析出的数值与期望**精确**相等。
//
// 这里原先是 math.Abs(got-want) > 1e-9 的近似比较——那个 epsilon 不是业务容差，
// 而是 float64 解析 "1620.50" 得不到精确值的补偿。改用 decimal 之后
// 解析结果与字面量逐位相同，容差没有存在的理由，去掉它才能真正守住
// 「模型写什么、我们就存什么」这条约束。
func eq(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	w := decimal.RequireFromString(want)
	if !got.Equal(w) {
		t.Errorf("%s = %v, 期望 %v", name, got, w)
	}
}

// TestParseDecision_StandardBlock 是提示词严格被遵守时的基准情况。
func TestParseDecision_StandardBlock(t *testing.T) {
	report := `## 对交易方案的裁定
交易员给出的买入方案在当前技术形态下成立，但仓位偏高。

===决策===
动作: 买入
置信度: 0.72
目标价: 1850.00
止损价: 1620.50
建议仓位: 25
风险评分: 5.5
决策依据: 均线多头排列且业绩确定性较高，但估值已处于近三年中位数上方。
一句话结论: 可小仓位买入，严格止损。
===结束===`

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionBuy {
		t.Errorf("Action = %v, 期望 buy", d.Action)
	}
	eq(t, "Confidence", d.Confidence, "0.72")
	eq(t, "TargetPrice", d.TargetPrice, "1850")
	eq(t, "StopLoss", d.StopLoss, "1620.50")
	eq(t, "Position", d.Position, "25")
	eq(t, "RiskScore", d.RiskScore, "5.5")
	if !strings.Contains(d.Reasoning, "均线多头排列") {
		t.Errorf("Reasoning 未解析: %q", d.Reasoning)
	}
	if !strings.Contains(d.Summary, "小仓位买入") {
		t.Errorf("Summary 未解析: %q", d.Summary)
	}
}

// TestParseDecision_DriftedFormat 覆盖模型「大体照做但处处走样」的真实情况：
// markdown 粗体、全角冒号、列表符号、百分号、价格区间、货币符号、
// 以及漏掉的结束标记。这些在同一份输出里同时出现是常态，不是极端情况。
func TestParseDecision_DriftedFormat(t *testing.T) {
	report := `综合判断如下。

===决策===
- **动作**：谨慎买入
- **置信度**：68%
- **目标价**：¥1,850.00 ~ 1,920.00
- **止损价**：1620.5 元（约 -8%）
- **建议仓位**：30%
- **风险评分**：6/10
- **决策依据**：多头证据更硬。
`

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionBuy {
		t.Errorf("Action = %v, 期望 buy", d.Action)
	}
	// 68 必须被识别成 0.68 而不是原样透传后被 Normalized 夹成 1。
	eq(t, "Confidence", d.Confidence, "0.68")
	// 区间取第一个数字，且千分位逗号要被剥掉。
	eq(t, "TargetPrice", d.TargetPrice, "1850")
	eq(t, "StopLoss", d.StopLoss, "1620.5")
	eq(t, "Position", d.Position, "30")
	eq(t, "RiskScore", d.RiskScore, "6")
}

// TestParseDecision_FractionalPosition 仓位写成 0.3 的语义是三成仓，不是 0.3%。
func TestParseDecision_FractionalPosition(t *testing.T) {
	d := ParseDecision("===决策===\n动作: 持有\n建议仓位: 0.3\n置信度: 0.5\n===结束===")
	eq(t, "Position", d.Position, "30")
	if d.Action != analysis_vo.ActionHold {
		t.Errorf("Action = %v, 期望 hold", d.Action)
	}
}

// TestParseDecision_OutOfRangeClamped 越界数字夹回合法区间，而不是让整次分析失败。
func TestParseDecision_OutOfRangeClamped(t *testing.T) {
	d := ParseDecision(`===决策===
动作: 卖出
置信度: 0.99
目标价: -5
止损价: -3
建议仓位: 320
风险评分: 27
===结束===`)

	if d.Action != analysis_vo.ActionSell {
		t.Errorf("Action = %v, 期望 sell", d.Action)
	}
	eq(t, "Position", d.Position, "100")
	eq(t, "RiskScore", d.RiskScore, "10")
	eq(t, "TargetPrice", d.TargetPrice, "0")
	eq(t, "StopLoss", d.StopLoss, "0")
}

// TestParseDecision_JSONDrift 模型自作主张输出 JSON 是最常见的一种走样。
// 数字被写成字符串同样要认，否则整个对象会因为一个引号被丢弃。
func TestParseDecision_JSONDrift(t *testing.T) {
	report := "这是我的结论：\n\n```json\n" +
		`{"action": "sell", "confidence": "0.81", "target_price": 12.5, "stop_loss": "15.2", "position": 0, "risk_score": 8, "summary": "基本面恶化"}` +
		"\n```"

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionSell {
		t.Errorf("Action = %v, 期望 sell", d.Action)
	}
	eq(t, "Confidence", d.Confidence, "0.81")
	eq(t, "TargetPrice", d.TargetPrice, "12.5")
	eq(t, "StopLoss", d.StopLoss, "15.2")
	eq(t, "RiskScore", d.RiskScore, "8")
	if d.Summary != "基本面恶化" {
		t.Errorf("Summary = %q", d.Summary)
	}
}

// TestParseDecision_FreeTextFallback 模型完全无视格式要求时，
// 退回全文关键词匹配仍要给出方向——一次跑了十几分钟的分析
// 不该因为末尾少了一个块就变成「待定」。
func TestParseDecision_FreeTextFallback(t *testing.T) {
	report := `## 风控终裁

综合三方意见，本标的估值已明显透支未来两年的成长性，
下行风险大于上行空间。我的意见是立即卖出并观察。`

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionSell {
		t.Errorf("Action = %v, 期望 sell", d.Action)
	}
	if strings.TrimSpace(d.Summary) == "" {
		t.Error("兜底摘要不应为空")
	}
	if strings.HasPrefix(d.Summary, "#") {
		t.Errorf("摘要不应取到 markdown 标题行: %q", d.Summary)
	}
}

// TestParseDecision_BlockWinsOverBodyHypotheticals 是这个解析器最容易出错的地方。
//
// 报告正文里必然充斥着「若跌破 1600 则卖出」这类假设句。如果兜底匹配拿全文去找关键词，
// 一份明确写着「买入」的决策会被正文里的假设句翻转成「卖出」。
func TestParseDecision_BlockWinsOverBodyHypotheticals(t *testing.T) {
	report := `## 裁定
如果后续跌破 1600 元，应当立即卖出并清仓；若站稳则继续持有观望。

===决策===
动作: 买入
置信度: 0.6
===结束===`

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionBuy {
		t.Errorf("Action = %v, 期望 buy（正文的假设句不得覆盖结构化块）", d.Action)
	}
}

// TestParseDecision_Unparseable 认不出来就是 undecided，且必须是一个合法可展示的值。
func TestParseDecision_Unparseable(t *testing.T) {
	for _, text := range []string{"", "   ", "抱歉，我无法提供投资建议。"} {
		d := ParseDecision(text)
		if d.Action != analysis_vo.ActionUndecided {
			t.Errorf("输入 %q 的 Action = %v, 期望 undecided", text, d.Action)
		}
		if !d.Action.Valid() {
			t.Errorf("输入 %q 产生了非法 Action", text)
		}
	}
}

// TestParseDecision_NoMarkerButKeyValues 模型把标记写丢了，但键值行还在。
func TestParseDecision_NoMarkerButKeyValues(t *testing.T) {
	report := `### 最终决策
动作: 减仓
置信度: 0.45
建议仓位: 10
风险评分: 7`

	d := ParseDecision(report)
	if d.Action != analysis_vo.ActionReduce {
		t.Errorf("Action = %v, 期望 reduce", d.Action)
	}
	eq(t, "Confidence", d.Confidence, "0.45")
	eq(t, "Position", d.Position, "10")
	eq(t, "RiskScore", d.RiskScore, "7")
}

// TestParseDecision_EnglishKeys 英文键同样要认：模型在低温下偶尔会切回英文。
func TestParseDecision_EnglishKeys(t *testing.T) {
	d := ParseDecision(`===决策===
action: hold
confidence: 0.55
target price: 210.4
stop loss: 180
position: 15
risk score: 4
===结束===`)

	if d.Action != analysis_vo.ActionHold {
		t.Errorf("Action = %v, 期望 hold", d.Action)
	}
	eq(t, "Confidence", d.Confidence, "0.55")
	eq(t, "TargetPrice", d.TargetPrice, "210.4")
	eq(t, "StopLoss", d.StopLoss, "180")
	eq(t, "Position", d.Position, "15")
	eq(t, "RiskScore", d.RiskScore, "4")
}

// TestDecisionBlockTemplate_ParsesItself 提示词里的样例块必须能被解析器读懂。
// 这条测试是「提示词和解析器不脱节」的执行点：改了其中一个而忘了另一个，
// 这里立刻红。
func TestDecisionBlockTemplate_ParsesItself(t *testing.T) {
	if !strings.Contains(DecisionBlockTemplate, DecisionBlockMarker) ||
		!strings.Contains(DecisionBlockTemplate, DecisionBlockEndMarker) {
		t.Fatal("样例块缺少起止标记")
	}
	kv := parseKeyValues(extractDecisionBlock(DecisionBlockTemplate))
	for _, key := range []string{"动作", "置信度", "目标价", "止损价", "建议仓位", "风险评分"} {
		if _, ok := kv[key]; !ok {
			t.Errorf("样例块的键 %q 解析不出来，解析器与提示词已脱节", key)
		}
	}
}
