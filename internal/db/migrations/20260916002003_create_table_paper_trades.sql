-- +goose Up
-- +goose StatementBegin
-- 模拟成交记录。**只追加**：仓储层没有 UPDATE 路径，也没有 DELETE 路径。
--
-- 成交历史是对账凭证。一旦允许某个业务动作改写或批量删除它，
-- 它作为「当时到底发生了什么」的唯一证据就失效了。
-- 账户重置也只清现金与持仓，不动这张表。
--
-- 它不是 PaperAccount 的子实体：历史无上限增长，做成常驻子实体会让
-- 「下一单」的加载成本随历史长度线性膨胀。读历史走独立的分页查询，
-- 返回值对象而不是实体。
CREATE TABLE `paper_trades`
(
    `id`           varchar(48)    NOT NULL COMMENT '成交 ID，由聚合在事务提交前生成（事件里要带上它）',
    `account_id`   varchar(48)    NOT NULL COMMENT '所属模拟账户',
    `symbol`       varchar(16)    NOT NULL,
    `market`       varchar(8)     NOT NULL DEFAULT '' COMMENT 'CN / HK / US',
    `symbol_raw`   varchar(24)    NOT NULL DEFAULT '' COMMENT '用户原始输入写法，仅供排查',
    `side`         varchar(8)     NOT NULL COMMENT 'buy / sell',

    `quantity`     decimal(20, 8) NOT NULL COMMENT '成交数量',
    `price`        decimal(20, 4) NOT NULL COMMENT '成交价格',

    -- amount 是成交那一刻算好的乘法派生量，也是真正从现金里划走/划入的那个数。
    --
    -- 它必须独立成列。把它当成「随时能用 quantity × price 算出来的冗余」而省掉，
    -- 等于让展示出来的成交额和 cash_after 的变动失去唯一事实来源：
    -- 只要取整口径、字段精度或后来改过的公式有任何一点偏差，两者就对不上，
    -- 而这种分叉无处收敛。读路径一律直接取本列，不重算。
    `amount`       decimal(20, 4) NOT NULL COMMENT '成交金额 = 数量 × 价格，成交时算好，读路径绝不重算',
    `fee`          decimal(20, 4) NOT NULL DEFAULT 0 COMMENT '本次手续费，落库的派生量',
    -- 卖出：= (卖价 − 落库的 avg_cost) × 数量 − 手续费；买入恒为 0（买入不实现盈亏）。
    `realized_pnl` decimal(20, 4) NOT NULL DEFAULT 0 COMMENT '本次成交实现的盈亏，成交时算好，读路径不重算',
    -- 成交后的现金快照。顺着历史把每一行的 cash_after 串起来，
    -- 就能定位账本是从哪一笔开始分叉的——这是只追加表最实用的性质。
    `cash_after`   decimal(20, 4) NOT NULL COMMENT '成交后现金余额快照，用于逐笔对账',

    `traded_at`    datetime(3)    NOT NULL,
    PRIMARY KEY (`id`),
    -- 唯一的读路径就是「某账户的成交历史按时间倒序分页」，
    -- 这个复合索引让排序直接走索引而不是 filesort。
    KEY `idx_paper_trades_account` (`account_id`, `traded_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci COMMENT ='模拟成交记录，只追加，非真实成交';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `paper_trades`;
-- +goose StatementEnd
