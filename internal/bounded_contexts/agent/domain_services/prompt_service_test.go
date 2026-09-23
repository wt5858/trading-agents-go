package domain_services

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	analysis_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/analysis/value_objects"
	stock_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	stock_repositories "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	stock_vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 编译期钉住跨上下文端口：stock 的实现一旦改签名，这里立刻红，
// 而不是等到 DI 装配时才发现。放在测试文件里是为了不让生产代码
// 依赖另一个上下文的仓储实现——依赖的只是端口。
var (
	_ MarketReader     = (*stock_repositories.MarketDataRepository)(nil)
	_ MarketBackfiller = (*stock_services.StockService)(nil)
)

func fullContext(t *testing.T) *entities.AnalysisContext {
	t.Helper()
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate("2024-03-01"),
		analysis_vo.DepthExhaustive, nil, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	ac := entities.NewAnalysisContext("run_test", req)
	quote, err := stock_vo.NewQuote(code, shared_vo.MustTradeDate("2024-03-01"))
	if err != nil {
		t.Fatalf("构造行情失败: %v", err)
	}
	quote.Close = dec("1700")
	quote.PreClose = dec("1680")
	quote.Change = dec("20")
	quote.ChangePct = dec("1.19")
	quote.Volume = dec("3200000")
	quote.Amount = dec("5440000000")
	quote.PE = dec("28.4")

	ac.LoadMarketBrief(entities.MarketBrief{
		Name:     "贵州茅台",
		Industry: "白酒",
		Quote:    quote,
		Indicators: value_objects.Indicators{
			Close: dec("1700"), MA5: dec("1690"), MA10: dec("1670"),
			MA20: dec("1650"), MA60: dec("1600"),
			MACDDIF: dec("12.5"), MACDDEA: dec("9.5"), MACDHist: dec("6"),
			RSI6: dec("61"), RSI14: dec("58"),
			BollUpper: dec("1750"), BollMid: dec("1650"), BollLower: dec("1550"),
			ATR14: dec("32.5"), VolMA5: dec("3000000"), VolMA20: dec("2800000"),
			DeviationMA20Pct: dec("3.03"), Samples: 120,
		},
		Financials: []stock_vo.Financial{{
			Code: code, ReportDate: shared_vo.MustTradeDate("2023-12-31"),
			PeriodType: stock_vo.PeriodTypeAnnual,
			Revenue:    dec("150000000000"), NetProfit: dec("74700000000"),
			EPS: dec("59.49"), ROE: dec("34.2"), GrossMargin: dec("91.9"),
			NetMargin: dec("49.8"), DebtRatio: dec("16.4"),
		}},
		Missing: []string{"社交舆情"},
	})

	// 给每一位可能被引用的上游成员都放一份报告，
	// 这样任何一个模板引用缺失都会以「渲染出占位符」而不是 panic 的形式暴露。
	for _, kind := range value_objects.AllKinds() {
		ac.CommitTurn(value_objects.TurnRecord{Kind: kind, Content: "【" + kind.DisplayName() + "的报告正文】"})
	}
	return ac
}

// TestPromptService_RendersEveryCrewMember 十四位成员各两个模板块，
// 少一个块的表现是那位成员在运行期直接失败——而那通常发生在深夜的某次分析里。
func TestPromptService_RendersEveryCrewMember(t *testing.T) {
	svc := NewPromptService()
	ac := fullContext(t)
	crew := entities.NewCrew()

	for _, kind := range value_objects.AllKinds() {
		t.Run(kind.String(), func(t *testing.T) {
			member := crew.Member(kind)
			if member == nil {
				t.Fatalf("花名册里没有 %s", kind)
			}
			msgs, err := svc.Render(entities.Turn{
				Contract: member.Contract(),
				Snapshot: ac.Snapshot(),
			})
			if err != nil {
				t.Fatalf("渲染失败: %v", err)
			}
			if len(msgs) != 2 {
				t.Fatalf("消息数 = %d, 期望 2（system + user）", len(msgs))
			}
			if msgs[0].Role != value_objects.RoleSystem || msgs[1].Role != value_objects.RoleUser {
				t.Fatalf("消息角色错误: %v / %v", msgs[0].Role, msgs[1].Role)
			}

			system, user := msgs[0].Content, msgs[1].Content
			if len(system) < 200 {
				t.Errorf("system 提示词过短（%d 字节），可能模板块写漏了", len(system))
			}
			if !strings.Contains(system, "数据纪律") {
				t.Error("system 缺少通用数据纪律")
			}
			// 标的信息必须出现在 user 里，否则模型不知道在分析哪只票。
			for _, want := range []string{"600519.SH", "贵州茅台", "2024-03-01"} {
				if !strings.Contains(user, want) {
					t.Errorf("user 缺少标的信息 %q", want)
				}
			}
			// 缺失数据要被如实告知，不能让模型以为那块数据是空的。
			if !strings.Contains(user, "社交舆情") {
				t.Error("user 未告知缺失的数据项")
			}
			// 没有未渲染的模板残留。
			if strings.Contains(system, "{{") || strings.Contains(user, "{{") {
				t.Error("提示词里残留了未渲染的模板标记")
			}
		})
	}
}

// TestPromptService_ToolNoteMatchesAuthorization 提示词里列出的工具
// 必须与契约授权一致：多列一个，模型会去调一个必然被拒绝的工具，白烧一轮。
func TestPromptService_ToolNoteMatchesAuthorization(t *testing.T) {
	svc := NewPromptService()
	ac := fullContext(t)
	crew := entities.NewCrew()

	market := crew.Member(value_objects.KindMarketAnalyst)
	msgs, err := svc.Render(entities.Turn{Contract: market.Contract(), Snapshot: ac.Snapshot()})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	system := msgs[0].Content
	for _, name := range market.Contract().Access.Names() {
		if !strings.Contains(system, name.String()) {
			t.Errorf("提示词未列出已授权工具 %s", name)
		}
	}
	if strings.Contains(system, value_objects.ToolGetFinancials.String()) {
		t.Error("提示词列出了未授权的财务工具")
	}

	// 不带工具的成员不应出现「可用工具」小节。
	manager := crew.Member(value_objects.KindRiskManager)
	msgs, err = svc.Render(entities.Turn{Contract: manager.Contract(), Snapshot: ac.Snapshot()})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if strings.Contains(msgs[0].Content, "## 可用工具") {
		t.Error("无授权的成员不应看到可用工具小节")
	}
}

// TestPromptService_RiskManagerCarriesDecisionBlock 风控经理的提示词必须
// 原样带上结构化块模板——解析器认的就是这个格式。
func TestPromptService_RiskManagerCarriesDecisionBlock(t *testing.T) {
	svc := NewPromptService()
	msgs, err := svc.Render(entities.Turn{
		Contract: entities.NewRiskManager().Contract(),
		Snapshot: fullContext(t).Snapshot(),
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	system := msgs[0].Content
	if !strings.Contains(system, value_objects.DecisionBlockMarker) ||
		!strings.Contains(system, value_objects.DecisionBlockEndMarker) {
		t.Fatal("风控经理提示词缺少结构化块标记")
	}
	for _, key := range []string{"动作", "置信度", "目标价", "止损价", "建议仓位", "风险评分"} {
		if !strings.Contains(system, key) {
			t.Errorf("结构化块缺少键 %q", key)
		}
	}
	// 三位辩手的意见必须出现在 user 里，否则终裁就成了对交易方案的复读。
	user := msgs[1].Content
	for _, kind := range []value_objects.AgentKind{
		value_objects.KindRiskAggressive,
		value_objects.KindRiskConservative,
		value_objects.KindRiskNeutral,
	} {
		if !strings.Contains(user, kind.DisplayName()) {
			t.Errorf("终裁提示词缺少 %s 的意见", kind.DisplayName())
		}
	}
}

// TestPromptService_AbsentAgentIsStatedExplicitly 缺席的上游必须被明写出来。
//
// 渲染成空白小节是最危险的处理方式：模型看到空白会自行脑补，
// 而看到「情绪分析师缺席」则会如实降低情绪面的权重。
func TestPromptService_AbsentAgentIsStatedExplicitly(t *testing.T) {
	code, err := shared_vo.NewStockCode("600519", shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	req, err := analysis_vo.NewRequest(code, shared_vo.MustTradeDate("2024-03-01"),
		analysis_vo.DepthExhaustive, nil, "")
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	ac := entities.NewAnalysisContext("run_test", req)
	ac.CommitTurn(value_objects.TurnRecord{Kind: value_objects.KindMarketAnalyst, Content: "技术面报告"})
	ac.CommitTurn(value_objects.TurnRecord{Kind: value_objects.KindSentimentAnalyst}.Failing("舆情数据源超时"))

	msgs, err := NewPromptService().Render(entities.Turn{
		Contract: entities.NewBullResearcher().Contract(),
		Snapshot: ac.Snapshot(),
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	user := msgs[1].Content
	if !strings.Contains(user, "缺席的分析师") || !strings.Contains(user, "舆情数据源超时") {
		t.Errorf("缺席信息未如实告知模型:\n%s", user)
	}
}

// TestFormatIndicators_NoFabricatedZeros 样本不足的指标必须显示「数据不足」，
// 不能把 0 当成真实值喂给模型——模型分不清这两者，但它会拿 0 去算乖离率。
func TestFormatIndicators_NoFabricatedZeros(t *testing.T) {
	text := formatIndicators(value_objects.Indicators{
		Close: dec("10"), MA5: dec("9.8"), Samples: 8,
	})
	if !strings.Contains(text, "MACD: 样本不足") {
		t.Errorf("MACD 样本不足未标注:\n%s", text)
	}
	if !strings.Contains(text, "布林带: 样本不足") {
		t.Errorf("布林带样本不足未标注:\n%s", text)
	}
	if !strings.Contains(text, "MA60=暂无") {
		t.Errorf("MA60 缺失未标注为暂无:\n%s", text)
	}
	if strings.Contains(text, "ATR14") {
		t.Errorf("样本不足时不应输出 ATR:\n%s", text)
	}

	if empty := formatIndicators(value_objects.Indicators{}); !strings.Contains(empty, "暂无技术指标数据") {
		t.Errorf("零值指标未给出明确说明: %s", empty)
	}
}

// dec 是测试里构造 decimal 字面量的简写。
func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }
