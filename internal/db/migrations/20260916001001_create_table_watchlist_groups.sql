-- +goose Up
-- +goose StatementBegin
-- 自选股分组表，聚合根 WatchlistGroup 的落地形态。
--
-- created_at / updated_at 刻意不给数据库默认值：DTO 上 autoCreateTime / autoUpdateTime
-- 都是 false，时间由聚合根自己维护。库里一旦补上 CURRENT_TIMESTAMP，
-- 「导入存量自选股」就会被静默改写成导入时刻，而那种错位只有在对账时才会暴露。
CREATE TABLE `watchlist_groups`
(
    `id`         bigint unsigned NOT NULL AUTO_INCREMENT,
    `user_id`    bigint unsigned NOT NULL,
    -- name 存中文分组名（「科技股」「打新」），字符集必须是 utf8mb4。
    -- 列宽 64 字节对应领域层 16 个字符的上限（一个汉字 3 字节，留足余量）：
    -- 上限按字符数判定在 value_objects.NewGroupName，这里只负责放得下。
    `name`       varchar(64)     NOT NULL,
    `created_at` datetime(3)     NOT NULL,
    `updated_at` datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 「同一用户下分组名唯一」这条不变式的**唯一**保证就在这个索引上。
    --
    -- 它为什么不在聚合里判：那是一条跨聚合实例的规则，内存中的某一个分组
    -- 看不见这个用户的其它分组。而「先 SELECT 有没有重名、再 INSERT」是 TOCTOU——
    -- 两个并发的创建请求会双双查到「不重名」，然后双双插入。
    -- 只有唯一索引能把检查和写入合并成一个原子操作。
    --
    -- user_id 放首位还顺带覆盖了「列出我的全部分组」这条热查询的等值定位。
    UNIQUE KEY `uk_watchlist_groups_user_name` (`user_id`, `name`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `watchlist_groups`;
-- +goose StatementEnd
