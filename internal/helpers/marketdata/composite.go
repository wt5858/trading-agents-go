package marketdata

import (
	"context"
	"errors"
	"strings"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/domain_services"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// Composite 是数据源的降级编排层，本身也实现 domain_services.DataProvider，
// 因此对上层完全透明：应用层只注入一个 DataProvider，不感知背后有几个源。
//
// 策略是「按注册顺序取第一个成功结果」：
//   - 顺序即优先级，把数据质量高的源放前面，Mock 放最后兜底；
//   - 只要有一个源成功就立刻返回，不做多源比对（那是另一层的职责）；
//   - 全部失败时把所有源的错误聚合抛出，否则线上只看到「取数失败」
//     完全无法判断是限流、限权还是网络问题。
type Composite struct {
	providers []domain_services.DataProvider
}

var _ domain_services.DataProvider = (*Composite)(nil)

// NewComposite 按优先级从高到低传入数据源。nil 元素会被忽略，
// 便于调用方写 NewComposite(tushare, maybeNilFinnhub, mock) 而不必先判空。
func NewComposite(providers ...domain_services.DataProvider) *Composite {
	list := make([]domain_services.DataProvider, 0, len(providers))
	for _, p := range providers {
		if p != nil {
			list = append(list, p)
		}
	}
	return &Composite{providers: list}
}

func (c *Composite) Name() string { return "composite" }

// Supports 只要有任一成员支持该市场即视为支持。
func (c *Composite) Supports(market shared_vo.Market) bool {
	for _, p := range c.providers {
		if p.Supports(market) {
			return true
		}
	}
	return false
}

// Providers 返回当前注册的数据源，供健康检查/管理接口展示降级链。
func (c *Composite) Providers() []domain_services.DataProvider {
	out := make([]domain_services.DataProvider, len(c.providers))
	copy(out, c.providers)
	return out
}

func (c *Composite) FetchStockList(ctx context.Context, market shared_vo.Market) ([]*entities.Stock, error) {
	return firstSuccess(ctx, c, market, "FetchStockList", func(p domain_services.DataProvider) ([]*entities.Stock, error) {
		return p.FetchStockList(ctx, market)
	})
}

func (c *Composite) FetchQuote(ctx context.Context, code shared_vo.StockCode) (*value_objects.Quote, error) {
	return firstSuccess(ctx, c, code.Market, "FetchQuote", func(p domain_services.DataProvider) (*value_objects.Quote, error) {
		return p.FetchQuote(ctx, code)
	})
}

func (c *Composite) FetchQuotes(ctx context.Context, market shared_vo.Market) ([]value_objects.Quote, error) {
	return firstSuccess(ctx, c, market, "FetchQuotes", func(p domain_services.DataProvider) ([]value_objects.Quote, error) {
		return p.FetchQuotes(ctx, market)
	})
}

func (c *Composite) FetchKlines(ctx context.Context, code shared_vo.StockCode, period value_objects.Period, r shared_vo.DateRange) ([]value_objects.Kline, error) {
	return firstSuccess(ctx, c, code.Market, "FetchKlines", func(p domain_services.DataProvider) ([]value_objects.Kline, error) {
		return p.FetchKlines(ctx, code, period, r)
	})
}

func (c *Composite) FetchKlinesByDate(
	ctx context.Context, market shared_vo.Market, period value_objects.Period, date shared_vo.TradeDate,
) ([]value_objects.Kline, error) {
	return firstSuccess(ctx, c, market, "FetchKlinesByDate",
		func(p domain_services.DataProvider) ([]value_objects.Kline, error) {
			return p.FetchKlinesByDate(ctx, market, period, date)
		})
}

func (c *Composite) FetchFinancials(ctx context.Context, code shared_vo.StockCode, limit int) ([]value_objects.Financial, error) {
	return firstSuccess(ctx, c, code.Market, "FetchFinancials", func(p domain_services.DataProvider) ([]value_objects.Financial, error) {
		return p.FetchFinancials(ctx, code, limit)
	})
}

func (c *Composite) FetchNews(ctx context.Context, code shared_vo.StockCode, r shared_vo.DateRange, limit int) ([]value_objects.News, error) {
	return firstSuccess(ctx, c, code.Market, "FetchNews", func(p domain_services.DataProvider) ([]value_objects.News, error) {
		return p.FetchNews(ctx, code, r, limit)
	})
}

// firstSuccess 是降级循环的唯一实现。
//
// 写成泛型自由函数而不是方法，是因为 Go 不允许方法带类型参数，
// 而五个 Fetch 方法的返回类型各不相同，否则只能靠 any + 断言，得不偿失。
func firstSuccess[T any](
	ctx context.Context,
	c *Composite,
	market shared_vo.Market,
	op string,
	call func(domain_services.DataProvider) (T, error),
) (T, error) {
	var zero T

	if !market.Valid() {
		return zero, custom_errors.Invalid("非法市场: %s", market)
	}

	var (
		errs        []error
		tried       []string
		unsupported int
		skipped     = true
	)
	for _, p := range c.providers {
		// 每轮都检查取消：前一个源超时后，没必要再把剩下的源挨个跑一遍。
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if !p.Supports(market) {
			continue
		}
		skipped = false

		result, err := call(p)
		if err == nil {
			tried = append(tried, p.Name())
			return result, nil
		}
		// 能力缺席既不计入 errs 也不计入 tried：它没有付出任何网络往返，
		// 把它混进聚合错误里，「东财被掐了」和「tushare 本来就没这个接口」
		// 读起来会一模一样，而这两件事的处置方式正好相反。
		if errors.Is(err, domain_services.ErrBatchUnsupported) {
			unsupported++
			continue
		}
		tried = append(tried, p.Name())
		// 把源名带进错误里，聚合后才能看出是哪一环挂的。
		errs = append(errs, &providerError{provider: p.Name(), op: op, err: err})
	}

	if skipped && len(errs) == 0 && unsupported == 0 {
		return zero, custom_errors.Unavailable("没有数据源支持市场 %s（%s）", market, op)
	}
	// 只有「全员能力缺席、一次真实失败都没有」才把哨兵透给上层。
	//
	// 混合场景（东财挂了 + tushare 没这个接口）必须走下面的聚合错误，绝不能带哨兵：
	// 上层看到哨兵就会回退到逐标的路径，那意味着掉头对刚刚掐掉我们连接的源
	// 再打几千次请求——本来只是一次失败，会变成一次封禁。
	if len(errs) == 0 && unsupported > 0 {
		return zero, custom_errors.Unavailable(
			"没有数据源提供 %s 的批量端点（市场 %s）", op, market,
		).Wrap(domain_services.ErrBatchUnsupported)
	}
	// errors.Join 保留每个子错误，上层仍可用 errors.Is/As 判断具体类型
	// （例如判断是否全部是 QuotaExceeded 以决定要不要重试）。
	return zero, custom_errors.Unavailable(
		"所有数据源均无法完成 %s（已尝试: %s）", op, strings.Join(tried, ", "),
	).Wrap(errors.Join(errs...))
}

// providerError 给底层错误加上来源标注，只是包装，不改变错误码。
type providerError struct {
	provider string
	op       string
	err      error
}

func (e *providerError) Error() string {
	return "[" + e.provider + "." + e.op + "] " + e.err.Error()
}

func (e *providerError) Unwrap() error { return e.err }
