-- +goose Up
-- +goose StatementBegin
-- 分析任务表。
--
-- 主键是领域服务生成的 UUID 而不是自增：任务要先落库再入队列，
-- 队列里必须有一个稳定标识，自增主键在入队那一刻还拿不到。
--
-- 与 analysis_batches 之间没有外键：两者是独立的聚合根，
-- 外键会要求它们同事务写入，正是本次重构要拆掉的东西。
CREATE TABLE `analysis_tasks`
(
    `id`          varchar(40)     NOT NULL,
    `user_id`     bigint unsigned NOT NULL,
    `status`      varchar(16)     NOT NULL COMMENT 'pending / running / completed / failed / cancelled',
    `batch_id`    varchar(40)     NOT NULL DEFAULT '' COMMENT '跨聚合的标识引用，非批量任务为空串；刻意不加外键',

    -- 下面三列是从 request JSON 拍平出来的冗余列，只服务运维排查与看板聚合
    -- （「今天 600519 被分析了多少次」不该去 JSON 里捞）。
    -- request 列才是权威数据源：重建聚合时一律从 JSON 反序列化，绝不读这三列。
    `symbol`      varchar(16)     NOT NULL DEFAULT '' COMMENT '由 request 派生的冗余列，仅供聚合统计',
    `market`      varchar(8)      NOT NULL DEFAULT '' COMMENT '由 request 派生的冗余列，仅供聚合统计',
    `trade_date`  char(10)        NOT NULL DEFAULT '' COMMENT 'YYYY-MM-DD；由 request 派生的冗余列',

    -- Request / Progress / Result 都是整体读写的值对象，用 JSON 列。
    -- 尤其 Progress 的步骤数组会随分析深度变化，拆表会变成一张写放大严重的小表。
    `request`     json            NULL COMMENT '任务参数的权威来源，重建聚合只读这一列',
    `progress`    json            NULL COMMENT '进度快照，含写入当时算好的 percent / eta，读路径不重算',
    `result`      json            NULL COMMENT '仅 status=completed 时有值；未完成任务占大头，保持 NULL 以免撑大表空间',
    `error`       text            NULL,

    `attempts`    bigint          NOT NULL DEFAULT 0 COMMENT '已尝试执行次数，重试上限由领域层判定',
    `created_at`  datetime(3)     NOT NULL,
    `started_at`  datetime(3)              DEFAULT NULL,
    `finished_at` datetime(3)              DEFAULT NULL,
    PRIMARY KEY (`id`),
    -- (user_id, status) 覆盖两条最热的查询：用户任务列表按状态筛选，
    -- 以及单用户并发额度统计（CountRunningByUser 只扫索引，不回表）。
    KEY `idx_tasks_user_status` (`user_id`, `status`),
    -- 批次内任务数是十级别的，单列索引足够反查。
    KEY `idx_tasks_batch` (`batch_id`),
    -- 支撑「最近任务」排序与按时间归档的清理作业。
    KEY `idx_tasks_created_at` (`created_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `analysis_tasks`;
-- +goose StatementEnd
