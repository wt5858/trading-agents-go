-- +goose Up
-- +goose StatementBegin
-- 模拟账户持仓。
--
-- 这张表承载的是 PaperAccount 的**子实体**，不是一个独立聚合。
-- 它只被 PaperAccountRepository 读写，而且写入只发生在 Save 的那个事务里——
-- 「买入扣的现金」和「建仓增加的成本」必须同生共死，分两次提交的话
-- 中间崩一次，账本就永久对不平了。
--
-- 也因此这张表没有自己的仓储入口：外界拿不到直接改持仓的方法。
CREATE TABLE `paper_positions`
(
    `account_id` varchar(48)    NOT NULL COMMENT '所属模拟账户',
    `symbol`     varchar(16)    NOT NULL COMMENT '规范化股票代码，跨聚合引用只存代码不存实体',
    `market`     varchar(8)     NOT NULL DEFAULT '' COMMENT 'CN / HK / US',
    `symbol_raw` varchar(24)    NOT NULL DEFAULT '' COMMENT '用户原始输入写法，仅供排查',

    `quantity`   decimal(20, 8) NOT NULL COMMENT '持仓数量。8 位小数为港股碎股等场景留余量',

    -- avg_cost 与 cost_basis 都是**买入时算一次并固化**的乘除派生量。
    -- 卖出算已实现盈亏时读的是 avg_cost 这个存量值，绝不用 cost_basis / quantity
    -- 现场反推：取整口径的细微差异会让同一笔卖出在不同代码路径上算出不同的盈亏。
    --
    -- 两列看似冗余（cost_basis ≈ avg_cost × quantity），但都必须存：
    -- avg_cost 按 4 位小数取整后反乘回去，未必等于当初真正从现金里划走的钱，
    -- 而 cost_basis 才是那个事实，组合汇总里的持仓成本合计读的是它。
    `avg_cost`   decimal(20, 4) NOT NULL COMMENT '移动加权平均成本（含买入手续费），买入时算好，读路径不重算',
    `cost_basis` decimal(20, 4) NOT NULL COMMENT '当前持仓占用的总成本，落库的派生量',

    -- 这里**没有** market_value / unrealized_pnl 列，这是刻意的。
    -- 市值与浮动盈亏依赖实时报价，每一秒都在变，不存在「当时的事实」可落库；
    -- 存下来的任何一个浮动盈亏下一秒就是错的。它们由读路径按
    -- (实时价格, 本表落库的 avg_cost) 计算，是「派生量必须落库」那条规则唯一的例外。
    -- 若将来需要浮动盈亏的历史曲线，正确做法是另起一张按日快照表，而不是在这里加列。

    `opened_at`  datetime(3)    NOT NULL COMMENT '首次建仓时间',
    `updated_at` datetime(3)    NOT NULL,
    -- 复合主键即「一个账户一只票只有一行持仓」这条不变式的执行者，
    -- 比在应用层写查重可靠得多，也给 Save 的 upsert 提供了天然的冲突目标。
    -- 清仓时该行被直接删除，不留 quantity = 0 的空行：空行会污染持仓数统计。
    PRIMARY KEY (`account_id`, `symbol`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci COMMENT ='模拟账户持仓，非真实持仓';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `paper_positions`;
-- +goose StatementEnd
