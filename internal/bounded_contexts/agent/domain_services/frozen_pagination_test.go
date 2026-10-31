package domain_services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/entities"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

func TestMissingEvidencePaginationReturnsTerminalPageThroughRuntime(t *testing.T) {
	turn := testTurn(t, entities.NewNewsAnalyst())
	reg, id := freezeMarket(turn)
	rt := NewRuntimeService(nil, reg, nil, nil, RuntimeConfig{}, nil)
	calls := []vo.ToolCall{{ID: "news-first", Name: vo.ToolGetNews}, {ID: "news-next", Name: vo.ToolGetNews, Arguments: json.RawMessage(`{"offset":1}`)}, {ID: "quote-next", Name: vo.ToolGetQuote, Arguments: json.RawMessage(`{"offset":1}`)}}
	messages, records, err := rt.invokeTools(context.Background(), calls, turn.Contract.Access, ToolInvocation{})
	if err != nil || len(records) != 3 {
		t.Fatalf("tool batch failed: %v %+v", err, records)
	}
	for i, message := range messages {
		var page frozenEvidence
		if err := json.Unmarshal([]byte(message.Content), &page); err != nil {
			t.Fatalf("empty corpus became tool failure: %s", message.Content)
		}
		if !records[i].OK || records[i].FailReason != "" || page.Total != 0 || !page.Missing || page.HasMore || page.NextOffset != nil || len(page.Documents) != 0 || page.SnapshotID != id || page.ID == "" || page.Data != "数据缺失" {
			t.Fatalf("invalid terminal page: %+v %+v", page, records[i])
		}
		if !strings.Contains(message.Content, `"next_offset":null`) {
			t.Fatalf("ambiguous end of pagination: %s", message.Content)
		}
		if i > 0 && page.Offset != 1 {
			t.Fatalf("lost requested offset: %+v", page)
		}
	}
}

func TestEvidencePaginationBeyondLastPageIsSafeAndKeepsCorpusTotal(t *testing.T) {
	reg, _ := sealEvidence(frozenRegistry{tools: []frozenTool{{spec: vo.NewToolSpec(vo.ToolGetNews, "news", vo.NewParamSchema(nil)), documents: []evidenceDocument{{Source: "a", Content: "first"}, {Source: "b", Content: "second"}, {Source: "c", Content: "third"}}}}}, "600519.SH", "2024-03-01")
	tool := reg.tools[0]
	for _, offset := range []int{0, 1, 2, 3, 4, int(^uint(0) >> 1)} {
		page, err := tool.page(offset, 1, "")
		if err != nil || page.Total != 3 || page.Missing {
			t.Fatalf("page offset %d: %+v %v", offset, page, err)
		}
		if offset < 2 {
			if !page.HasMore || page.NextOffset == nil || *page.NextOffset != offset+1 {
				t.Fatalf("lost continuation: %+v", page)
			}
		} else if page.HasMore || page.NextOffset != nil {
			t.Fatalf("stale continuation from first page: %+v", page)
		}
		if offset >= 3 && (len(page.Documents) != 0 || page.Data != "已到末页，无后续条目；请停止翻页。") {
			t.Fatalf("exhausted page misrepresented as missing source: %+v", page)
		}
	}
}

func TestInvalidFrozenArgumentsReturnActionableMessagesAndPreserveCause(t *testing.T) {
	turn := testTurn(t, entities.NewNewsAnalyst())
	reg, _ := freezeMarket(turn)
	rt := NewRuntimeService(nil, reg, nil, nil, RuntimeConfig{}, nil)
	for _, args := range []string{`{"offset":-1}`, `{"limit":0}`, `{"limit":-1}`, `{"limit":4}`, `{"document_id":"unread"}`, `{"offset":"invalid"}`, `{"unknown":true}`, `null`, `{} {}`, `{"period":"weekly"}`} {
		t.Run(args, func(t *testing.T) {
			tool, _ := reg.Lookup(vo.ToolGetNews)
			_, err := tool.Invoke(context.Background(), ToolInvocation{Arguments: json.RawMessage(args)})
			if custom_errors.CodeOf(err) != custom_errors.CodeInvalidArgument {
				t.Fatalf("wrong error classification: %v", err)
			}
			if args == `{"offset":"invalid"}` {
				var cause *json.UnmarshalTypeError
				if !errors.As(err, &cause) {
					t.Fatalf("JSON cause lost: %v", err)
				}
			}
			messages, records, err := rt.invokeTools(context.Background(), []vo.ToolCall{{ID: "invalid", Name: vo.ToolGetNews, Arguments: json.RawMessage(args)}}, turn.Contract.Access, ToolInvocation{})
			if err != nil || records[0].OK || records[0].FailReason == "服务内部错误" || !strings.Contains(messages[0].Content, "工具参数无效") || !strings.Contains(messages[0].Content, "不能证明源数据缺失") {
				t.Fatalf("unhelpful feedback: %+v %+v %v", messages, records, err)
			}
		})
	}
}

func TestResearchSearchUsesSameTerminalPaginationAndTypedArgumentErrors(t *testing.T) {
	turn := testTurn(t, entities.NewNewsAnalyst())
	turn.Research = &vo.ResearchTask{}
	reg, _ := freezeMarket(turn)
	rt := NewRuntimeService(nil, reg, nil, nil, RuntimeConfig{}, nil)
	for _, offset := range []int{0, 1, int(^uint(0) >> 1)} {
		args := json.RawMessage(fmt.Sprintf(`{"query":"无匹配资料","offset":%d}`, offset))
		messages, records, err := rt.invokeTools(context.Background(), []vo.ToolCall{{ID: "search", Name: vo.ToolSearchEvidence, Arguments: args}}, vo.NewDataAccess(vo.ToolSearchEvidence), ToolInvocation{})
		if err != nil || !records[0].OK {
			t.Fatalf("empty search failed: %+v %v", records, err)
		}
		var page frozenEvidence
		if err := json.Unmarshal([]byte(messages[0].Content), &page); err != nil {
			t.Fatal(err)
		}
		if !page.Missing || page.Total != 0 || page.HasMore || page.NextOffset != nil {
			t.Fatalf("invalid search termination: %+v", page)
		}
	}
	for _, args := range []string{`{}`, `{"query":123}`, `{"query":"关键词","offset":-1}`, `{"query":"关键词","limit":0}`, `{"query":"关键词","extra":true}`, `{} {}`} {
		tool, _ := reg.Lookup(vo.ToolSearchEvidence)
		_, err := tool.Invoke(context.Background(), ToolInvocation{Arguments: json.RawMessage(args)})
		if custom_errors.CodeOf(err) != custom_errors.CodeInvalidArgument || custom_errors.MessageOf(err) == "服务内部错误" {
			t.Fatalf("search args became internal error: %s %v", args, err)
		}
	}
}

func TestUnexpectedToolErrorsRemainRedacted(t *testing.T) {
	rt := NewRuntimeService(nil, mapRegistry{tools: map[vo.ToolName]Tool{vo.ToolGetNews: &recordingTool{name: vo.ToolGetNews, err: errors.New("database secret details")}}}, nil, nil, RuntimeConfig{}, nil)
	messages, records, err := rt.invokeTools(context.Background(), []vo.ToolCall{{ID: "failure", Name: vo.ToolGetNews}}, vo.NewDataAccess(vo.ToolGetNews), ToolInvocation{})
	if err != nil || records[0].OK || records[0].FailReason != "服务内部错误" || strings.Contains(messages[0].Content, "secret") {
		t.Fatalf("unexpected internal details exposed: %+v %+v %v", messages, records, err)
	}
}
