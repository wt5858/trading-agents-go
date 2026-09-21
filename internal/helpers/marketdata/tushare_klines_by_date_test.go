package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// 按交易日拉全市场 K 线**必须翻页**。
//
// A 股在市标的已经 5900 余只，而 Tushare 的 daily 单次有返回上限——就贴着这个数。
// 赌一次拉完的后果不是报错，而是「某一天莫名其妙少了几百只票的日线」，
// 而缺口要等到有人对着某只票的图发现断档才会暴露。
func TestFetchKlinesByDatePagesUntilExhausted(t *testing.T) {
	const tailRows = 917

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
		if req.APIName != "daily" {
			t.Errorf("日线应当调用 daily，实际 %s", req.APIName)
		}
		// 日期必须是紧凑格式：传 YYYY-MM-DD 会被 Tushare 当成非法参数静默返回空集，
		// 而那会让这一天看起来像休市。
		if got := req.Params["trade_date"]; got != "20260918" {
			t.Errorf("trade_date 应为 20260918，实际 %v", got)
		}
		if _, ok := req.Params["ts_code"]; ok {
			t.Error("按交易日批量拉时不该传 ts_code，传了就退化成单只查询")
		}
		offset := int(req.Params["offset"].(float64))
		offsets = append(offsets, offset)

		n := klinesByDatePageSize
		if offset > 0 {
			n = tailRows
		}
		writeTushareKlineRows(t, w, offset, n)
	}))
	defer srv.Close()

	p := NewTushareProvider(fakeProviderToken, srv.Client())
	p.endpoint = srv.URL

	klines, err := p.FetchKlinesByDate(context.Background(),
		shared_vo.MarketCN, value_objects.PeriodDaily, shared_vo.MustTradeDate("2026-09-18"))
	if err != nil {
		t.Fatalf("按交易日拉取 K 线失败: %v", err)
	}
	if want := []int{0, klinesByDatePageSize}; fmt.Sprint(offsets) != fmt.Sprint(want) {
		t.Fatalf("翻页 offset 序列错误: 得到 %v，期望 %v", offsets, want)
	}
	if want := klinesByDatePageSize + tailRows; len(klines) != want {
		t.Fatalf("只拿到 %d 根 K 线，期望 %d——翻页提前终止了", len(klines), want)
	}
	// 单位换算必须跟逐标的那条路径一致，否则同一只票的成交量会因为走了哪条路而不同。
	first := klines[0]
	if got := first.Volume.String(); got != "100" { // vol=1 手 -> 100 股
		t.Fatalf("成交量应换算成股（1 手 = 100 股），实际 %s", got)
	}
	if got := first.Amount.String(); got != "1000" { // amount=1 千元 -> 1000 元
		t.Fatalf("成交额应换算成元（1 千元 = 1000 元），实际 %s", got)
	}
	if first.Code.Symbol == "" {
		t.Fatal("按交易日拉回来的 K 线必须自带代码，否则落库时没有自然键")
	}
}

// 周线/月线是独立接口而不是 daily 的参数，走错接口会拿回完全不同周期的数据，
// 而它们的字段结构一模一样——不会报错，只会静默污染。
func TestFetchKlinesByDateUsesPeriodSpecificAPI(t *testing.T) {
	for period, wantAPI := range map[value_objects.Period]string{
		value_objects.PeriodDaily:   "daily",
		value_objects.PeriodWeekly:  "weekly",
		value_objects.PeriodMonthly: "monthly",
	} {
		var gotAPI string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				APIName string `json:"api_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotAPI = req.APIName
			writeTushareKlineRows(t, w, 0, 1)
		}))

		p := NewTushareProvider(fakeProviderToken, srv.Client())
		p.endpoint = srv.URL
		if _, err := p.FetchKlinesByDate(context.Background(),
			shared_vo.MarketCN, period, shared_vo.MustTradeDate("2026-09-18")); err != nil {
			t.Fatalf("%s 拉取失败: %v", period, err)
		}
		srv.Close()

		if gotAPI != wantAPI {
			t.Fatalf("周期 %s 应当调用 %s，实际 %s", period, wantAPI, gotAPI)
		}
	}
}

// 脏行（代码解析不出来）要整行跳过，不能拼一个空 Code 放进去：
// 没有自然键的 K 线会在落库时被丢掉，但已经先虚报了一次成功。
func TestFetchKlinesByDateSkipsUnparsableRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"code": 0,
			"data": map[string]any{
				"fields": []string{"ts_code", "trade_date", "open", "high", "low", "close", "vol", "amount"},
				"items": [][]any{
					{"600000.SH", "20260918", 1, 2, 0.5, 1.5, 1, 1},
					{"", "20260918", 1, 2, 0.5, 1.5, 1, 1},  // 代码为空
					{"600001.SH", "", 1, 2, 0.5, 1.5, 1, 1}, // 日期为空
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewTushareProvider(fakeProviderToken, srv.Client())
	p.endpoint = srv.URL

	klines, err := p.FetchKlinesByDate(context.Background(),
		shared_vo.MarketCN, value_objects.PeriodDaily, shared_vo.MustTradeDate("2026-09-18"))
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(klines) != 1 {
		t.Fatalf("三行里只有一行合法，实际留下 %d 行", len(klines))
	}
}

// writeTushareKlineRows 按 Tushare 的列式结构造 n 行日线。
func writeTushareKlineRows(t *testing.T, w http.ResponseWriter, offset, n int) {
	t.Helper()
	items := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		symbol := fmt.Sprintf("%06d", 600000+offset+i)
		items = append(items, []any{symbol + ".SH", "20260918", 10.0, 11.0, 9.0, 10.5, 1, 1})
	}
	resp := map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"fields": []string{"ts_code", "trade_date", "open", "high", "low", "close", "vol", "amount"},
			"items":  items,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Errorf("写响应失败: %v", err)
	}
}
