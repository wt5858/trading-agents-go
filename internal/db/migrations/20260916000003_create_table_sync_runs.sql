-- +goose Up
-- +goose StatementBegin
-- 数据同步运行记录。
CREATE TABLE `sync_runs`
(
    `id`           varchar(48)   NOT NULL,
    `kind`         varchar(24)   NOT NULL COMMENT '同步类型，如 stock_list / quote',
    `market`       varchar(8)    NOT NULL,
    `status`       varchar(16)   NOT NULL COMMENT 'running / success / failed / cancelled',

    -- running_key 是这张表最重要的一列，它把「同类型同市场同时只能有一个实例在跑」
    -- 变成了一条数据库约束，而不是一段应用层的检查代码。
    --
    -- MySQL 没有部分索引，但唯一索引不约束 NULL：运行中写 "kind:market"，
    -- 进入终态写 NULL，于是 uk_sync_runs_running 天然只对运行中的行生效。
    -- 所以这一列**必须可空**，而且终态必须写 NULL 而不是空串——
    -- 空串之间仍然互相冲突，那样第二次同步就再也插不进去了。
    --
    -- 并发重复触发因此会被 INSERT 时的 1062 直接挡掉。换成「先查有没有在跑、再插入」
    -- 就是典型的 TOCTOU：两个请求都能查到「没有」，然后双双插入。
    `running_key`  varchar(40)            DEFAULT NULL COMMENT '运行中为 kind:market，终态必须置 NULL 以释放唯一索引占位',

    `total`        bigint        NOT NULL DEFAULT 0,
    `succeeded`    bigint        NOT NULL DEFAULT 0,
    `failed`       bigint        NOT NULL DEFAULT 0,
    `skipped`      bigint        NOT NULL DEFAULT 0,
    -- 成功率是落库那一刻固化的事实，读路径直接取，不用 succeeded/total 重算。
    `success_rate` decimal(5, 2) NOT NULL DEFAULT 0 COMMENT '百分数，0.00 - 100.00；写入时算好，读路径不重算',

    `cursor`       varchar(64)            DEFAULT NULL COMMENT '断点续传游标，语义由各同步类型自行解释',
    `triggered_by` varchar(64)            DEFAULT NULL COMMENT '触发来源：用户名、定时任务名或 system',
    `started_at`   datetime(3)   NOT NULL,
    `finished_at`  datetime(3)            DEFAULT NULL COMMENT '仅终态有值；卡在 running 的行由启动时的僵死清理补写',
    `duration_ms`  bigint        NOT NULL DEFAULT 0 COMMENT '派生量，随记录落库，不用 finished_at - started_at 重算',
    `error`        text          NULL,
    `created_at`   datetime(3)   NOT NULL,
    `updated_at`   datetime(3)   NOT NULL,
    PRIMARY KEY (`id`),
    -- 唯一索引本身就是并发互斥的执行者，详见 running_key 列上的说明。
    UNIQUE KEY `uk_sync_runs_running` (`running_key`),
    KEY `idx_sync_runs_kind_market` (`kind`, `market`),
    -- 启动时的僵死清理走这个索引：WHERE status = 'running' AND started_at < ?
    KEY `idx_sync_runs_status` (`status`),
    -- LatestOf 与 List 都按 started_at 倒序取，单列索引让排序走索引而不是 filesort。
    KEY `idx_sync_runs_started` (`started_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `sync_runs`;
-- +goose StatementEnd
