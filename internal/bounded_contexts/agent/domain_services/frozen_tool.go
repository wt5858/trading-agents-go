package domain_services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/agent/value_objects"
	stock "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

type evidenceDocument struct {
	ContentFingerprint string `json:"content_fingerprint"`
	ID                 string `json:"document_id"`
	Source             string `json:"source"`
	URL                string `json:"url,omitempty"`
	PublishedAt        string `json:"published_at,omitempty"`
	AvailableOn        string `json:"available_on,omitempty"`
	Content            string `json:"content"`
}
type frozenEvidence struct {
	Preparation   *stock.DataPreparation `json:"preparation,omitempty"`
	DataKind      string                 `json:"data_kind,omitempty"`
	MissingFields []string               `json:"missing_fields,omitempty"`
	MissingReason string                 `json:"missing_reason,omitempty"`
	Retryable     bool                   `json:"retryable"`
	Tool          vo.ToolName            `json:"tool"`
	ID            string                 `json:"evidence_id"`
	SnapshotID    string                 `json:"snapshot_id"`
	AsOf          string                 `json:"as_of"`
	Source        string                 `json:"source"`
	Data          string                 `json:"data"`
	Missing       bool                   `json:"missing"`
	Truncated     bool                   `json:"truncated"`
	Documents     []evidenceDocument     `json:"documents,omitempty"`
	NextOffset    *int                   `json:"next_offset"`
	HasMore       bool                   `json:"has_more"`
	Offset        int                    `json:"offset"`
	Total         int                    `json:"total"`
}
type frozenTool struct {
	spec      vo.ToolSpec
	evidence  frozenEvidence
	documents []evidenceDocument
}

func (t frozenTool) Spec() vo.ToolSpec { return t.spec }
func (t frozenTool) page(offset, limit int, documentID string) (frozenEvidence, error) {
	if offset < 0 {
		return frozenEvidence{}, custom_errors.Invalid("offset 必须为大于或等于 0 的整数")
	}
	if limit < 1 || limit > 3 {
		return frozenEvidence{}, custom_errors.Invalid("limit 必须为 1..3；可省略以使用默认值 1")
	}
	docs := t.documents
	if documentID != "" {
		var matches []evidenceDocument
		for _, d := range docs {
			if d.ID == documentID {
				matches = append(matches, d)
			}
		}
		if len(matches) == 0 {
			return frozenEvidence{}, custom_errors.Invalid("document_id 不在本工具的冻结快照中；请使用已返回的文档 ID，或省略 document_id 按 offset 分页")
		}
		docs = matches
	}
	e := t.evidence
	e.Tool = t.spec.Name
	e.Documents = nil
	e.NextOffset = nil
	e.HasMore = false
	e.Offset = offset
	e.Total = len(docs)
	e.Truncated = t.evidence.Truncated
	// A nonnegative offset beyond the corpus is an exhausted page, not a tool
	// failure. Clamp before adding limit so even MaxInt cannot overflow or panic.
	start := offset
	if start > len(docs) {
		start = len(docs)
	}
	end := start + limit
	if end > len(docs) {
		end = len(docs)
	}
	e.Documents = append([]evidenceDocument(nil), docs[start:end]...)
	var content strings.Builder
	for _, d := range e.Documents {
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(d.Content)
	}
	e.Data = content.String()
	e.Missing = len(docs) == 0
	if e.Missing {
		e.Data = "数据缺失"
		if e.MissingReason == "" {
			e.MissingReason = "本工具的冻结证据没有可用记录；本轮重复调用不会刷新数据。"
		}
	} else if len(e.Documents) == 0 {
		e.Data = "已到末页，无后续条目；请停止翻页。"
	}
	if end < len(docs) {
		next := end
		e.NextOffset = &next
		e.HasMore = true
	}
	raw, _ := json.Marshal(e.Documents)
	e.ID = evidenceDigest(e.SnapshotID + t.spec.Name.String() + fmt.Sprint(offset) + string(raw))
	return e, nil
}
func (t frozenTool) Invoke(ctx context.Context, in ToolInvocation) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", custom_errors.Unavailable("工具调用已取消").Wrap(err)
	}
	args := struct {
		Period     string `json:"period"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		DocumentID string `json:"document_id"`
	}{Limit: 1}
	if strings.TrimSpace(string(in.Arguments)) == "null" {
		return "", custom_errors.Invalid("工具参数必须是 JSON 对象；无参数时请传 {}")
	}
	if len(in.Arguments) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(in.Arguments)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return "", custom_errors.Invalid("工具参数无效；请按工具 schema 提供 offset、limit 或 document_id").Wrap(err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return "", custom_errors.Invalid("工具参数只能包含一个 JSON 对象")
		}
	}
	if args.Period != "" && args.Period != "daily" {
		return "", custom_errors.Invalid("本冻结快照仅支持 daily 周期")
	}

	if args.Limit < 1 || args.Limit > 3 {
		return "", custom_errors.Invalid("limit 必须为 1..3；可省略以使用默认值 1")
	}
	for limit := args.Limit; limit >= 1; limit-- {
		page, err := t.page(args.Offset, limit, args.DocumentID)
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(page)
		if err != nil {
			return "", err
		}
		if len([]rune(string(raw))) <= 15000 {
			return string(raw), nil
		}
	}
	return "", custom_errors.Unavailable("证据元数据超出单次工具结果的长度上限")

}
func sealEvidence(reg frozenRegistry, subject string, asOf string) (frozenRegistry, string) {
	type corpus struct {
		Name      vo.ToolName
		Documents []evidenceDocument
		Missing   bool
	}
	var manifest []corpus
	for i := range reg.tools {
		t := &reg.tools[i]
		t.evidence.AsOf = asOf
		if !t.evidence.Missing && len(t.documents) == 0 {
			t.documents = splitDocument(evidenceDocument{Source: t.evidence.Source, AvailableOn: asOf, Content: t.evidence.Data})
		}
		var segments []evidenceDocument
		for _, d := range t.documents {
			segments = append(segments, splitDocument(d)...)
		}
		t.documents = segments
		// Tools have a stable finite corpus. Every returned segment remains independently retrievable.
		if len(t.documents) > 256 {
			t.documents = t.documents[:256]
			t.evidence.Truncated = true
		}
		manifest = append(manifest, corpus{t.spec.Name, t.documents, t.evidence.Missing})
		t.spec.Schema = vo.NewParamSchema(map[string]vo.ParamField{"offset": {Type: "integer", Description: "条目/片段偏移，从0起"}, "limit": {Type: "integer", Description: "每页1..3条，默认1"}, "document_id": {Type: "string", Description: "指定已知文档/片段ID"}})
		t.spec.Description += " 读取冻结证据，保留来源和可获得时间；使用offset/limit翻页，仅按返回的next_offset继续查询。has_more=false或next_offset=null表示结束，total=0表示本快照没有条目，不要再增加offset重试。"
	}
	raw, _ := json.Marshal(manifest)
	id := evidenceDigest(subject + asOf + string(raw))
	for i := range reg.tools {
		reg.tools[i].evidence.SnapshotID = id
		page, _ := reg.tools[i].page(0, 1, "")
		reg.tools[i].evidence = page
	}
	return reg, id
}
func splitDocument(doc evidenceDocument) []evidenceDocument {
	if doc.ContentFingerprint == "" {
		doc.ContentFingerprint = evidenceDigest(strings.Join(strings.Fields(doc.Content), " "))
	}
	if doc.ID == "" {
		doc.ID = evidenceDigest(doc.Source + doc.URL + doc.PublishedAt + doc.AvailableOn + doc.Content)
	}
	rs := []rune(doc.Content)
	if len(rs) <= 1200 {
		return []evidenceDocument{doc}
	}
	var out []evidenceDocument
	for start := 0; start < len(rs); start += 1200 {
		end := start + 1200
		if end > len(rs) {
			end = len(rs)
		}
		part := doc
		part.ID = doc.ID + fmt.Sprintf(":%d", start/1200)
		part.Content = string(rs[start:end])
		out = append(out, part)
	}
	return out
}
