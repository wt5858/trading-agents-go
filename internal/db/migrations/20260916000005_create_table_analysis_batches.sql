-- +goose Up
-- +goose StatementBegin
-- 批量分析批次表。
--
-- 与 analysis_tasks 之间没有外键：Batch 与 Task 是两个独立的聚合根，
-- 外键会把它们绑进同一个事务，正是本次重构要拆掉的东西。
CREATE TABLE `analysis_batches`
(
    `id`               varchar(40)     NOT NULL,
    `user_id`          bigint unsigned NOT NULL,

    -- 子任务 ID 列表用 JSON 数组存：批次创建后集合不再变化，
    -- 而且 analysis_tasks.batch_id 上已有索引可以反查，没必要再维护一张关联表。
    -- 批次规模由 entities.MaxBatchSize 钉在百级，整份存 JSON 是安全的。
    `task_ids`         json            NULL COMMENT '子任务 ID 数组；创建后不再变化',
    `settled_task_ids` json            NULL COMMENT '已结算的子任务 ID 集合，是结算幂等的依据',

    `total`            bigint          NOT NULL DEFAULT 0,
    `completed`        bigint          NOT NULL DEFAULT 0,
    `failed`           bigint          NOT NULL DEFAULT 0,
    `percent`          decimal(5, 2)   NOT NULL DEFAULT 0 COMMENT '派生量，随聚合落库；读路径直接取，不用 (completed+failed)/total 重算',
    -- version 支撑乐观锁：并发结算时后写的 UPDATE 命中 0 行，由领域服务重试。
    -- 没有它，两个子任务同时回报结局会互相覆盖计数。
    `version`          bigint          NOT NULL DEFAULT 0 COMMENT '乐观锁版本号，每次结算 +1',

    `created_at`       datetime(3)     NOT NULL,
    -- 滞留批次的兜底扫描按 updated_at 排序取，故它必须随每次结算一起写。
    `updated_at`       datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- (user_id, created_at) 覆盖唯一的热查询：用户的批次列表按时间倒序翻页。
    KEY `idx_batches_user` (`user_id`, `created_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `analysis_batches`;
-- +goose StatementEnd
