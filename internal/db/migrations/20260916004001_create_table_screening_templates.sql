-- +goose Up
-- +goose StatementBegin
-- 选股模板表，聚合根 ScreeningTemplate 的落地形态。
--
-- created_at / updated_at 刻意不给数据库默认值：DTO 上 autoCreateTime / autoUpdateTime
-- 都是 false，时间由聚合根自己维护。库里一旦补上 CURRENT_TIMESTAMP，
-- 「导入存量模板」就会被静默改写成导入时刻，而那种错位只有在对账时才会暴露。
CREATE TABLE `screening_templates`
(
    `id`             bigint unsigned NOT NULL AUTO_INCREMENT,
    `user_id`        bigint unsigned NOT NULL COMMENT '模板属主；公开模板只是可被他人读取，改写永远只有属主能做',
    -- name 存中文模板名（「低估值蓝筹」「高成长小盘」），字符集必须是 utf8mb4。
    -- 列宽 128 字节对应领域层 32 个字符的上限（一个汉字 3 字节，留足余量）：
    -- 上限按字符数判定在 value_objects.NewTemplateName，这里只负责放得下。
    `name`           varchar(128)    NOT NULL,
    `description`    varchar(600)    NOT NULL DEFAULT '' COMMENT '策略说明，上限 200 字符由领域层截断',

    -- 排序规则拆成两列而不是存一个 "total_mv desc" 的串。
    --
    -- 理由与防注入是同一回事：存成一个串，读回来就得解析它，而解析出来的那半截
    -- 会被当成 ORDER BY 的列名用。两列分开存，字段名读回来仍然要过
    -- value_objects 里的白名单（RehydrateSortSpec），方向则只有两个合法取值。
    `sort_field`     varchar(32)     NOT NULL DEFAULT '' COMMENT '排序字段名，必须是白名单内的可筛选字段；空表示用默认排序',
    `sort_direction` varchar(4)      NOT NULL DEFAULT 'desc' COMMENT 'asc / desc',
    -- 列名不叫 limit：limit 是 MySQL 保留字，建表和每一条手写查询都要加反引号，
    -- 迟早会有人漏掉一次。
    `result_limit`   int             NOT NULL DEFAULT 50 COMMENT '返回条数，上限 500 由领域层收敛',

    `is_public`      tinyint(1)      NOT NULL DEFAULT 0 COMMENT '是否公开到选股策略广场；只影响可读性，不影响可写性',

    `created_at`     datetime(3)     NOT NULL,
    `updated_at`     datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 「同一用户下模板名唯一」这条不变式的**唯一**保证就在这个索引上。
    --
    -- 它为什么不在聚合里判：那是一条跨聚合实例的规则，内存中的某一个模板
    -- 看不见这个用户的其它模板。而「先 SELECT 有没有重名、再 INSERT」是 TOCTOU——
    -- 两个并发的创建请求会双双查到「不重名」，然后双双插入。
    -- 只有唯一索引能把检查和写入合并成一个原子操作，仓储负责把 1062 翻译成
    -- AlreadyExists。
    --
    -- user_id 放首位还顺带覆盖了「列出我的全部模板」这条热查询的等值定位。
    UNIQUE KEY `uk_screening_templates_user_name` (`user_id`, `name`),
    -- 选股策略广场按更新时间倒序翻页，(is_public, updated_at) 让过滤与排序
    -- 一次吃完，不需要额外的 filesort。
    KEY `idx_screening_templates_public` (`is_public`, `updated_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `screening_templates`;
-- +goose StatementEnd
