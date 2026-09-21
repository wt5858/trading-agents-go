-- +goose Up
-- +goose StatementBegin
-- 股票基础信息表。name / industry 存中文，字符集必须是 utf8mb4。
--
-- 同样不给 updated_at 数据库默认值：时间由聚合根维护，
-- 否则一次全量同步会把所有行的 updated_at 抹成同一时刻，增量对账就失去依据了。
CREATE TABLE `stocks`
(
    `id`         bigint unsigned NOT NULL AUTO_INCREMENT,
    `market`     varchar(8)      NOT NULL COMMENT 'A / HK / US',
    `symbol`     varchar(16)     NOT NULL COMMENT '不含市场后缀的纯代码，如 600519',
    `raw_code`   varchar(24)     NOT NULL DEFAULT '' COMMENT '数据源原始写法（600519.SH），仅供排查数据源差异，不参与索引',
    `name`       varchar(64)     NOT NULL DEFAULT '',
    `industry`   varchar(64)     NOT NULL DEFAULT '',
    `area`       varchar(64)     NOT NULL DEFAULT '',
    `list_date`  date                     DEFAULT NULL,
    `delisted`   tinyint(1)      NOT NULL DEFAULT 0,
    -- 市值用定点数而不是 float：float 在 SQL 层做聚合会累积误差，
    -- 而这两列是数据源直接给出的派生量，落库一次之后任何地方都不允许再用股价乘股本反推。
    -- 单位随数据源，领域层不做换算。
    `total_mv`   decimal(20, 4)  NOT NULL DEFAULT 0 COMMENT '总市值，单位随数据源，不做换算',
    `circ_mv`    decimal(20, 4)  NOT NULL DEFAULT 0 COMMENT '流通市值，单位随数据源，不做换算',
    `source`     varchar(16)     NOT NULL DEFAULT '' COMMENT '数据来源标识，如 tushare / finnhub',
    `updated_at` datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- (market, symbol) 才是业务主键：symbol 单列不唯一。market 放首位，
    -- 让「某市场全量列表」这条同步任务的热查询也能吃到同一个索引。
    UNIQUE KEY `uk_stocks_market_symbol` (`market`, `symbol`),
    -- name 上的 LIKE '关键词%' 能走索引，LIKE '%关键词%' 走不了——这是刻意的取舍：
    -- A 股全量不到 6000 行，中缀搜索全表扫也在毫秒级。
    KEY `idx_stocks_name` (`name`),
    KEY `idx_stocks_industry` (`industry`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `stocks`;
-- +goose StatementEnd
