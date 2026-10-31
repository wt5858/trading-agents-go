package domain_services

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Search is restricted to the role's already authorized, point-in-time corpus.
// It performs no network/repository IO and returns original segment identities.
type frozenSearch struct{ registry frozenRegistry }

func (s frozenSearch) Spec() vo.ToolSpec {
	return vo.NewToolSpec(vo.ToolSearchEvidence, "在本职冻结证据内按关键词检索，空格分隔关键词，按匹配数排序；不搜索互联网。保留来源和原始文档ID，offset/limit分页，仅按next_offset继续；has_more=false或next_offset=null表示结束，total=0时停止翻页。", vo.NewParamSchema(map[string]vo.ParamField{"query": {Type: "string"}, "offset": {Type: "integer"}, "limit": {Type: "integer"}}, "query"))
}
func (s frozenSearch) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", custom_errors.Unavailable("工具调用已取消").Wrap(err)
	}
	var args struct {
		Query  string `json:"query"`
		Offset int    `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	d := json.NewDecoder(strings.NewReader(string(in.Arguments)))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return "", custom_errors.Invalid("检索参数无效；请提供 query 字符串及可选的 offset、limit").Wrap(err)
	}
	if d.Decode(new(any)) != io.EOF {
		return "", custom_errors.Invalid("检索参数只能包含一个 JSON 对象")
	}
	terms := strings.Fields(strings.ToLower(args.Query))
	if len(terms) == 0 || len(terms) > 12 || len([]rune(args.Query)) > 500 {
		return "", custom_errors.Invalid("query 必须包含 1..12 个空格分隔的关键词，且不超过 500 个字符")
	}
	limit := 1
	if args.Limit != nil {
		limit = *args.Limit
	}
	type hit struct {
		doc   evidenceDocument
		score int
	}
	var hits []hit
	seen := map[string]bool{}
	var meta frozenEvidence
	for _, t := range s.registry.tools {
		if meta.SnapshotID == "" {
			meta = t.evidence
		}
		for _, doc := range t.documents {
			if seen[doc.ID] {
				continue
			}
			seen[doc.ID] = true
			text := strings.ToLower(doc.Content)
			score := 0
			for _, term := range terms {
				if strings.Contains(text, term) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, hit{doc, score})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].doc.ID < hits[j].doc.ID
	})
	var docs []evidenceDocument
	for _, h := range hits {
		docs = append(docs, h.doc)
	}
	t := frozenTool{spec: s.Spec(), documents: docs, evidence: frozenEvidence{SnapshotID: meta.SnapshotID, AsOf: meta.AsOf, Source: "authorized_frozen_search"}}
	if len(docs) == 0 {
		t.evidence.MissingReason = "本次检索没有匹配记录；可更换关键词，相同查询不会刷新冻结数据。"
	}
	for _, original := range s.registry.tools {
		t.evidence.Truncated = t.evidence.Truncated || original.evidence.Truncated
	}
	page, err := t.page(args.Offset, limit, "")
	if err != nil {
		return "", err
	}
	page.ID = evidenceDigest(page.ID + args.Query)
	raw, err := json.Marshal(page)
	if len([]rune(string(raw))) > 15000 {
		return "", custom_errors.Invalid("检索结果页过大，请将 limit 减为 1")
	}
	return string(raw), err
}
