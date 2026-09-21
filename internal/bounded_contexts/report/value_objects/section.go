// Package value_objects 是报告上下文的值对象：构造即校验、不可变、无生命周期。
//
// 本包不感知数据库、HTTP，也不感知分析引擎。它只回答一个问题：
// 「一份报告由哪些章节构成，它们该按什么顺序排」。
package value_objects

import "sort"

// SectionKey 是章节标识。
//
// 证据类章节的 key 与 analysis 上下文 Result.Reports 的 agentID 字面量逐字一致
// （market / fundamentals / ... / risk_manager）。这是契约而不是巧合：投影时直接拿
// key 去 Reports 里取内容，任何一侧改了字面量，对应章节就会在报告里凭空消失，
// 而且是静默消失——没有报错，只是少了一段。用具名常量而不是裸字符串，
// 至少让这条契约在本文件里有一个唯一的、可被 grep 到的执行点。
type SectionKey string

const (
	// SectionConclusion 是唯一一个不来自任何智能体报告的章节：
	// 它由 Report 聚合根据分析决策自行合成。
	SectionConclusion SectionKey = "conclusion"

	SectionMarket       SectionKey = "market"
	SectionFundamentals SectionKey = "fundamentals"
	SectionNews         SectionKey = "news"
	SectionSentiment    SectionKey = "sentiment"
	SectionSector       SectionKey = "sector"
	SectionIndex        SectionKey = "index"

	SectionBull            SectionKey = "bull"
	SectionBear            SectionKey = "bear"
	SectionResearchManager SectionKey = "research_manager"

	SectionTrader SectionKey = "trader"

	SectionRiskAggressive   SectionKey = "risk_aggressive"
	SectionRiskConservative SectionKey = "risk_conservative"
	SectionRiskNeutral      SectionKey = "risk_neutral"
	SectionRiskManager      SectionKey = "risk_manager"
)

func (k SectionKey) String() string { return string(k) }

func (k SectionKey) IsZero() bool { return k == "" }

// Section 是报告的一个章节：位置固定、内容不可变。
//
// Order 冗余存在章节自身上，而不是每次读报告时再查一遍排序表。理由是
// 「报告是一份已经交付给用户的文档」：排序表以后新增或调整维度时，
// 一年前生成的那份报告不该跟着变了顺序。生成那一刻的排布必须被固化下来。
type Section struct {
	Key     SectionKey
	Title   string
	Content string
	Order   int
}

func (s Section) IsZero() bool { return s.Key.IsZero() && s.Content == "" }

// SectionSpec 是章节在报告里的规范标题与规范位置。
type SectionSpec struct {
	Key   SectionKey
	Title string
	Order int
}

// canonicalSections 是章节排序的权威表，表内顺序即阅读顺序：
//
//	结论 -> 分维度证据（技术面/基本面/消息面/情绪/板块/大盘 -> 多空辩论 -> 研究裁决 -> 交易方案）-> 风险
//
// 这个顺序表达的是一条阅读契约，不是审美偏好：
//   - 结论必须在最前——用户打开报告第一眼要看到「买还是卖」，让他先翻六页技术指标
//     再找结论，等于把最重要的信息藏起来；
//   - 证据居中，且按「事实 -> 争论 -> 裁决 -> 方案」递进，读者能顺着看到结论是怎么来的；
//   - 风险必须在最后——风险提示是报告的落点，夹在证据中间会被淹没。
//
// Order 以 10 递增：以后插入新的分析维度时不必重排全表，也不必改动存量报告里
// 已经固化的 Order 值。
var canonicalSections = []SectionSpec{
	{Key: SectionConclusion, Title: "投资结论", Order: 10},

	{Key: SectionMarket, Title: "技术面分析", Order: 20},
	{Key: SectionFundamentals, Title: "基本面分析", Order: 30},
	{Key: SectionNews, Title: "消息面分析", Order: 40},
	{Key: SectionSentiment, Title: "市场情绪分析", Order: 50},
	{Key: SectionSector, Title: "板块轮动分析", Order: 60},
	{Key: SectionIndex, Title: "大盘环境分析", Order: 70},

	{Key: SectionBull, Title: "多头论证", Order: 80},
	{Key: SectionBear, Title: "空头论证", Order: 90},
	{Key: SectionResearchManager, Title: "研究结论", Order: 100},

	{Key: SectionTrader, Title: "交易方案", Order: 110},

	{Key: SectionRiskAggressive, Title: "风险评估·激进视角", Order: 120},
	{Key: SectionRiskConservative, Title: "风险评估·保守视角", Order: 130},
	{Key: SectionRiskNeutral, Title: "风险评估·中性视角", Order: 140},
	{Key: SectionRiskManager, Title: "风险终裁", Order: 150},
}

// trailingOrderBase 是表外章节的起始位置。
//
// 取一个远大于表内最大 Order 的值，保证未登记的章节永远排在已知章节之后，
// 且以后往表里加维度时不会追上它。
const trailingOrderBase = 10000

// CanonicalSections 返回排序表的副本。返回副本而不是切片本身：
// 值对象不可变，把底层数组交出去等于允许调用方改写全局排序契约。
func CanonicalSections() []SectionSpec {
	return append([]SectionSpec(nil), canonicalSections...)
}

// SpecOf 查某个 key 的规范标题与位置。
//
// 线性扫一张十几行的常量表，不建 map：表小到查找开销可以忽略，
// 而 map 会让「表内顺序即阅读顺序」这条最重要的性质从代码里消失。
func SpecOf(key SectionKey) (SectionSpec, bool) {
	for _, spec := range canonicalSections {
		if spec.Key == key {
			return spec, true
		}
	}
	return SectionSpec{}, false
}

// OrderSections 把「章节内容」按权威表投影成有序章节列表。
//
// 三条规则，都刻意如此：
//
//  1. 空内容的章节直接丢弃。分析师是容错执行的，六个视角瞎掉一个是常态；
//     给用户一个只有标题、点开是空白的折叠块，比没有这一节更糟。
//
//  2. 表里没登记的 key 不丢弃，而是按字典序追加在末尾，标题退化为 key 本身。
//     这里不选择「丢弃未知 key」：上游新增一位分析师、有人忘了往表里补一行，
//     代价会是一整段分析内容在报告里静默蒸发——那是一个没人会发现的数据丢失。
//     排在末尾且标题难看，反而是一个能被看见的提示。
//
//  3. 返回的 Order 取自表（或追加位），随章节一起落库，读路径不再重排。
func OrderSections(contents map[SectionKey]string) []Section {
	if len(contents) == 0 {
		return nil
	}

	out := make([]Section, 0, len(contents))
	known := make(map[SectionKey]struct{}, len(canonicalSections))

	for _, spec := range canonicalSections {
		known[spec.Key] = struct{}{}
		if content := contents[spec.Key]; content != "" {
			out = append(out, Section{
				Key:     spec.Key,
				Title:   spec.Title,
				Content: content,
				Order:   spec.Order,
			})
		}
	}

	// 表外章节先收集再排序，保证同一份输入每次产出同样的顺序——
	// map 遍历顺序是随机的，不排序会让同一次分析生成两份章节顺序不同的报告。
	var unknown []SectionKey
	for key, content := range contents {
		if _, ok := known[key]; ok || content == "" || key.IsZero() {
			continue
		}
		unknown = append(unknown, key)
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i] < unknown[j] })
	for i, key := range unknown {
		out = append(out, Section{
			Key:     key,
			Title:   key.String(),
			Content: contents[key],
			Order:   trailingOrderBase + i*10,
		})
	}

	return out
}

// FindSection 在已固化的章节列表里按 key 取一节。
//
// 不按 Order 做二分：章节数是十级别的，线性扫更简单，也不依赖「列表一定有序」
// 这个从数据库读回来时无从保证的前提。
func FindSection(sections []Section, key SectionKey) (Section, bool) {
	for _, s := range sections {
		if s.Key == key {
			return s, true
		}
	}
	return Section{}, false
}
