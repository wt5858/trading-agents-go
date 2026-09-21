package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// newTestEastmoney 把 provider 指向本地桩服务，并关掉限速（rps<=0）。
// 限速本身在 throttle_test.go 里单独测，这里测的是报文映射。
func newTestEastmoney(srv *httptest.Server) *EastmoneyProvider {
	p := NewEastmoneyProvider(srv.Client(), 0, 0)
	for m := range p.spotHosts {
		p.spotHosts[m] = srv.URL
	}
	p.klineHost = srv.URL
	return p
}

// clistPage 造一页 clist 响应。quoteTime 取昨天，避开「未来日期」这条守卫。
func clistPage(total int, rows []map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"rc":   0,
		"data": map[string]any{"total": total, "diff": rows},
	})
	return string(b)
}

func spotRowJSON(code, name string, extra map[string]any) map[string]any {
	row := map[string]any{
		"f2":   10.5,
		"f3":   1.07,
		"f5":   45123,
		"f12":  code,
		"f13":  0,
		"f14":  name,
		"f20":  1106601885,
		"f21":  560757718,
		"f124": time.Now().Add(-24 * time.Hour).Unix(),
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// TestEastmoneyClistPaginatesByTotal 钉住翻页按响应里的 total 走完。
//
// 东财的 pz 是服务端硬顶 100（实测请求 1000/5000 都只回 100 条，且不报错），
// 所以「一次要够多」这条路是堵死的，只能翻页。翻不完就静默少同步几千只票。
func TestEastmoneyClistPaginatesByTotal(t *testing.T) {
	const total = 250
	var pages []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
		pages = append(pages, pn)

		start := (pn - 1) * eastmoneyPageSize
		n := eastmoneyPageSize
		if start+n > total {
			n = total - start
		}
		rows := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			code := fmt.Sprintf("%06d", 600000+start+i)
			rows = append(rows, spotRowJSON(code, "测试股份", nil))
		}
		_, _ = w.Write([]byte(clistPage(total, rows)))
	}))
	defer srv.Close()

	quotes, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("取批量行情失败: %v", err)
	}
	if len(quotes) != total {
		t.Fatalf("只拿到 %d 条，期望 %d——翻页提前结束了", len(quotes), total)
	}
	if want := []int{1, 2, 3}; fmt.Sprint(pages) != fmt.Sprint(want) {
		t.Fatalf("翻页序列 %v，期望 %v", pages, want)
	}
}

// TestEastmoneyClistFailsOnShortSweep 半截列表绝不能当成成功返回。
//
// 上层拿本地标的全集当分母算覆盖率，收到半截会把没问到的票记成「源没给」并计入失败，
// 而真相是「我们没问完」。这两件事的处置方式完全不同。
func TestEastmoneyClistFailsOnShortSweep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pn, _ := strconv.Atoi(r.URL.Query().Get("pn"))
		if pn == 1 {
			rows := make([]map[string]any, 0, eastmoneyPageSize)
			for i := 0; i < eastmoneyPageSize; i++ {
				rows = append(rows, spotRowJSON(fmt.Sprintf("%06d", 600000+i), "测试股份", nil))
			}
			_, _ = w.Write([]byte(clistPage(250, rows)))
			return
		}
		// 第二页开始返回空 diff —— 这正是东财软限流的形态。
		_, _ = w.Write([]byte(clistPage(250, nil)))
	}))
	defer srv.Close()

	if _, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketCN); err == nil {
		t.Fatal("翻页没取完必须报错，不能返回半截列表")
	}
}

// TestEastmoneyClistFailsOnNullData rc=0 但 data 为 null 是限流的另一种形态。
// 当成空集会让同步报「成功 0 条」，那是最难排查的失败。
func TestEastmoneyClistFailsOnNullData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rc":0,"data":null}`))
	}))
	defer srv.Close()

	if _, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketCN); err == nil {
		t.Fatal("data 为 null 必须报错")
	}
}

// TestEastmoneyHandlesMissingNumericPlaceholder 停牌股的数值字段是字符串 "-"。
// 裸类型断言会 panic，静默当 0 则会把停牌价写成 0——后者至少不该让整批同步崩掉。
func TestEastmoneyHandlesMissingNumericPlaceholder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		row := spotRowJSON("600519", "贵州茅台", map[string]any{"f2": "-", "f20": "-", "f21": "-"})
		_, _ = w.Write([]byte(clistPage(1, []map[string]any{row})))
	}))
	defer srv.Close()

	quotes, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("停牌股不该让整批失败: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("得到 %d 条，期望 1 条", len(quotes))
	}
	if !quotes[0].Close.IsZero() {
		t.Fatalf("缺值应降级为 0，实际 %s", quotes[0].Close)
	}
}

// TestEastmoneyVolumeUnitPerMarket A 股的成交量是「手」，美股本来就是「股」。
// 照抄 Tushare 无条件乘 100 会让美股成交量全部大 100 倍——而成交量是量比、
// 换手这些因子的输入，错了不报错，只会让选股结果悄悄失真。
func TestEastmoneyVolumeUnitPerMarket(t *testing.T) {
	cases := []struct {
		market shared_vo.Market
		code   string
		raw    int
		want   string
	}{
		{shared_vo.MarketCN, "600519", 45123, "4512300"},
		{shared_vo.MarketUS, "AAPL", 45123, "45123"},
	}
	for _, tc := range cases {
		t.Run(tc.market.String(), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				row := spotRowJSON(tc.code, "x", map[string]any{"f5": tc.raw})
				_, _ = w.Write([]byte(clistPage(1, []map[string]any{row})))
			}))
			defer srv.Close()

			quotes, err := newTestEastmoney(srv).FetchQuotes(context.Background(), tc.market)
			if err != nil {
				t.Fatalf("取行情失败: %v", err)
			}
			if len(quotes) != 1 {
				t.Fatalf("得到 %d 条，期望 1 条", len(quotes))
			}
			if got := quotes[0].Volume.String(); got != tc.want {
				t.Fatalf("%s 成交量 = %s，期望 %s", tc.market, got, tc.want)
			}
		})
	}
}

// TestEastmoneyTranslatesUSUnderscoreTicker 东财用 BRK_A 表示 BRK.A，
// 而领域的美股代码正则不收下划线。不翻译的话这类票会被整批静默丢掉。
func TestEastmoneyTranslatesUSUnderscoreTicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		row := spotRowJSON("BRK_A", "Berkshire Hathaway", map[string]any{"f13": 106})
		_, _ = w.Write([]byte(clistPage(1, []map[string]any{row})))
	}))
	defer srv.Close()

	quotes, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketUS)
	if err != nil {
		t.Fatalf("取行情失败: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("BRK_A 被丢掉了，得到 %d 条", len(quotes))
	}
	if got := quotes[0].Symbol(); got != "BRK-A" {
		t.Fatalf("代码是 %q，期望 BRK-A", got)
	}
}

// TestEastmoneyRejectsQuoteWithoutTimestamp f124 缺失时宁可丢掉这一行。
//
// 退而用 time.Now() 会在非交易日造出幽灵记录：clist 在周末照样返回周五的数据，
// 盖上今天的日期之后，LatestQuote 按 trade_date 倒序取，会永远返回那条幽灵。
func TestEastmoneyRejectsQuoteWithoutTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		row := spotRowJSON("600519", "贵州茅台", map[string]any{"f124": "-"})
		_, _ = w.Write([]byte(clistPage(1, []map[string]any{row})))
	}))
	defer srv.Close()

	quotes, err := newTestEastmoney(srv).FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("单行脏数据不该让整批失败: %v", err)
	}
	if len(quotes) != 0 {
		t.Fatalf("没有行情时间的行应当被丢弃，实际保留了 %d 条", len(quotes))
	}
}

// TestEastmoneyStockListCarriesMarketValueInYuan 市值单位必须是元。
// 与 mock、与 screening 的 market_cap 过滤同一口径，差 1e8 会让阈值筛选静默失真。
func TestEastmoneyStockListCarriesMarketValueInYuan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		row := spotRowJSON("600519", "贵州茅台", map[string]any{
			"f20": 2214115013896, "f21": 1666781243896,
		})
		_, _ = w.Write([]byte(clistPage(1, []map[string]any{row})))
	}))
	defer srv.Close()

	list, err := newTestEastmoney(srv).FetchStockList(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("取股票列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("得到 %d 条，期望 1 条", len(list))
	}
	if got := list[0].TotalMV.String(); got != "2214115013896" {
		t.Fatalf("总市值 = %s，期望元为单位的 2214115013896", got)
	}
	// clist 不返回行业/地区/上市日期，这是它的已知短板；
	// 仓储的 upsert 因此做成「空值不覆盖」，见 stockOnConflict。
	if list[0].Industry != "" {
		t.Fatalf("clist 本就没有行业字段，不该凭空出现 %q", list[0].Industry)
	}
}

// TestEastmoneyKlineColumnOrder K 线 CSV 的第 2 列是**收盘**不是最高。
// 照 OHLC 的直觉写会把开收高低整体接错，而错位之后每根 K 线看起来都「像那么回事」。
func TestEastmoneyKlineColumnOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fqt"); got != "0" {
			t.Errorf("fqt = %q，必须是 0（不复权），否则会和 tushare 写的口径互相覆盖", got)
		}
		if got := r.URL.Query().Get("secid"); got != "1.600519" {
			t.Errorf("secid = %q，6 开头应当走上交所前缀 1", got)
		}
		_, _ = w.Write([]byte(`{"rc":0,"data":{"code":"600519","klines":[
			"2025-09-01,1430.22,1424.12,1436.02,1413.72,45123,6658918555.00,1.56,-0.27,-3.90,0.36"]}}`))
	}))
	defer srv.Close()

	klines, err := newTestEastmoney(srv).FetchKlines(context.Background(),
		mustCode(t, "600519", shared_vo.MarketCN), "daily", shared_vo.LastNDays(30))
	if err != nil {
		t.Fatalf("取 K 线失败: %v", err)
	}
	if len(klines) != 1 {
		t.Fatalf("得到 %d 根，期望 1 根", len(klines))
	}
	k := klines[0]
	for _, c := range []struct {
		name, got, want string
	}{
		{"开盘", k.Open.String(), "1430.22"},
		{"收盘", k.Close.String(), "1424.12"},
		{"最高", k.High.String(), "1436.02"},
		{"最低", k.Low.String(), "1413.72"},
		{"成交量(手→股)", k.Volume.String(), "4512300"},
		{"成交额(元)", k.Amount.String(), "6658918555"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s，期望 %s", c.name, c.got, c.want)
		}
	}
	if k.Adjusted {
		t.Error("fqt=0 对应不复权，Adjusted 必须为 false，否则和落库口径自相矛盾")
	}
}

// TestEastmoneyLive 打真实的东方财富接口。
//
// 默认跳过——它依赖外网，而且东财会限流。启用方式：
//
//	TA_TEST_EASTMONEY=1 go test ./internal/helpers/marketdata/ -run TestEastmoneyLive -v
//
// 上面那些桩测试只能证明「我解析得了我自己造的报文」。真正会出问题的是
// 东财改字段、改分页、改主机名这类事，只有打真接口才看得见。
// 这条用例用的是最小请求量（一次 K 线调用），不会把出口 IP 打进封禁。
func TestEastmoneyLive(t *testing.T) {
	if os.Getenv("TA_TEST_EASTMONEY") == "" {
		t.Skip("未设置 TA_TEST_EASTMONEY，跳过打真实东财接口的测试")
	}
	p := NewEastmoneyProvider(nil, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	klines, err := p.FetchKlines(ctx,
		mustCode(t, "600519", shared_vo.MarketCN), "daily", shared_vo.LastNDays(30))
	if err != nil {
		t.Fatalf("取贵州茅台日线失败: %v", err)
	}
	if len(klines) == 0 {
		t.Fatal("近 30 天一根 K 线都没有，接口契约可能变了")
	}
	k := klines[len(klines)-1]
	t.Logf("最新一根: %s O=%s H=%s L=%s C=%s V=%s A=%s",
		k.TradeDate, k.Open, k.High, k.Low, k.Close, k.Volume, k.Amount)

	// OHLC 不变式是判断「列有没有接错」最有效的一条：列序一旦错位，
	// 高低价几乎必然违反这个关系，而单看数值是看不出来的。
	if !k.OHLCValid() {
		t.Fatalf("OHLC 不满足 low <= open,close <= high，列序可能错位: %+v", k)
	}
	if k.Adjusted {
		t.Error("fqt=0 应当对应不复权")
	}
}

// TestEastmoneyLiveFullSweep 打真实接口做一次全市场扫描。
//
// 比上面那条重得多（A 股约 60 页、按默认 rps 要半分钟），所以单独一条，
// 想快速验证契约的人可以只跑 TestEastmoneyLive。
//
// 它验证的是桩测试无论如何覆盖不到的那件事：翻页在**真实的** total 与限流下
// 能不能走完。half-sweep 会被 fetchSpot 判成错误，所以这条一旦过了，
// 就说明拿到的是完整市场。
func TestEastmoneyLiveFullSweep(t *testing.T) {
	if os.Getenv("TA_TEST_EASTMONEY") == "" {
		t.Skip("未设置 TA_TEST_EASTMONEY，跳过打真实东财接口的测试")
	}
	p := NewEastmoneyProvider(nil, 2, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	list, err := p.FetchStockList(ctx, shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("A 股全市场扫描失败: %v", err)
	}
	// A 股在市标的 2026 年在 5900 上下。给一个宽松下界即可——
	// 这里要抓的是「只拿回几百条却报成功」，不是精确计数。
	if len(list) < 4000 {
		t.Fatalf("只拿到 %d 只标的，A 股在市数量远不止于此，翻页可能提前结束", len(list))
	}
	t.Logf("A 股标的数 = %d", len(list))

	withMV := 0
	for _, s := range list {
		if s.TotalMV.IsPositive() {
			withMV++
		}
		// 市值不变式在 entities.List 里已经守过一遍，这里再确认一次口径：
		// 元为单位的 A 股总市值应当是 1e8 量级起步，若整体偏小说明单位错了。
		if s.CircMV.GreaterThan(s.TotalMV) && s.TotalMV.IsPositive() {
			t.Fatalf("%s 流通市值大于总市值，不该通过聚合校验", s.FullSymbol())
		}
	}
	if withMV*2 < len(list) {
		t.Fatalf("只有 %d/%d 只标的有市值，f20 字段可能已经改名", withMV, len(list))
	}
}

func mustCode(t *testing.T, symbol string, market shared_vo.Market) shared_vo.StockCode {
	t.Helper()
	code, err := shared_vo.NewStockCode(symbol, market)
	if err != nil {
		t.Fatalf("构造股票代码失败: %v", err)
	}
	return code
}
