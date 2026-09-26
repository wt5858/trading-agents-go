package domain_services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// TestRuntime_ToolTraceRecordsSuccessAndFailure 锁定工具埋点的核心价值：
// 失败的工具调用必须在轨迹里留下痕迹。
//
// 这条性质容易被误以为「反正 TestRuntime_ToolFailureIsFedBackNotFatal 测过了」。
// 那个测的是**模型看到了什么**——失败被包装成一句「工具执行失败，请不要猜测该数据」
// 回灌给模型，模型换个角度继续论证，整位分析师照常成功返回。
// 恰恰因为这套降级如此顺滑，「这份报告是在没拿到行情的情况下写出来的」这件事
// 在轨迹、报告、成本统计里全都看不出来。本测试守的就是那唯一的痕迹。
func TestRuntime_ToolTraceRecordsSuccessAndFailure(t *testing.T) {
	ok := &recordingTool{name: value_objects.ToolGetKlines, result: "K 线数据"}
	broken := &recordingTool{name: value_objects.ToolGetQuote, err: custom_errors.NotFound("行情不存在")}

	client := &scriptedClient{replies: []value_objects.CompletionResult{
		// 第 0 轮：一次要两个工具，其中一个会失败。
		{
			Content: "我查一下行情和 K 线",
			ToolCalls: []value_objects.ToolCall{
				{ID: "c1", Name: value_objects.ToolGetQuote, Arguments: json.RawMessage(`{}`)},
				{ID: "c2", Name: value_objects.ToolGetKlines, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		// 第 1 轮：再补一次 K 线。
		{
			ToolCalls: []value_objects.ToolCall{
				{ID: "c3", Name: value_objects.ToolGetKlines, Arguments: json.RawMessage(`{"limit":120}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		{Content: "行情缺失，本报告仅基于 K 线", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetQuote:  broken,
		value_objects.ToolGetKlines: ok,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("工具失败不应让成员失败: %v", err)
	}

	if len(res.ToolCalls) != 3 {
		t.Fatalf("工具明细条数 = %d, 期望 3（第 0 轮两次 + 第 1 轮一次）: %+v", len(res.ToolCalls), res.ToolCalls)
	}

	// 顺序必须与调用顺序一致：同一轮里按模型给出的先后，跨轮按轮次递增。
	// 乱序会让「模型在第几轮才去补数据」这个判断失去依据。
	wantSeq := []struct {
		round int
		name  value_objects.ToolName
		ok    bool
	}{
		{0, value_objects.ToolGetQuote, false},
		{0, value_objects.ToolGetKlines, true},
		{1, value_objects.ToolGetKlines, true},
	}
	for i, want := range wantSeq {
		got := res.ToolCalls[i]
		if got.Round != want.round {
			t.Errorf("第 %d 条 Round = %d, 期望 %d", i, got.Round, want.round)
		}
		if got.Name != want.name {
			t.Errorf("第 %d 条 Name = %s, 期望 %s", i, got.Name, want.name)
		}
		if got.OK != want.ok {
			t.Errorf("第 %d 条 OK = %v, 期望 %v", i, got.OK, want.ok)
		}
	}

	// 失败条目必须带得出原因；只有 OK=false 而没有原因，排查时等于没记。
	failed := res.ToolCalls[0]
	if failed.FailReason == "" {
		t.Error("失败的工具调用没有记下原因")
	}
	if !strings.Contains(failed.FailReason, "行情不存在") {
		t.Errorf("失败原因 = %q, 期望包含底层错误信息", failed.FailReason)
	}
	// 成功条目不该有失败原因，否则统计失败率时「字段非空」这类朴素口径会出错。
	if res.ToolCalls[1].FailReason != "" {
		t.Errorf("成功的工具调用不应带失败原因: %q", res.ToolCalls[1].FailReason)
	}

	// 成功调用要记下回灌给模型的字符数——它是「这次调用往上下文里塞了多少」的唯一度量。
	if res.ToolCalls[1].ResultChars != len([]rune("K 线数据")) {
		t.Errorf("ResultChars = %d, 期望 %d", res.ToolCalls[1].ResultChars, len([]rune("K 线数据")))
	}
}

// TestRuntime_ToolTraceRecordsTruncation 锁定截断标记。
//
// 截断是静默的：模型只会看到结尾多了一句「结果过长已截断」，
// 而调用方拿到的报告与没截断时毫无区别。一份因为只看到前 4000 字
// 而下错结论的报告，事后只能靠这一列判断。
func TestRuntime_ToolTraceRecordsTruncation(t *testing.T) {
	// limit 必须显著大于截断提示语本身（约 24 字符）。
	// 取一个很小的值（比如 10）会让截断后的结果**比原文更长**，
	// 于是「截断后应该变短」这类直觉断言会失败——而那不是实现的错。
	// 这也正是 clip 必须自己返回截断状态、而不能由调用方比长度推断的原因。
	const limit = 100
	huge := &recordingTool{name: value_objects.ToolGetKlines, result: strings.Repeat("价", limit*3)}

	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{
			ToolCalls: []value_objects.ToolCall{
				{ID: "c1", Name: value_objects.ToolGetKlines, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		{Content: "报告", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetKlines: huge,
	}}, NewPromptService(), nil, RuntimeConfig{MaxToolResultRunes: limit}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("工具明细条数 = %d, 期望 1", len(res.ToolCalls))
	}
	rec := res.ToolCalls[0]
	if !rec.Truncated {
		t.Error("超长工具结果没有被标记为截断")
	}
	// ResultChars 记的是截断**后**真正进了上下文的量，不是工具原本返回的量。
	// 记成截断前的话，这一列会变成「工具查到了多少」，而那个数字回答不了
	// 「上下文被吃掉多少」——后者才是撞上限时要追的账。
	if rec.ResultChars <= limit {
		t.Errorf("ResultChars = %d, 期望大于 %d（含截断提示语）", rec.ResultChars, limit)
	}
	if rec.ResultChars >= len([]rune(huge.result)) {
		t.Errorf("ResultChars = %d, 不应达到未截断时的 %d", rec.ResultChars, len([]rune(huge.result)))
	}
}

// TestRuntime_UnauthorizedToolIsRecordedAsFailure 越权调用同样要进轨迹。
//
// 越权是提示词注入最直接的表现形式。它现在只在日志里留一句、
// 对模型表现为一条普通的工具失败——如果轨迹里也不留痕，
// 「这次运行期间有人尝试过越权取数」就是一件事后完全无法发现的事。
func TestRuntime_UnauthorizedToolIsRecordedAsFailure(t *testing.T) {
	forbidden := &recordingTool{name: value_objects.ToolGetFinancials, result: "机密财报"}
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{
			ToolCalls: []value_objects.ToolCall{
				{ID: "c1", Name: value_objects.ToolGetFinancials, Arguments: json.RawMessage(`{}`)},
			},
			Usage: value_objects.Usage{Calls: 1},
		},
		{Content: "只能基于技术面作答", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{
		value_objects.ToolGetFinancials: forbidden,
	}}, NewPromptService(), nil, RuntimeConfig{}, nil)

	// 市场分析师没有财务工具的授权。
	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("越权调用不应让整位成员失败: %v", err)
	}
	if len(forbidden.invocations()) != 0 {
		t.Fatal("未授权的工具被执行了")
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("越权调用没有进轨迹: %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].OK {
		t.Error("越权调用应记为失败")
	}
	if !strings.Contains(res.ToolCalls[0].FailReason, "未对该智能体授权") {
		t.Errorf("越权原因未记录: %q", res.ToolCalls[0].FailReason)
	}
}

// TestRuntime_NoToolCallsWhenModelAnswersDirectly 守的是「没调就是没调」。
//
// 空轨迹与「调了但没记下来」在读路径上是同一个样子，
// 因此必须有一个测试钉住前者，否则埋点整体失效时没有任何测试会变红。
func TestRuntime_NoToolCallsWhenModelAnswersDirectly(t *testing.T) {
	client := &scriptedClient{replies: []value_objects.CompletionResult{
		{Content: "一次成文的报告", Usage: value_objects.Usage{Calls: 1}},
	}}

	rt := NewRuntimeService(fixedRouter{client}, mapRegistry{map[value_objects.ToolName]Tool{}},
		NewPromptService(), nil, RuntimeConfig{}, nil)

	res, err := rt.Execute(context.Background(), testTurn(t, entities.NewMarketAnalyst()))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.ToolRounds != 0 {
		t.Errorf("ToolRounds = %d, 期望 0", res.ToolRounds)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("没有调用工具时轨迹应为空: %+v", res.ToolCalls)
	}
}
