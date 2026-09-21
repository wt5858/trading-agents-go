-- +goose Up
-- +goose StatementBegin
-- 定时任务改为消息驱动：调度器抢到触发后只写一条 queued 记录并发消息，
-- 真正的执行发生在消费端。本次变更给 job_executions 加上支撑这套流程所需的列与索引。
--
-- 与原设计的差别要说清楚：这张表不再是纯追加的。一行现在会被改写两次
-- （queued -> running -> 终态）。两次改写都带状态谓词，因此每次都是一个原子的
-- 状态推进而不是覆盖，「终态不可改写」由 WHERE 子句保证而不是靠调用方自觉。
ALTER TABLE `job_executions`
    -- queued_at 是调度器发现这次触发的时刻。
    -- 它与 scheduled_for 之差是调度延迟，与 started_at 之差是消息在队列里排的队。
    -- 后者是消息驱动之后才出现的一段延迟，也是最值得盯的一段：它变大说明消费者不够用，
    -- 而调度延迟对此毫无反应——只看调度延迟的话，堵了半小时的队列和健康时一模一样。
    ADD COLUMN `queued_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
        COMMENT '入队时刻；与 started_at 之差即消息在队列中的等待时长' AFTER `scheduled_for`,

    -- attempt 从 1 开始。重试不改写失败的记录，而是新建下一条——
    -- 重试三次的任务如果只看得到最后一次，前两次为什么失败就无从查起。
    ADD COLUMN `attempt`   int         NOT NULL DEFAULT 1
        COMMENT '这次触发的第几次尝试，从 1 开始；重试是新记录而不是改写' AFTER `finished_at`;
-- +goose StatementEnd

-- +goose StatementBegin
-- 存量数据补一个合理的 queued_at：老记录是同步执行的，入队与开始是同一刻。
UPDATE `job_executions`
SET `queued_at` = `started_at`
WHERE `queued_at` > `started_at`;
-- +goose StatementEnd

-- +goose StatementBegin
-- uk_exec_occurrence 是一次尝试的天然主键，作用是**挡住重复入队**而不是加速查询。
--
-- 没有它会怎样：调度器在「CAS 抢到触发」与「写下 queued 记录」之间被杀掉再重启，
-- 恢复巡检会重新入队同一次触发，于是同一次触发有两条 queued 记录、被跑两遍。
-- 应用层先查一下在不在挡不住这件事——那正是 TOCTOU，两个副本的查询会同时返回「不存在」。
ALTER TABLE `job_executions`
    ADD UNIQUE KEY `uk_exec_occurrence` (`job_id`, `scheduled_for`, `attempt`);
-- +goose StatementEnd

-- +goose StatementBegin
-- idx_exec_pending 专供恢复巡检：「哪些记录卡在 queued/running 且已经卡了很久」。
-- 没有它，这个每分钟一次的巡检就是一次全表扫描，而这张表是六位数行起步的。
ALTER TABLE `job_executions`
    ADD KEY `idx_exec_pending` (`status`, `queued_at`);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE `job_executions`
    DROP KEY `idx_exec_pending`,
    DROP KEY `uk_exec_occurrence`,
    DROP COLUMN `attempt`,
    DROP COLUMN `queued_at`;
-- +goose StatementEnd
