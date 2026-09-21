package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// TestFetchStockListPagesUntilExhausted 钉住「不满一页才算到底」这条终止条件。
//
// 这类截断的危险之处在于它不报错：少拿了几千只票，同步照样标成功，
// 只有对着数据库数行数才看得出来。所以断言的是总条数，不是「有没有报错」。
func TestFetchStockListPagesUntilExhausted(t *testing.T) {
	const tailRows = 37

	var offsets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIName string         `json:"api_name"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败: %v", err)
			return
		}
		if req.APIName != "stock_basic" {
			t.Errorf("期望调用 stock_basic，实际 %s", req.APIName)
		}
		offset := int(req.Params["offset"].(float64))
		offsets = append(offsets, offset)

		// 第一页给满，迫使调用方继续翻；第二页不满一页，作为终止信号。
		n := stockListPageSize
		if offset > 0 {
			n = tailRows
		}
		writeTushareRows(t, w, offset, n)
	}))
	defer srv.Close()

	p := NewTushareProvider(fakeProviderToken, srv.Client())
	p.endpoint = srv.URL

	list, err := p.FetchStockList(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("拉取股票列表失败: %v", err)
	}

	if want := []int{0, stockListPageSize}; fmt.Sprint(offsets) != fmt.Sprint(want) {
		t.Fatalf("翻页 offset 序列错误: 得到 %v，期望 %v", offsets, want)
	}
	if want := stockListPageSize + tailRows; len(list) != want {
		t.Fatalf("只拿到 %d 只标的，期望 %d——翻页提前终止了", len(list), want)
	}
}

// TestFetchStockListStopsOnSinglePartialPage 单页就取完时不该多发一次请求：
// Tushare 按调用次数计频，白跑一趟是实打实的配额浪费。
func TestFetchStockListStopsOnSinglePartialPage(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeTushareRows(t, w, 0, 3)
	}))
	defer srv.Close()

	p := NewTushareProvider(fakeProviderToken, srv.Client())
	p.endpoint = srv.URL

	list, err := p.FetchStockList(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("拉取股票列表失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("发了 %d 次请求，期望 1 次", calls)
	}
	if len(list) != 3 {
		t.Fatalf("得到 %d 只标的，期望 3", len(list))
	}
}

// writeTushareRows 按 Tushare 的列式结构造 n 行合法的 A 股主数据，
// 代码从 600000+offset 起递增，保证跨页不重复。
func writeTushareRows(t *testing.T, w http.ResponseWriter, offset, n int) {
	t.Helper()
	items := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		symbol := fmt.Sprintf("%06d", 600000+offset+i)
		items = append(items, []any{symbol + ".SH", symbol, "测试股份", "深圳", "软件服务", "20100101", "L"})
	}
	resp := map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"fields": []string{"ts_code", "symbol", "name", "area", "industry", "list_date", "list_status"},
			"items":  items,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}
