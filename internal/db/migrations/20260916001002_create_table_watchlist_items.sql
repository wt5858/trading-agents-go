-- +goose Up
-- +goose StatementBegin
-- 自选股明细表，**子实体** WatchlistItem 的落地形态。
--
-- 它与 watchlist_groups 属于同一个聚合：两张表由 WatchlistGroupRepository.Save
-- 在同一个事务里一起写。这与「不同聚合根之间不共享事务」并不矛盾——
-- 被禁止的是把两个**聚合根**绑进一个事务，而根与它的子实体本来就是一个一致性单元。
--
-- # 为什么没有指向 watchlist_groups 的外键
--
-- 删除分组时确实必须连它的自选股一起删，但那件事由仓储在事务里用一条显式的
-- DELETE 完成，而不是交给 ON DELETE CASCADE。理由是级联**看不见**：
-- 读代码的人不会知道还有一张表被清空了，排查数据丢失时也想不到去翻建表语句。
-- 仓储本来就持有事务，把这一步写成一条摆在明面上的语句成本为零。
-- 顺带的好处是每次插入自选股都省掉一次外键检查。
CREATE TABLE `watchlist_items`
(
    `id`             bigint unsigned NOT NULL AUTO_INCREMENT,
    `group_id`       bigint unsigned NOT NULL COMMENT '所属分组；子实体只能经由聚合根写入，没有独立的仓储',

    -- 代码拆成 market / symbol 两列而不是整体存一个串：它们要进唯一索引和 WHERE 条件。
    -- 口径与 stocks 表保持一致，便于将来按 (market, symbol) 关联查询。
    `market`         varchar(8)      NOT NULL COMMENT 'CN / HK / US',
    `symbol`         varchar(16)     NOT NULL COMMENT '不含市场后缀的纯代码，如 600519',
    `raw_code`       varchar(24)     NOT NULL DEFAULT '' COMMENT '用户输入的原始写法（600519.SH），仅供排查，不参与索引与判重',

    `note`           varchar(300)    NOT NULL DEFAULT '' COMMENT '备注存中文，上限 100 字符由领域层判定，此处列宽只负责放得下',

    -- 参考价 = 加入自选那一刻的最新价，是被观测到的事实，写入后永不改写。
    -- 用定点数而不是 float：这一列是「自选以来涨跌幅」的**分母**，
    -- float 的末位漂移会让同一条记录在不同机器上算出不同的百分比。
    -- 为 0 表示加自选时取不到行情（停牌、新股、数据源未覆盖），此时该百分比不展示。
    `ref_price`      decimal(20, 4)  NOT NULL DEFAULT 0 COMMENT '加入自选时的参考价；观测值，非派生量，永不重算',
    -- 日期一并存下，参考价才可审计：用户看到 +12.3% 时必须能回答「相对哪一天的哪个价」。
    `ref_trade_date` varchar(10)     NOT NULL DEFAULT '' COMMENT '参考价对应的交易日 YYYY-MM-DD',
    `ref_price_at`   datetime(3)              DEFAULT NULL COMMENT '取到参考价的时刻',

    -- sort_order 由聚合根维护，恒为组内 0..n-1 的稠密连续序列。
    -- 这里**刻意不加唯一约束**：重排时必然出现两行同号的中间态（交换两只票的位置），
    -- 加了唯一约束就得引入临时负数之类的把戏，白白把一条批量 upsert 拆成多步。
    -- 稠密连续是聚合根的不变式，数据库只负责按它排序。
    `sort_order`     int             NOT NULL DEFAULT 0 COMMENT '组内排序，由聚合根保证稠密连续',

    `created_at`     datetime(3)     NOT NULL,
    `updated_at`     datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 「同一分组内不重复持有同一只股票」的数据库兜底。
    --
    -- 聚合根的 AddItem 已经判过一次，为什么还要这一道：内存判断只在**同一个聚合实例**
    -- 内成立。同一个用户开两个标签页同时点「加自选」，会加载出两个各自认为
    -- 自己没重复的聚合实例。唯一索引是唯一挡得住那一幕的东西。
    UNIQUE KEY `uk_watchlist_items_group_code` (`group_id`, `market`, `symbol`),
    -- 加载聚合时的热查询是 WHERE group_id IN (...) ORDER BY sort_order，
    -- 这个索引让定位和排序一次吃完，不需要额外的排序开销。
    KEY `idx_watchlist_items_group_sort` (`group_id`, `sort_order`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `watchlist_items`;
-- +goose StatementEnd
