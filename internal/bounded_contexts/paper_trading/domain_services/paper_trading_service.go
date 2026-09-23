package domain_services

import (
	"context"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/paper_trading/value_objects"
	"github.com/wt5858/trading-agents-go/internal/domain_kernel/domain_event"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/pkg/idx"
)

const (
	// maxAccountsPerUser 限制单用户的模拟账户数。模拟盘是学习工具，
	// 几个并行策略足够了；没有上限的话，一次脚本调用就能给同一个用户开出上万个账户。
	//
	// 这里的检查只负责给用户一句人话。**真正的上限在数据库里**：paper_accounts
	// 的 ck_paper_accounts_seq 封住槽位总数，uk_paper_accounts_user_seq 封住并发。
	// 两处必须一起改，理由见那个迁移脚本——只改这里的话，并发开户仍然能突破上限，
	// 只改那里的话，用户会收到一条「开户并发冲突」而不是「最多 10 个」。
	maxAccountsPerUser = 10

	// saveRetries 是乐观锁冲突后的重试次数。
	//
	// 冲突意味着「我读到的账户状态已经过期」，唯一正确的补救是重新加载、
	// 在最新状态上重新判定资金/持仓是否仍然足够、再提交。
	// 注意重试必须从**重新加载**开始：拿着旧聚合再 Save 一次，
	// 只会用过期的现金余额覆盖别人的写入，那正是乐观锁要拦的事情。
	//
	// 3 次是个务实的上限：同一账户上的并发下单是低频事件，
	// 连续冲突三次说明有异常的并发压力，这时候让调用方收到冲突错误
	// 比在服务端无限自旋更好。
	saveRetries = 3
)

// PaperTradingService 是模拟交易上下文的用例编排者。
//
// 再强调一次本上下文的定位：这里没有真实资金、没有券商通道、没有撮合。
// 一切输出都是模拟结果，不构成任何投资建议。
type PaperTradingService struct {
	accountRepo *repositories.PaperAccountRepository
	quotes      QuoteReader
	publisher   domain_event.Publisher
}

func NewPaperTradingService(
	accountRepo *repositories.PaperAccountRepository,
	quotes QuoteReader,
	publisher domain_event.Publisher,
) *PaperTradingService {
	if publisher == nil {
		publisher = domain_event.NoopPublisher{}
	}
	return &PaperTradingService{accountRepo: accountRepo, quotes: quotes, publisher: publisher}
}

// ---------------------------------------------------------------------------
// 开户 / 重置
// ---------------------------------------------------------------------------

// OpenAccountInput 的金额是 string。
// 接口层如果用 float64 绑定 JSON 数字，精度在进入领域层之前就丢了，
// 后面再怎么用 decimal 都补不回来，所以边界上必须是字符串。
type OpenAccountInput struct {
	Name        string
	InitialCash string
}

func (s *PaperTradingService) OpenAccount(ctx context.Context, op Operator, in OpenAccountInput) (*entities.PaperAccount, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	initialCash, err := value_objects.ParseMoney(in.InitialCash, "初始资金")
	if err != nil {
		return nil, err
	}

	n, err := s.accountRepo.CountByUser(ctx, op.UserID)
	if err != nil {
		return nil, err
	}
	if n >= maxAccountsPerUser {
		return nil, custom_errors.QuotaExceeded("每个用户最多开立 %d 个模拟账户", maxAccountsPerUser)
	}

	account, err := entities.OpenPaperAccount(
		idx.Prefixed("pacct"), op.UserID, strings.TrimSpace(in.Name), initialCash)
	if err != nil {
		return nil, err
	}
	if err := s.accountRepo.Create(ctx, account); err != nil {
		return nil, err
	}
	s.publish(ctx, account)
	return account, nil
}

func (s *PaperTradingService) ListAccounts(ctx context.Context, op Operator) ([]*entities.PaperAccount, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	return s.accountRepo.FindByUser(ctx, op.UserID)
}

func (s *PaperTradingService) GetAccount(ctx context.Context, op Operator, accountID string) (*entities.PaperAccount, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	return s.loadOwned(ctx, op, accountID)
}

// ResetAccount 把账户恢复到开户状态（成交历史保留，理由见 entities.Reset）。
func (s *PaperTradingService) ResetAccount(ctx context.Context, op Operator, accountID string) (*entities.PaperAccount, error) {
	if err := requireLogin(op); err != nil {
		return nil, err
	}
	return s.mutate(ctx, op, accountID, func(a *entities.PaperAccount) error {
		return a.Reset()
	})
}

// ---------------------------------------------------------------------------
// 下单
// ---------------------------------------------------------------------------

type PlaceOrderInput struct {
	AccountID string
	Code      string
	Market    string
	Side      string
	Quantity  string
	// Price 留空表示按最新行情成交（市价单的模拟）。
	Price string
	Fee   string
}

// PlaceOrder 下一笔模拟委托，立即成交。
//
// # 这里没有「先查够不够、再下单」
//
// 资金/持仓是否足够的判定完全在 entities.Buy / Sell 内部，和状态变更是同一件事。
// 本层不做任何前置的 CanBuy 检查：那会是 TOCTOU（两个并发请求都查到「够」），
// 而且会把不变式复制成两份，早晚分叉。
//
// # 乐观锁冲突时怎么办
//
// 重新加载账户、在最新状态上重放这次下单、再提交。重放会重新走一遍
// entities 的校验，所以「重试时资金已经不够了」会得到一个正确的拒绝，
// 而不是一次基于过期余额的错误成交。
func (s *PaperTradingService) PlaceOrder(ctx context.Context, op Operator, in PlaceOrderInput) (value_objects.TradeRecord, error) {
	if err := requireLogin(op); err != nil {
		return value_objects.TradeRecord{}, err
	}

	side, err := value_objects.NewOrderSide(in.Side)
	if err != nil {
		return value_objects.TradeRecord{}, err
	}
	code, err := shared_vo.NewStockCode(in.Code, shared_vo.Market(strings.ToUpper(strings.TrimSpace(in.Market))))
	if err != nil {
		return value_objects.TradeRecord{}, err
	}
	quantity, err := value_objects.ParseQuantity(in.Quantity, "委托数量")
	if err != nil {
		return value_objects.TradeRecord{}, err
	}
	fee := decimal.Zero
	if strings.TrimSpace(in.Fee) != "" {
		if fee, err = value_objects.ParseMoney(in.Fee, "手续费"); err != nil {
			return value_objects.TradeRecord{}, err
		}
	}

	// 价格在进入重试循环之前就定下来：如果每次重试都重新取一次行情，
	// 同一次下单在冲突重试后可能以另一个价格成交，用户看到的成交价
	// 会和他按下按钮时看到的那个对不上。
	price, err := s.resolvePrice(ctx, code, in.Price)
	if err != nil {
		return value_objects.TradeRecord{}, err
	}

	var trade *entities.Trade
	_, err = s.mutate(ctx, op, in.AccountID, func(a *entities.PaperAccount) error {
		var mErr error
		if side.IsBuy() {
			trade, mErr = a.Buy(code, quantity, price, fee)
		} else {
			trade, mErr = a.Sell(code, quantity, price, fee)
		}
		return mErr
	})
	if err != nil {
		return value_objects.TradeRecord{}, err
	}
	// ToRecord 搬运的是聚合里固化好的 Amount，不是 quantity × price 重算的结果。
	return trade.ToRecord(), nil
}

// resolvePrice 决定成交价：显式指定优先，否则取最新行情。
//
// 取行情走的是批量接口（这里只有一个标的），因为端口上根本没有单标的方法——
// 见 QuoteReader 的说明。
func (s *PaperTradingService) resolvePrice(ctx context.Context, code shared_vo.StockCode, raw string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) != "" {
		return value_objects.ParseMoney(raw, "委托价格")
	}
	if s.quotes == nil {
		return decimal.Zero, custom_errors.Invalid("未指定委托价格，且行情源不可用")
	}
	quotes, err := s.quotes.LatestQuotes(ctx, []shared_vo.StockCode{code})
	if err != nil {
		return decimal.Zero, err
	}
	now := time.Now()
	for _, q := range quotes {
		if q.Code.Symbol != code.Symbol || !q.Usable() {
			continue
		}
		// 新鲜度只在下单路径上查，估值路径不查（那边靠 HasQuote 把事实透出去）。
		// 一条停更几个月的报价仍然是 Usable 的——它只是很旧。直接拿来撮合，
		// 用户会以为自己按今天的价格成交，实际成交在几个月前的收盘价上，
		// 而且毫无提示，只会沉淀成一笔成本离谱的持仓。
		if !q.Fresh(now, value_objects.MaxQuoteAge) {
			return decimal.Zero, custom_errors.Unavailable(
				"%s 的最新行情停留在 %s，已超出可撮合范围，请显式指定委托价格",
				code.FullSymbol(), q.AsOf.Format("2006-01-02"))
		}
		return q.Price, nil
	}
	// 下单路径上拿不到行情必须直接拒绝，不能像估值那样回退到成本价：
	// 用一个猜出来的价格成交，会污染此后所有的盈亏计算。
	return decimal.Zero, custom_errors.Unavailable("暂无 %s 的最新行情，请显式指定委托价格", code.FullSymbol())
}

// ---------------------------------------------------------------------------
// 组合估值 / 成交历史
// ---------------------------------------------------------------------------

// GetPortfolio 给账户组合估值。
//
// # 一次批量行情调用，不是每个持仓一次
//
// 全部持仓的代码先收集成一个切片，一次 LatestQuotes 拿回来，再在内存里按
// symbol 索引。30 个持仓是 1 次调用，不是 30 次；也因此这里不需要任何并发原语，
// 本来就只有一次 I/O。
//
// # 这里出现的乘除是规则允许的那个例外
//
// 市值与浮动盈亏依赖实时报价，不存在「当时的事实」可落库，所以必须读时计算。
// 但算式里的平均成本用的是**落库的存量值**，不是现场反推的。详见 value_objects/portfolio.go。
func (s *PaperTradingService) GetPortfolio(ctx context.Context, op Operator, accountID string) (value_objects.PortfolioSummary, error) {
	if err := requireLogin(op); err != nil {
		return value_objects.PortfolioSummary{}, err
	}
	account, err := s.loadOwned(ctx, op, accountID)
	if err != nil {
		return value_objects.PortfolioSummary{}, err
	}

	codes := make([]shared_vo.StockCode, 0, len(account.Positions))
	for _, p := range account.Positions {
		codes = append(codes, p.Code)
	}

	quoteBySymbol := make(map[string]value_objects.LiveQuote, len(codes))
	if len(codes) > 0 && s.quotes != nil {
		quotes, qErr := s.quotes.LatestQuotes(ctx, codes)
		if qErr != nil {
			// 行情不可用不该让「我持有什么、现金还剩多少」这类事实也查不出来。
			// 降级为按成本价估值，并通过 PositionValuation.HasQuote 把这个事实透出去。
			quotes = nil
		}
		for _, q := range quotes {
			quoteBySymbol[q.Code.Symbol] = q
		}
	}

	valuations := make([]value_objects.PositionValuation, 0, len(account.Positions))
	for _, p := range account.Positions {
		valuations = append(valuations, value_objects.ValuePosition(
			p.Code, p.Quantity, p.AvgCost, p.CostBasis, quoteBySymbol[p.Code.Symbol]))
	}

	return value_objects.NewPortfolioSummary(
		account.ID, account.Cash, account.InitialCash, account.RealizedPnL, valuations), nil
}

// TradeHistory 分页返回成交历史（值对象）。
func (s *PaperTradingService) TradeHistory(
	ctx context.Context, op Operator, accountID string, page shared_vo.Page,
) ([]value_objects.TradeRecord, int64, error) {
	if err := requireLogin(op); err != nil {
		return nil, 0, err
	}
	// 先确认归属再查历史：直接按 accountID 查成交表会让任何人
	// 用一个猜到的 ID 读到别人的交易记录。
	if _, err := s.loadOwned(ctx, op, accountID); err != nil {
		return nil, 0, err
	}
	return s.accountRepo.ListTrades(ctx, accountID, page)
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// loadOwned 加载账户并做归属判定。越权返回 NotFound，理由见 requireOwnership。
func (s *PaperTradingService) loadOwned(ctx context.Context, op Operator, accountID string) (*entities.PaperAccount, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, custom_errors.Invalid("模拟账户 ID 不能为空")
	}
	account, err := s.accountRepo.FindByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if err := requireOwnership(op, account); err != nil {
		return nil, err
	}
	return account, nil
}

// mutate 是「加载 - 变更 - 保存」这条路径的唯一实现，带乐观锁重试。
//
// 把它抽出来不只是去重：它保证了每一条写路径都走同一套重试语义——
// 重试必然从**重新加载**开始，而不是拿着旧聚合再提交一次。
// 后者会用过期的现金余额覆盖别人刚写进去的结果，正是乐观锁要防的事。
//
// 变更闭包里抛出的业务错误（资金不足、持仓不足）不会触发重试：
// 那不是并发问题，重试一百次答案也一样。
func (s *PaperTradingService) mutate(
	ctx context.Context,
	op Operator,
	accountID string,
	apply func(*entities.PaperAccount) error,
) (*entities.PaperAccount, error) {
	var lastErr error
	for attempt := 0; attempt < saveRetries; attempt++ {
		account, err := s.loadOwned(ctx, op, accountID)
		if err != nil {
			return nil, err
		}
		if err := apply(account); err != nil {
			return nil, err
		}
		if err := s.accountRepo.Save(ctx, account); err != nil {
			if repositories.IsConflict(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		// 事件在提交成功之后才发布：先发后提交的话，一次回滚会让下游
		// 收到一笔根本没有发生过的成交。
		s.publish(ctx, account)
		return account, nil
	}
	return nil, lastErr
}

func (s *PaperTradingService) publish(ctx context.Context, a *entities.PaperAccount) {
	if evts := a.GetAllPendingEvents(); len(evts) > 0 {
		_ = s.publisher.Publish(ctx, evts...)
	}
}
