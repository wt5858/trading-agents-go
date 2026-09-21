package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// newsServer 造一个假的东财搜索接口，并把收到的 param 交给调用方检查。
func newsServer(t *testing.T, items []map[string]any, onParam func(eastmoneySearchRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb := r.URL.Query().Get("cb")
		if cb == "" {
			// 真实接口不带 cb 会回 400。把这条也仿真出来，
			// 免得将来有人"顺手"去掉 cb 而测试还是绿的。
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if onParam != nil {
			var req eastmoneySearchRequest
			if err := json.Unmarshal([]byte(r.URL.Query().Get("param")), &req); err != nil {
				t.Errorf("param 不是合法 JSON: %v", err)
			}
			onParam(req)
		}
		body, _ := json.Marshal(map[string]any{
			"code": 0,
			"msg":  "OK",
			"result": map[string]any{
				"cmsArticleWebOld": items,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		// 关键：真实响应是 JSONP，外面裹着一层回调。
		fmt.Fprintf(w, "%s(%s)", cb, body)
	}))
}

func newsItem(date, title, url string) map[string]any {
	return map[string]any{
		"date": date, "title": title, "content": "摘要",
		"mediaName": "第一财经", "url": url,
	}
}

// 报文是 JSONP（cb({...})），不剥壳直接按 JSON 解会失败。
// 这是这个接口与行情/K 线最大的形态差别，必须钉住。
func TestEastmoneyNewsUnwrapsJSONP(t *testing.T) {
	today := time.Now().In(beijing).Format("2006-01-02")
	srv := newsServer(t, []map[string]any{
		newsItem(today+" 10:00:00", "茅台发布中报", "http://finance.eastmoney.com/a/1.html"),
	}, nil)
	defer srv.Close()

	p := NewEastmoneyProvider(srv.Client(), 0, 1)
	p.searchHost = srv.URL
	code, _ := shared_vo.NewStockCode("600519", shared_vo.MarketCN)

	items, err := p.FetchNews(context.Background(), code, shared_vo.LastNDays(7), 10)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应当拿到 1 条，实际 %d", len(items))
	}
	if items[0].Title != "茅台发布中报" || items[0].Source != "第一财经" {
		t.Fatalf("映射错误: %+v", items[0])
	}
	// 发布时间是北京时间且报文里不带时区，按 UTC 解会整体偏 8 小时。
	if got := items[0].PublishedAt.In(beijing).Format("2006-01-02 15:04"); got != today+" 10:00" {
		t.Fatalf("发布时间应为北京时间 %s 10:00，实际 %s", today, got)
	}
}

// 高亮标签必须在请求里就关掉，而不是拿回来再洗。
// 默认值 <em></em> 会被直接插进标题与正文，落库之后前端和 LLM 拿到的就是带标签的文本。
func TestEastmoneyNewsDisablesHighlightTags(t *testing.T) {
	var got eastmoneySearchRequest
	srv := newsServer(t, nil, func(req eastmoneySearchRequest) { got = req })
	defer srv.Close()

	p := NewEastmoneyProvider(srv.Client(), 0, 1)
	p.searchHost = srv.URL
	code, _ := shared_vo.NewStockCode("600519", shared_vo.MarketCN)

	if _, err := p.FetchNews(context.Background(), code, shared_vo.LastNDays(7), 10); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	scope := got.Param[eastmoneySearchNewsType]
	if scope.PreTag != "" || scope.PostTag != "" {
		t.Fatalf("高亮标签必须置空，实际 pre=%q post=%q", scope.PreTag, scope.PostTag)
	}
	if scope.Sort != "time" {
		t.Fatalf("应当按时间倒序，实际 %q——默认的相关度排序会把几个月前的旧文顶到前面", scope.Sort)
	}
	if got.Keyword != "600519" {
		t.Fatalf("关键词应为六位代码，实际 %q", got.Keyword)
	}
}

// 接口没有日期过滤参数，窗口只能在本地裁。
// 不裁的话一只冷门票会把几个月前的旧闻带进当天的分析上下文。
func TestEastmoneyNewsFiltersByDateRange(t *testing.T) {
	now := time.Now().In(beijing)
	srv := newsServer(t, []map[string]any{
		newsItem(now.Format("2006-01-02")+" 10:00:00", "窗口内", "http://a/1.html"),
		newsItem(now.AddDate(0, 0, -60).Format("2006-01-02")+" 10:00:00", "窗口外", "http://a/2.html"),
	}, nil)
	defer srv.Close()

	p := NewEastmoneyProvider(srv.Client(), 0, 1)
	p.searchHost = srv.URL
	code, _ := shared_vo.NewStockCode("600519", shared_vo.MarketCN)

	items, err := p.FetchNews(context.Background(), code, shared_vo.LastNDays(7), 10)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(items) != 1 || items[0].Title != "窗口内" {
		t.Fatalf("回看窗口外的条目应当被裁掉，实际 %+v", items)
	}
}

// URL 是资讯的去重依据，缺了它落库会 upsert 出一条 url="" 的黑洞文档，
// 把所有无 URL 的资讯合并成一条。
func TestEastmoneyNewsDropsItemsWithoutNaturalKey(t *testing.T) {
	today := time.Now().In(beijing).Format("2006-01-02")
	srv := newsServer(t, []map[string]any{
		newsItem(today+" 10:00:00", "好的", "http://a/1.html"),
		newsItem(today+" 10:00:00", "没有链接", ""),
		newsItem(today+" 10:00:00", "", "http://a/3.html"),
		newsItem("日期是坏的", "坏日期", "http://a/4.html"),
	}, nil)
	defer srv.Close()

	p := NewEastmoneyProvider(srv.Client(), 0, 1)
	p.searchHost = srv.URL
	code, _ := shared_vo.NewStockCode("600519", shared_vo.MarketCN)

	items, err := p.FetchNews(context.Background(), code, shared_vo.LastNDays(7), 10)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(items) != 1 || items[0].Title != "好的" {
		t.Fatalf("四条里只有一条合法，实际 %+v", items)
	}
}

// 这个搜索库按中文关键词检索，港股美股代码搜不出对应资讯。
// 如实声明不支持，让降级链继续往下找，而不是返回一堆不相干的结果。
func TestEastmoneyNewsOnlyCoversCN(t *testing.T) {
	p := NewEastmoneyProvider(&http.Client{}, 0, 1)
	for _, market := range []shared_vo.Market{shared_vo.MarketHK, shared_vo.MarketUS} {
		code := shared_vo.StockCode{Symbol: "00700", Market: market}
		if _, err := p.FetchNews(context.Background(), code, shared_vo.LastNDays(7), 10); err == nil {
			t.Fatalf("市场 %s 应当返回不可用", market)
		}
	}
}
