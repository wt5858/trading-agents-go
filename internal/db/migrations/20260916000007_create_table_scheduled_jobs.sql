-- +goose Up
-- +goose StatementBegin
-- 定时任务定义表。
--
-- 注意这里没有 claimed_by / claimed_at 之类的抢占列：抢占凭据不落在这张表上，
-- 它的持久化形态是 job_executions.scheduled_for。
CREATE TABLE `scheduled_jobs`
(
    `id`                       varchar(40)     NOT NULL,

    -- name 唯一。这不只是防手滑：运维排查时是按名字找任务的，
    -- 出现两条同名任务时「暂停那个行情同步」会变成一次赌博。
    `name`                     varchar(128)    NOT NULL,

    `status`                   varchar(16)     NOT NULL COMMENT 'enabled / paused / disabled',

    -- next_run_at 身兼两职：既是业务字段，也是这一行的**乐观锁版本号**。
    -- ClaimDue 用 `WHERE id = ? AND next_run_at = <我 SELECT 时看到的值>` 做 CAS，
    -- RowsAffected == 1 就是「本副本赢得了这一次触发」的充分必要证据。
    -- 因此任何更新路径都不得在 ClaimDue 之外随意改写它（SaveRunOutcome 刻意不碰）。
    `next_run_at`              datetime(3)     NOT NULL COMMENT '下次触发时刻，同时充当 ClaimDue 抢占的乐观锁版本号',

    `kind`                     varchar(32)     NOT NULL COMMENT '任务种类，决定交给哪个运行器',
    -- cron_spec 存原文而不是解析结果：解析结果是派生物，
    -- 而原文是管理员输入的、需要原样回显的权威数据。
    `cron_spec`                varchar(128)    NOT NULL COMMENT '存 cron 原文，不存解析结果',
    `payload`                  json            NULL COMMENT '运行器自行解释的不透明参数，整体读写',

    `last_run_at`              datetime(3)              DEFAULT NULL,
    `last_status`              varchar(16)     NOT NULL DEFAULT '' COMMENT '最近一次执行结局，空串表示从未执行过',

    `consecutive_failures`     bigint          NOT NULL DEFAULT 0 COMMENT '连续失败计数，达到阈值后自动熔断；Resume 时清零',
    `max_consecutive_failures` bigint          NOT NULL DEFAULT 5 COMMENT '熔断阈值',

    -- 超时存秒而不是 time.Duration（纳秒）：纳秒数在 SQL 客户端里没法读，
    -- 而这一列是运维会直接手改的少数几列之一。
    `timeout_seconds`          bigint          NOT NULL DEFAULT 600 COMMENT '单次执行超时，单位秒（刻意不用纳秒，方便运维手改）',

    `total_runs`               bigint          NOT NULL DEFAULT 0,
    `success_runs`             bigint          NOT NULL DEFAULT 0,
    `success_rate`             decimal(5, 2)   NOT NULL DEFAULT 0 COMMENT '百分数；写入时算好，读路径不用 success_runs/total_runs 重算',

    -- created_by 只存 identity 上下文的用户 ID，没有也不该有外键：
    -- 跨上下文只按标识引用，外键会把两个上下文绑进同一个数据库生命周期。
    `created_by`               bigint unsigned NOT NULL COMMENT 'identity 上下文的用户 ID，刻意不加外键',
    `created_at`               datetime(3)     NOT NULL,
    `updated_at`               datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_jobs_name` (`name`),
    -- idx_jobs_due 是调度器的命脉：ClaimDue 每个 tick 都会跑
    -- `WHERE status = 'enabled' AND next_run_at <= ? ORDER BY next_run_at`。
    -- (status, next_run_at) 这个顺序让它既走索引过滤又走索引排序，
    -- 完全避免在一张会长到几万行的表上做全表扫 + filesort。
    KEY `idx_jobs_due` (`status`, `next_run_at`),
    -- (kind, status) 服务管理界面的筛选，与 idx_jobs_due 的职责不同，不要合并：
    -- 合并成一个三列索引之后，调度器的热查询就得带上 kind 才能走全索引。
    KEY `idx_jobs_kind_status` (`kind`, `status`),
    KEY `idx_jobs_creator` (`created_by`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `scheduled_jobs`;
-- +goose StatementEnd
