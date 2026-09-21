package repositories

import (
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TestStockOnConflictRendersColumnMerge 钉住 upsert 的列级合并策略真的进了 SQL。
//
// 用 DryRun 渲染而不是打真库：这里要证明的是「GORM 的 MySQL driver 会把
// clause.Expr 原样写进 ON DUPLICATE KEY UPDATE，而不是当成占位参数」——
// 那是一条关于 driver 行为的断言，渲染出 SQL 文本就够了，连接数据库反而更慢更脆。
// 合并语义本身在真库上另有一条测试（见 TestBulkUpsertKeepsExistingProfile_LiveMySQL）。
//
// 这条测试的真正价值在于：合并策略一旦被人「顺手简化」回
// clause.AssignmentColumns(所有列)，东财同步就会把 Tushare 填的行业静默清空，
// 而那个后果要等到前端筛选框空掉才会有人发现。
func TestStockOnConflictRendersColumnMerge(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN: "unused:unused@tcp(127.0.0.1:1)/unused?parseTime=true",
		// 不连库就拿不到服务端版本号，跳过探测；DryRun 也不会真的执行。
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun: true,
		// sql.Open 本身是惰性的，真正会拨号的是 gorm.Open 的自动 ping，关掉它。
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatalf("构造 DryRun 会话失败: %v", err)
	}

	// 只渲染 ON CONFLICT 这一段，不走完整的 Create：Create 的回调链会去碰连接池，
	// 而这里要看的东西全在这个子句里。stmt.Build 会优先用 dialector 注册的
	// 子句构造器，所以渲染出来的就是 MySQL 方言的真实形态。
	stmt := &gorm.Statement{DB: db, Table: "stocks", Clauses: map[string]clause.Clause{}}
	stmt.AddClause(stockOnConflict())
	stmt.Build("ON CONFLICT")
	sql := stmt.SQL.String()
	if sql == "" {
		t.Fatal("没有渲染出 SQL")
	}

	// 每一条都对应一个具体的数据损坏场景，注释说明的是「不这么写会怎样」。
	wants := map[string]string{
		"IF(VALUES(`name`) = '', `name`, VALUES(`name`))":             "源没给名称时不该把已有名称抹成空串",
		"IF(VALUES(`industry`) = '', `industry`, VALUES(`industry`))": "东财列表没有行业，不能覆盖 Tushare 填好的行业",
		"IF(VALUES(`area`) = '', `area`, VALUES(`area`))":             "同上，地区",
		"COALESCE(VALUES(`list_date`), `list_date`)":                  "上市日期是可空 DATE，缺失形态是 NULL 不是空串",
		"IF(VALUES(`total_mv`) = 0, `total_mv`, VALUES(`total_mv`))":  "市值 0 表示本源不提供，不是市值为零",
		"IF(VALUES(`total_mv`) = 0, `circ_mv`, VALUES(`circ_mv`))":    "流通市值必须与总市值同闸门，否则会拼出跨源的一行并违反 circ_mv <= total_mv",
		"IF(VALUES(`delisted`) = 1, 1, `delisted`)":                   "退市只能单向翻转，否则只返在市股的源会绕过 Relist() 悄悄复活退市票",
		"`source`=VALUES(`source`)":                                   "source 必须永远反映最后写入者，否则跨源对账时看不出这行是谁给的",
	}
	for frag, why := range wants {
		if !strings.Contains(sql, frag) {
			t.Errorf("生成的 SQL 缺少 %q\n  原因：%s\n  实际 SQL: %s", frag, why, sql)
		}
	}

	// 反向断言：这几列绝不能出现无条件覆盖的形态。
	for _, bad := range []string{
		"`industry`=VALUES(`industry`)",
		"`area`=VALUES(`area`)",
		"`list_date`=VALUES(`list_date`)",
		"`delisted`=VALUES(`delisted`)",
	} {
		if strings.Contains(sql, bad) {
			t.Errorf("出现了无条件覆盖 %q，合并策略被绕过了\n  实际 SQL: %s", bad, sql)
		}
	}
}
