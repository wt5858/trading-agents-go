-- +goose Up
-- +goose StatementBegin
-- 定时任务执行历史。
--
-- 与 scheduled_jobs 之间**没有外键**：两者是独立的聚合根，外键会把它们绑进同一个事务，
-- 也会让「删除一条任务」被迫连带处理几十万行历史。执行历史是审计记录，
-- 「任务被删了」不代表「它从来没跑过」，因此删除任务时刻意不做级联。
--
-- 这张表是纯追加的：写入路径只有 INSERT，没有任何 UPDATE，清理路径只有按时间的批量 DELETE。
-- 没有 UPDATE 意味着归档作业可以和调度器并行跑而不会互相阻塞在行锁上。
CREATE TABLE `job_executions`
(
    `id`            varchar(40)  NOT NULL,

    `job_id`        varchar(40)  NOT NULL COMMENT '跨聚合的标识引用，刻意不加外键',
    `started_at`    datetime(3)  NOT NULL,

    `job_kind`      varchar(32)  NOT NULL DEFAULT '' COMMENT '执行时刻的任务种类快照，任务改名改种类后历史仍可读',

    -- scheduled_for 与 started_at 之差就是调度延迟，两列都必须存：
    -- 只存其一就再也算不出调度器有没有滞后。
    `scheduled_for` datetime(3)  NOT NULL COMMENT '本次触发的计划时刻；与 started_at 之差即调度延迟',
    `finished_at`   datetime(3)           DEFAULT NULL,

    `status`        varchar(16)  NOT NULL COMMENT 'success / failed / timeout 等',
    `summary`       varchar(512) NOT NULL DEFAULT '' COMMENT '运行器给出的人类可读结果摘要，含中文',
    `item_count`    bigint       NOT NULL DEFAULT 0 COMMENT '本次处理的条目数，语义由各运行器自行解释',
    `error`         varchar(512) NOT NULL DEFAULT '',

    -- duration_ms 是派生量，随记录一起落库。读路径直接取，不用 finished_at - started_at 重算：
    -- SQL 层的时间差计算在跨时区/跨精度时会给出和应用层不一致的结果，
    -- 而这一列是耗时看板的唯一数据源。
    `duration_ms`   bigint       NOT NULL DEFAULT 0 COMMENT '派生量，随记录落库，不在 SQL 层重算',

    `manual`        tinyint(1)   NOT NULL DEFAULT 0 COMMENT '是否由人工触发；区分「调度器跑的」与「运维手点的」',
    PRIMARY KEY (`id`),
    -- idx_exec_job 支撑「某任务的执行历史，按时间倒序分页」这条唯一的列表查询，
    -- (job_id, started_at) 让过滤和排序都走索引。
    KEY `idx_exec_job` (`job_id`, `started_at`),
    -- idx_exec_started_at 单列索引专供 PurgeOlderThan：保留期清理是
    -- `DELETE WHERE started_at < ?`，不带 job_id，走不了上面那个复合索引的第一列。
    KEY `idx_exec_started_at` (`started_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `job_executions`;
-- +goose StatementEnd
