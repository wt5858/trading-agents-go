-- +goose Up
-- +goose StatementBegin
-- 模拟交易账户（聚合根）。
--
-- 这是**模拟盘**：没有真实资金、没有券商通道、没有撮合。表里的任何数字
-- 都不得被当作真实业绩使用。
--
-- 全表金额一律 decimal(20,4)，绝不用 float/double：
-- 二进制浮点表示不了十进制小数，0.1 存进去读出来可能是 0.09999999999999999。
-- 一个慢慢对不平的模拟账本没有任何参考价值，因为使用者再也分不清
-- 「我的策略亏了 0.03」和「浮点误差累计了 0.03」。
CREATE TABLE `paper_accounts`
(
    `id`           varchar(48)     NOT NULL COMMENT '账户 ID，由领域服务生成，持仓与成交都引用它',
    `user_id`      bigint unsigned NOT NULL COMMENT '归属用户，跨聚合引用，不加外键',
    `name`         varchar(64)     NOT NULL DEFAULT '' COMMENT '账户名称，便于并行跑多个策略时区分',

    `initial_cash` decimal(20, 4)  NOT NULL COMMENT '开户资金，也是重置时恢复的基准，落库后不变',
    `cash`         decimal(20, 4)  NOT NULL COMMENT '可用现金。它与 paper_positions 共同构成账本两端',

    -- 已实现盈亏与累计手续费都是**逐笔成交时算好、累加落库**的派生量。
    -- 读路径直接取这两列，绝不去 paper_trades 做 SUM：
    -- 历史会无限增长，SUM 既越来越慢，又会在历史被归档/分页截断时静默算错，
    -- 而且和账户现金余额的变动失去唯一事实来源。
    `realized_pnl` decimal(20, 4)  NOT NULL DEFAULT 0 COMMENT '已实现盈亏累计，写入时算好，读路径不重算',
    `total_fee`    decimal(20, 4)  NOT NULL DEFAULT 0 COMMENT '累计手续费，写入时累加，读路径不重算',

    -- version 是乐观锁。同一账户上两笔并发下单如果各自「读-改-写」，
    -- 后写的会用自己读到的旧现金覆盖前一次的结果，账户凭空多出一笔钱。
    -- 仓储的 UPDATE 带 WHERE id = ? AND version = ?，把「我读到的版本仍是当前版本」
    -- 这个前提写进语句本身，检查与写入因此是一个原子操作；命中 0 行即冲突。
    `version`      bigint          NOT NULL DEFAULT 0 COMMENT '乐观锁版本号，每次保存 +1',

    `created_at`   datetime(3)     NOT NULL,
    `updated_at`   datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 用户的账户列表是唯一的热查询；单用户账户数是个位数，单列索引足够。
    KEY `idx_paper_accounts_user` (`user_id`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci COMMENT ='模拟交易账户，非真实资金';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `paper_accounts`;
-- +goose StatementEnd
