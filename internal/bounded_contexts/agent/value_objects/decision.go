package value_objects

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"

	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/decimalx"
)

// DecisionBlockMarker 是我们要求风控经理在报告末尾输出的结构化块的起止标记。
//
// 用一对显眼的自定义标记而不是让模型直接输出 JSON：
// 风控经理的报告正文里本来就会出现代码块、表格和引号，
// 要求整份输出是 JSON 会让正文没处放；而「正文 + 末尾一个固定块」
// 既保留了可读的中文报告，又给了解析器一个明确的锚点。
const (
	DecisionBlockMarker    = "===决策==="
	DecisionBlockEndMarker = "===结束==="
)

// DecisionBlockTemplate 是写进提示词里的样例块。
// 解析器和提示词共用同一个常量，避免「改了提示词忘了改解析器」这种经典脱节。
const DecisionBlockTemplate = DecisionBlockMarker + `
动作: 买入/增持/持有/减仓/卖出 （只能五选一）
置信度: 0.00-1.00 之间的小数
目标价: 数字，无法给出时填 0
止损价: 数字，无法给出时填 0
建议仓位: 0-100 之间的数字，表示占可投资金的百分比
风险评分: 0-10 之间的数字，越高越危险
决策依据: 一段不超过 200 字的说明
一句话结论: 不超过 40 字
` + DecisionBlockEndMarker

// ParseDecision 从风控经理的自由文本里提取结构化决策。
//
// # 为什么不返回 error
//
// 模型输出的结构化程度是不可控的：同一个提示词，今天给标准块，
// 明天可能给一段 JSON，后天可能干脆写成散文。把「没按格式写」当成错误，
// 意味着一次跑了十几分钟、烧了真金白银的分析会在最后一步整体失败。
// 因此解析器按三层依次降级，每一层都只可能让结果更精确、不会让它失败：
//
//  1. 优先找 ===决策=== 块，逐行解析键值；
//  2. 没有块就找 JSON 对象（模型很爱自作主张裹一层 ```json）；
//  3. 键值里没找到动作，就退回全文关键词匹配（analysis 的 ParseAction），
//     实在认不出来就是 undecided——这是一个诚实、可展示、不误导用户的结论。
//
// 返回前统一走 Decision.Normalized()，越界数字被夹回合法区间而不是被拒绝。
func ParseDecision(text string) analysis_vo.Decision {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return analysis_vo.Decision{Action: analysis_vo.ActionUndecided}.Normalized()
	}

	d := analysis_vo.Decision{}

	// 第一层：标准块。找不到结束标记时退而取起始标记之后的全部内容——
	// 模型经常写着写着就忘了收尾。
	block := extractDecisionBlock(raw)
	if block != "" {
		applyKeyValues(&d, parseKeyValues(block))
	}

	// 第二层：JSON。块解析没拿到动作时才尝试，避免一份同时包含两种格式的输出
	// 让后者覆盖前者（块是我们明确要求的格式，优先级更高）。
	if d.Action == "" {
		if obj := extractJSONObject(raw); obj != nil {
			applyJSON(&d, obj)
		}
	}

	// 第三层：全文兜底。整份报告里总会写「建议买入」之类的句子。
	if d.Action == "" || !d.Action.Valid() || d.Action == analysis_vo.ActionUndecided {
		d.Action = analysis_vo.ParseAction(preferBlock(block, raw))
	}

	if strings.TrimSpace(d.Summary) == "" {
		d.Summary = firstSentence(stripDecisionBlock(raw))
	}
	if strings.TrimSpace(d.Reasoning) == "" {
		d.Reasoning = truncateRunes(stripDecisionBlock(raw), 600)
	}
	return d.Normalized()
}

// ClaimOf 截取一段报告的核心论点，用于决策链上「这一环说了什么」的一句话概括。
//
// 复用 firstSentence 而不是另写一个提取器：报告的第一句本来就是结论句
// （提示词要求每位成员开门见山），再发明一套「找结论段」的启发式规则，
// 只会得到第二套需要跟着提示词一起维护的解析逻辑。
// 先剥掉结构化决策块，免得风控经理的论点变成「动作: 买入」这种键值行。
func ClaimOf(text string) string {
	return firstSentence(stripDecisionBlock(text))
}

// preferBlock 在做关键词兜底时优先只看结构化块：
// 报告正文里必然充斥着「如果跌破就卖出」这类假设句，
// 拿全文去匹配关键词很容易把假设当成结论。块里没有动作时才退回全文。
func preferBlock(block, full string) string {
	if strings.TrimSpace(block) != "" {
		if a := analysis_vo.ParseAction(block); a != analysis_vo.ActionUndecided {
			return block
		}
	}
	return full
}

var (
	// 数字：允许负号、千分位逗号、小数点。
	reNumber = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?`)
	// 键值行：支持中英文冒号、markdown 粗体与列表符号。
	reKeyValue = regexp.MustCompile(`^\s*(?:[-*>]\s*)?(?:\*\*)?\s*([^:：*]{1,20}?)\s*(?:\*\*)?\s*[:：]\s*(.*)$`)
	// JSON 对象：非贪婪匹配第一个含 action 键的花括号块。
	reJSONObject = regexp.MustCompile(`(?s)\{[^{}]*"action"[^{}]*\}`)
)

// extractDecisionBlock 抠出结构化块的正文。
func extractDecisionBlock(text string) string {
	start := strings.Index(text, DecisionBlockMarker)
	if start < 0 {
		// 模型偶尔会把标记写成「决策：」或 markdown 小标题，退一步找中文关键词行。
		return fallbackBlock(text)
	}
	rest := text[start+len(DecisionBlockMarker):]
	if end := strings.Index(rest, DecisionBlockEndMarker); end >= 0 {
		return rest[:end]
	}
	return rest
}

// fallbackBlock 在没有显式标记时，截取从第一处「动作/决策」键值行开始的尾部。
// 只截尾部而不是返回全文，是为了不让正文里的假设句混进键值解析。
func fallbackBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		m := reKeyValue.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch normalizeKey(m[1]) {
		case "action", "动作", "决策", "操作建议", "交易动作":
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

// stripDecisionBlock 去掉结构化块，剩下的是可读的报告正文。
func stripDecisionBlock(text string) string {
	start := strings.Index(text, DecisionBlockMarker)
	if start < 0 {
		return text
	}
	head := text[:start]
	rest := text[start:]
	if end := strings.Index(rest, DecisionBlockEndMarker); end >= 0 {
		head += rest[end+len(DecisionBlockEndMarker):]
	}
	return strings.TrimSpace(head)
}

// parseKeyValues 把块解析成键值对，键统一小写去空格。
// 同一个键重复出现时保留第一次：模型复述格式说明时会把样例也写一遍，
// 而样例总是排在真正的答案之后。
func parseKeyValues(block string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		m := reKeyValue.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := normalizeKey(m[1])
		val := strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`\"'"))
		if key == "" || val == "" {
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = val
		}
	}
	return out
}

func normalizeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	k = strings.NewReplacer(" ", "", "_", "", "-", "", "　", "").Replace(k)
	return k
}

// applyKeyValues 把解析出来的键值填进决策。
// 每一类字段都收多个别名：模型对「目标价」的叫法在不同温度下能有五六种。
func applyKeyValues(d *analysis_vo.Decision, kv map[string]string) {
	if v, ok := firstOf(kv, "动作", "决策", "操作", "操作建议", "交易动作", "建议", "action", "decision", "recommendation"); ok {
		d.Action = analysis_vo.ParseAction(v)
	}
	if v, ok := firstOf(kv, "置信度", "信心", "信心度", "把握", "confidence", "conf"); ok {
		d.Confidence = normalizeConfidence(parseNumber(v))
	}
	if v, ok := firstOf(kv, "目标价", "目标价位", "目标价格", "targetprice", "target"); ok {
		d.TargetPrice = parseNumber(v)
	}
	if v, ok := firstOf(kv, "止损价", "止损", "止损价位", "stoploss", "stop"); ok {
		d.StopLoss = parseNumber(v)
	}
	if v, ok := firstOf(kv, "建议仓位", "仓位", "仓位建议", "position", "positionsize"); ok {
		d.Position = normalizePosition(parseNumber(v))
	}
	if v, ok := firstOf(kv, "风险评分", "风险分", "风险分数", "风险等级", "riskscore", "risk"); ok {
		d.RiskScore = parseNumber(v)
	}
	if v, ok := firstOf(kv, "决策依据", "理由", "依据", "分析", "reasoning", "rationale"); ok {
		d.Reasoning = v
	}
	if v, ok := firstOf(kv, "一句话结论", "结论", "摘要", "总结", "summary", "conclusion"); ok {
		d.Summary = v
	}
}

// decisionJSON 是模型自作主张输出 JSON 时的宽松形态。
// 每个数字字段都用 json.Number 收：模型会把 0.75 写成 "0.75"，
// 用 float64 接会直接解析失败并丢掉整个对象。
//
// json.Number 底层就是原始字符串，交给 decimal.NewFromString 是一步无损转换；
// 原先的 n.Float64() 则要先过一遍二进制浮点，模型给的 12.85 到这里就已经
// 变成 12.849999999999999 了——而这个数会被当作目标价写进报告。
type decisionJSON struct {
	Action      string      `json:"action"`
	Confidence  json.Number `json:"confidence"`
	TargetPrice json.Number `json:"target_price"`
	StopLoss    json.Number `json:"stop_loss"`
	Position    json.Number `json:"position"`
	RiskScore   json.Number `json:"risk_score"`
	Reasoning   string      `json:"reasoning"`
	Summary     string      `json:"summary"`
}

func extractJSONObject(text string) *decisionJSON {
	m := reJSONObject.FindString(text)
	if m == "" {
		return nil
	}
	var obj decisionJSON
	if err := json.Unmarshal([]byte(m), &obj); err != nil {
		return nil
	}
	return &obj
}

func applyJSON(d *analysis_vo.Decision, obj *decisionJSON) {
	d.Action = analysis_vo.ParseAction(obj.Action)
	d.Confidence = normalizeConfidence(numberOf(obj.Confidence))
	d.TargetPrice = numberOf(obj.TargetPrice)
	d.StopLoss = numberOf(obj.StopLoss)
	d.Position = normalizePosition(numberOf(obj.Position))
	d.RiskScore = numberOf(obj.RiskScore)
	if obj.Reasoning != "" {
		d.Reasoning = obj.Reasoning
	}
	if obj.Summary != "" {
		d.Summary = obj.Summary
	}
}

func numberOf(n json.Number) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(n.String()))
	if err != nil {
		return decimal.Zero
	}
	return d
}

// parseNumber 取字符串里的第一个数字，认不出来返回 0。
//
// 只取第一个是刻意的：模型很爱写「目标价: 12.8~13.5」或「止损: 10.5（-8%）」，
// 取第一个数字在这两种写法下都给出保守且正确的答案。
func parseNumber(s string) decimal.Decimal {
	m := reNumber.FindString(s)
	if m == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(strings.ReplaceAll(m, ",", ""))
	if err != nil {
		return decimal.Zero
	}
	return d
}

// normalizeConfidence 统一置信度量纲。
// 模型一半时间给 0.75，一半时间给 75（或 "75%"），两种都得认。
// 分界线放在 1：置信度恰好等于 1 的语义是「完全确定」，不该被当成 1%。
func normalizeConfidence(v decimal.Decimal) decimal.Decimal {
	if v.GreaterThan(dOne) {
		return v.DivRound(hundred, decimalx.RatioScale+2)
	}
	return v
}

// normalizePosition 统一仓位量纲，语义是百分比。
// (0,1] 区间的值一律按小数比例理解（0.3 = 30%），大于 1 的按百分数理解。
func normalizePosition(v decimal.Decimal) decimal.Decimal {
	if v.IsPositive() && v.LessThanOrEqual(dOne) {
		return v.Mul(hundred)
	}
	return v
}

func firstOf(kv map[string]string, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := kv[k]; ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}

// firstSentence 取正文的第一句，用作兜底摘要。
func firstSentence(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	// 先按行切，标题行（# 开头）跳过，避免摘要变成「## 风控终裁报告」。
	for _, line := range strings.Split(t, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#*->  "))
		if line == "" {
			continue
		}
		for i, r := range line {
			if r == '。' || r == '!' || r == '！' || r == '?' || r == '？' {
				return truncateRunes(line[:i], 60)
			}
		}
		return truncateRunes(line, 60)
	}
	return ""
}

// truncateRunes 按 rune 截断，避免把一个中文字符切成两半。
func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
