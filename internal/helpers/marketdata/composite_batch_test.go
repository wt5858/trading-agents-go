package marketdata

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// stubProvider 只实现本组用例关心的 FetchQuotes，其余方法给零值。
type stubProvider struct {
	name    string
	markets []shared_vo.Market
	quotes  []value_objects.Quote
	err     error
	calls   *[]string
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Supports(m shared_vo.Market) bool {
	for _, x := range s.markets {
		if x == m {
			return true
		}
	}
	return false
}

func (s *stubProvider) FetchQuotes(_ context.Context, _ shared_vo.Market) ([]value_objects.Quote, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, s.name)
	}
	return s.quotes, s.err
}

func (s *stubProvider) FetchStockList(context.Context, shared_vo.Market) ([]*entities.Stock, error) {
	return nil, nil
}

func (s *stubProvider) FetchQuote(context.Context, shared_vo.StockCode) (*value_objects.Quote, error) {
	return nil, nil
}

func (s *stubProvider) FetchKlines(context.Context, shared_vo.StockCode, value_objects.Period, shared_vo.DateRange) ([]value_objects.Kline, error) {
	return nil, nil
}

func (s *stubProvider) FetchKlinesByDate(context.Context, shared_vo.Market, value_objects.Period, shared_vo.TradeDate) ([]value_objects.Kline, error) {
	return nil, nil
}

func (s *stubProvider) FetchFinancials(context.Context, shared_vo.StockCode, int) ([]value_objects.Financial, error) {
	return nil, nil
}

func (s *stubProvider) FetchNews(context.Context, shared_vo.StockCode, shared_vo.DateRange, int) ([]value_objects.News, error) {
	return nil, nil
}

func unsupportedErr() error {
	return custom_errors.Unavailable("无批量端点").Wrap(domain_services.ErrBatchUnsupported)
}

var cnOnly = []shared_vo.Market{shared_vo.MarketCN}

// TestCompositeSkipsBatchUnsupportedAndContinues 能力缺席只意味着「换一条路走」，
// 降级链必须继续往下试，而不是停在第一个源上。
func TestCompositeSkipsBatchUnsupportedAndContinues(t *testing.T) {
	var calls []string
	want := []value_objects.Quote{{Source: "good"}}
	c := NewComposite(
		&stubProvider{name: "nobatch", markets: cnOnly, err: unsupportedErr(), calls: &calls},
		&stubProvider{name: "good", markets: cnOnly, quotes: want, calls: &calls},
	)

	got, err := c.FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err != nil {
		t.Fatalf("应当降级到第二个源: %v", err)
	}
	if len(got) != 1 || got[0].Source != "good" {
		t.Fatalf("拿到 %+v，期望第二个源的结果", got)
	}
	if len(calls) != 2 {
		t.Fatalf("调用序列 %v，两个源都该被试到", calls)
	}
}

// TestCompositeSurfacesSentinelWhenAllUnsupported 全员都没有批量端点时，
// 哨兵必须透出来，上层据此回退到逐标的路径。
func TestCompositeSurfacesSentinelWhenAllUnsupported(t *testing.T) {
	c := NewComposite(
		&stubProvider{name: "a", markets: cnOnly, err: unsupportedErr()},
		&stubProvider{name: "b", markets: cnOnly, err: unsupportedErr()},
	)

	_, err := c.FetchQuotes(context.Background(), shared_vo.MarketCN)
	if !errors.Is(err, domain_services.ErrBatchUnsupported) {
		t.Fatalf("期望透出 ErrBatchUnsupported，得到 %v", err)
	}
}

// TestCompositeHidesSentinelWhenAnyProviderActuallyFailed 这条是整个设计里最要紧的一条。
//
// 混合场景（一个源真的挂了 + 一个源本就没这接口）绝不能透出哨兵：
// 上层看到哨兵会回退到逐标的路径，也就是掉头对那个刚刚掐掉我们连接的源
// 再打几千次请求——本来只是一次失败，会变成一次封禁。
func TestCompositeHidesSentinelWhenAnyProviderActuallyFailed(t *testing.T) {
	c := NewComposite(
		&stubProvider{name: "broken", markets: cnOnly, err: custom_errors.Unavailable("连接被重置")},
		&stubProvider{name: "nobatch", markets: cnOnly, err: unsupportedErr()},
	)

	_, err := c.FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if errors.Is(err, domain_services.ErrBatchUnsupported) {
		t.Fatalf("有源真实失败时不能透出哨兵，否则上层会回退并对故障源发起几千次请求: %v", err)
	}
}

// TestCompositeDoesNotCountUnsupportedAsTried 能力缺席没有付出网络往返，
// 把它算进「已尝试」会让错误信息把「东财被掐了」和「tushare 没这接口」
// 说成同一件事，而这两者的处置方式相反。
func TestCompositeDoesNotCountUnsupportedAsTried(t *testing.T) {
	c := NewComposite(
		&stubProvider{name: "nobatch", markets: cnOnly, err: unsupportedErr()},
		&stubProvider{name: "broken", markets: cnOnly, err: custom_errors.Unavailable("连接被重置")},
	)

	_, err := c.FetchQuotes(context.Background(), shared_vo.MarketCN)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "broken") {
		t.Fatalf("错误信息应当点名真正失败的源，实际: %s", msg)
	}
	if strings.Contains(msg, "nobatch") {
		t.Fatalf("没有批量端点的源不该出现在「已尝试」里，实际: %s", msg)
	}
}
