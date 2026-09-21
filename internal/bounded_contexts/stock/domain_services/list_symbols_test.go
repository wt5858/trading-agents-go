package domain_services

import (
	"context"
	"fmt"
	"os"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// TestListSymbols_PagesPastMaxPageSize_LiveMySQL 钉住逐标的同步的取数范围。
//
// 这里必须打真库，不能用桩：要证明的性质是「Page 值对象把每页条数收敛到
// maxPageSize 之后，翻页仍然能取完」——而收敛发生在 NewPage 里、生效在
// SQL 的 LIMIT/OFFSET 上。任何模拟仓储都会按我们自己以为的页大小返回数据，
// 正好把 bug 一起复刻进去，测了等于没测。
//
// 默认跳过。启用方式：
//
//	TA_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:33061)/ta_test?parseTime=true&loc=Local' \
//	  go test ./internal/bounded_contexts/stock/...
func TestListSymbols_PagesPastMaxPageSize_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的翻页测试")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	// 建表，让这个测试也能指向一个空库跑。指向已迁移过的库时是 no-op。
	if err := db.AutoMigrate(&dtos.StockDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 条数取 maxPageSize 的两倍多一点：少于一页测不出翻页，
	// 正好整数页又会让「不满一页即到底」这条终止条件失去意义。
	const seeded = 523
	// 用 HK 市场，代码段 4-5 位数字，和本地开发库里的 A 股样例数据不会撞。
	const market = shared_vo.MarketHK

	stockRepo := repositories.NewStockRepository(db)
	ctx := context.Background()

	list := make([]*entities.Stock, 0, seeded)
	for i := 0; i < seeded; i++ {
		code, err := shared_vo.NewStockCode(fmt.Sprintf("%05d", 10000+i), market)
		if err != nil {
			t.Fatalf("构造股票代码失败: %v", err)
		}
		s, err := entities.List(entities.ListParams{
			Code: code, Name: "翻页测试", Source: "test",
		})
		if err != nil {
			t.Fatalf("构造股票失败: %v", err)
		}
		list = append(list, s)
	}
	if err := stockRepo.BulkUpsert(ctx, list); err != nil {
		t.Fatalf("写入测试标的失败: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM stocks WHERE market = ? AND source = 'test'", string(market))
	})

	svc := NewSyncService(stockRepo, nil, nil, nil, nil, nil, SyncConfig{})
	codes, err := svc.listSymbols(ctx, market)
	if err != nil {
		t.Fatalf("列举标的失败: %v", err)
	}

	if len(codes) != seeded {
		t.Fatalf("只列举到 %d 只标的，期望 %d——翻页在 maxPageSize 处提前收工，"+
			"quotes/klines/financials/news 都会漏同步", len(codes), seeded)
	}
	// 去重顺带钉住「翻页没有重复取同一页」：条数对但内容重复同样是错的。
	seen := make(map[string]struct{}, len(codes))
	for _, c := range codes {
		if _, dup := seen[c.Symbol]; dup {
			t.Fatalf("标的 %s 被重复列举", c.Symbol)
		}
		seen[c.Symbol] = struct{}{}
	}
}
