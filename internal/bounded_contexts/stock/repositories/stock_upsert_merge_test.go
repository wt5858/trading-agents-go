package repositories

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

// TestBulkUpsertKeepsExistingProfile_LiveMySQL 验证多源写同一只票时的列级合并。
//
// 必须打真库。合并语义整个活在 ON DUPLICATE KEY UPDATE 的表达式里，由 MySQL 求值；
// 任何假 DB 都只会按我们自己以为的规则回放，正好把 bug 一起复刻进去。
// （另有一条 DryRun 测试钉住 SQL 文本，那条证明的是「表达式进了 SQL」，
// 不是「MySQL 按我们想的那样算」。两条缺一不可。）
//
// 默认跳过。启用方式：
//
//	TA_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:33061)/trading_agents?parseTime=true&loc=Local' \
//	  go test ./internal/bounded_contexts/stock/repositories/ -run LiveMySQL -v
func TestBulkUpsertKeepsExistingProfile_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的合并测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&dtos.StockDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	repo := NewStockRepository(db)
	ctx := context.Background()
	// 用一个不可能和真实数据撞车的港股代码。
	code, err := shared_vo.NewStockCode("99001", shared_vo.MarketHK)
	if err != nil {
		t.Fatalf("构造代码失败: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM stocks WHERE market = ? AND symbol = ?", string(code.Market), code.Symbol)
	})
	db.Exec("DELETE FROM stocks WHERE market = ? AND symbol = ?", string(code.Market), code.Symbol)

	listed := time.Date(2010, 1, 4, 0, 0, 0, 0, time.UTC)

	// 第一个源（类比 Tushare stock_basic）：有行业/地区/上市日期，没有市值。
	rich, err := entities.List(entities.ListParams{
		Code: code, Name: "合并测试", Industry: "白酒", Area: "深圳",
		ListDate: &listed, Source: "srcA",
	})
	if err != nil {
		t.Fatalf("构造股票失败: %v", err)
	}
	if err := repo.BulkUpsert(ctx, []*entities.Stock{rich}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	// 第二个源（类比东财 clist）：有名称与市值，行业/地区/上市日期全缺。
	// 旧的无条件覆盖会把上面那三列抹成空，而且同步照样报「成功」。
	lean, err := entities.List(entities.ListParams{
		Code: code, Name: "合并测试", Source: "srcB",
		TotalMV: decimal.NewFromInt(2214115013896),
		CircMV:  decimal.NewFromInt(1666781243896),
	})
	if err != nil {
		t.Fatalf("构造股票失败: %v", err)
	}
	if err := repo.BulkUpsert(ctx, []*entities.Stock{lean}); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}

	got, err := repo.FindByCode(ctx, code)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}

	// 缺列的源不得抹掉已有信息。
	if got.Industry != "白酒" {
		t.Errorf("industry = %q，第二个源没提供这一列，不该把它抹掉", got.Industry)
	}
	if got.Area != "深圳" {
		t.Errorf("area = %q，同上", got.Area)
	}
	if got.ListDate == nil {
		t.Error("list_date 被抹成 NULL 了，COALESCE 没生效")
	}
	// 有值的列照常更新。
	if got.TotalMV.String() != "2214115013896" {
		t.Errorf("total_mv = %s，第二个源提供了市值，应当写进去", got.TotalMV)
	}
	if got.Source != "srcB" {
		t.Errorf("source = %q，必须反映最后写入者，否则跨源对账无从判断", got.Source)
	}

	// 反向：市值缺失的源不得把已有市值清零。
	if err := repo.BulkUpsert(ctx, []*entities.Stock{rich}); err != nil {
		t.Fatalf("三次写入失败: %v", err)
	}
	got, err = repo.FindByCode(ctx, code)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.TotalMV.String() != "2214115013896" {
		t.Errorf("total_mv = %s，不提供市值的源不该把它清零（0 表示「本源没有」而非「市值为零」）", got.TotalMV)
	}
	if got.Industry != "白酒" {
		t.Errorf("industry = %q，应当仍然存在", got.Industry)
	}
}

// TestBulkUpsertDelistIsOneWay_LiveMySQL 退市只能单向翻转。
//
// 只返回在市股票的数据源（东财 clist 就是）给出的 delisted 恒为 false，
// 而 false 正好是零值——「没报告这只票」和「确认它在市」分不开。
// 无条件覆盖会让别的源标过退市的票被它悄悄复活，绕过 Relist() 该发的领域事件。
func TestBulkUpsertDelistIsOneWay_LiveMySQL(t *testing.T) {
	dsn := os.Getenv("TA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TA_TEST_MYSQL_DSN，跳过需要真库的退市测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&dtos.StockDto{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	repo := NewStockRepository(db)
	ctx := context.Background()
	code, err := shared_vo.NewStockCode("99002", shared_vo.MarketHK)
	if err != nil {
		t.Fatalf("构造代码失败: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM stocks WHERE market = ? AND symbol = ?", string(code.Market), code.Symbol)
	})
	db.Exec("DELETE FROM stocks WHERE market = ? AND symbol = ?", string(code.Market), code.Symbol)

	delisted, _ := entities.List(entities.ListParams{
		Code: code, Name: "退市测试", Delisted: true, Source: "srcA",
	})
	if err := repo.BulkUpsert(ctx, []*entities.Stock{delisted}); err != nil {
		t.Fatalf("写入退市状态失败: %v", err)
	}

	alive, _ := entities.List(entities.ListParams{
		Code: code, Name: "退市测试", Delisted: false, Source: "srcB",
	})
	if err := repo.BulkUpsert(ctx, []*entities.Stock{alive}); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}

	got, err := repo.FindByCode(ctx, code)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !got.Delisted {
		t.Error("退市状态被同步悄悄撤销了；撤销退市必须走 Relist()，那里才会发领域事件")
	}
}
