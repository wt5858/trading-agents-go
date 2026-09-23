-- +goose Up

-- +goose StatementBegin
-- notifications 的清理作业按 created_at 范围删，但这张表上没有任何索引以它打头：
-- 两个现存索引都以 user_id 起手（uk_notifications_user_dedupe、
-- idx_notifications_user_read_created），对一个不带 user_id 的范围条件都用不上。
--
-- 分块删除本身挡不住这件事。前面几块之所以看着便宜，只是因为主键顺序恰好近似
-- created_at 顺序；而**最后一块**（返回行数不足一批的那次）必须扫到表尾才能确认
-- 没有更多可删的行。InnoDB 的 DELETE 对它**检查过**的每一行加 next-key 锁，不只是
-- 删掉的那些，于是这一下会把正在写通知的事件处理器一起锁住——正是分块本来想避免的。
--
-- 对照组是 job_executions：它有 idx_exec_started_at，同样形状的清理循环就没有这个问题。
ALTER TABLE `notifications`
    ADD KEY `idx_notifications_created_at` (`created_at`);
-- +goose StatementEnd

-- +goose StatementBegin
-- analysis_batches 的滞留巡检是 WHERE completed + failed < total AND updated_at < ?
-- ORDER BY updated_at ASC，而表上只有主键和 (user_id, created_at)——没有一个能用。
-- 这个巡检按固定间隔跑在一张只增不减的表上，每次都是全表扫描加文件排序。
--
-- 只索引 updated_at：另一半条件 completed + failed < total 是列间比较，
-- 不可能走索引（sargable 的前提是一侧为常量）。让它退化成时间窗内的后置过滤即可——
-- 窗口本身已经把候选集压到很小。
ALTER TABLE `analysis_batches`
    ADD KEY `idx_batches_stale` (`updated_at`);
-- +goose StatementEnd

-- +goose StatementBegin
-- analysis_tasks 的任务列表：WHERE user_id = ? [AND status = ?]
-- ORDER BY created_at DESC, id DESC。
--
-- idx_tasks_user_status 只到 status 为止，等值条件命中之后剩下的行仍是主键序，
-- 于是每翻一页都要把该用户的全部任务排一遍。analysis_reports 上同形状的查询
-- 早就配了 idx_reports_user_created (user_id, created_at DESC)，这张表只是漏了。
--
-- 建两条而不是一条，是因为 status 可选：
--   带 status → 前三列全是等值/有序，排序直接由索引给出；
--   不带 status → status 不是常量，(user_id, status, created_at) 的 created_at
--                 段是分段有序的，排不了，必须另有一条以 user_id 起手直接接 created_at。
-- 排序尾巴带上 id，是因为查询用它做同毫秒的 tie-break；方向也要跟着 DESC。
ALTER TABLE `analysis_tasks`
    DROP KEY `idx_tasks_user_status`,
    ADD KEY `idx_tasks_user_status_created` (`user_id`, `status`, `created_at` DESC, `id` DESC),
    ADD KEY `idx_tasks_user_created` (`user_id`, `created_at` DESC, `id` DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- sync_runs.LatestOf 是 WHERE kind = ? AND market = ? ORDER BY started_at DESC LIMIT 1，
-- 而 idx_sync_runs_kind_market 到 market 为止，取一行最新记录要把该 kind/market 的
-- 全部历史排一遍。这张表每跑一次同步就多一行，只增不减。
--
-- 直接加宽原索引而不是新建：(kind, market) 是新索引的前缀，原索引的全部用途都被覆盖。
-- 表上另有 idx_sync_runs_started (started_at)，那条服务的是不带 kind/market 的
-- 时间范围查询，与这条不重合，保留。
ALTER TABLE `sync_runs`
    DROP KEY `idx_sync_runs_kind_market`,
    ADD KEY `idx_sync_runs_kind_market_started` (`kind`, `market`, `started_at` DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- job_executions.ListStale 的条件是两个分支的 OR：
--   (status = 'queued'  AND queued_at  < ?) OR (status = 'running' AND started_at < ?)
-- 第一支能走 idx_exec_pending (status, queued_at)，第二支没有对应索引，
-- 于是 MySQL 无法做 index merge union，整条查询退回全表扫描——而这是库里最大的表。
--
-- 补上第二支的索引，两支就都各有所依。对照组是 analysis_tasks.ListStale：
-- 它的两支共用同一个时间列，所以一条 idx_tasks_stale 就够，index merge 能成立。
ALTER TABLE `job_executions`
    ADD KEY `idx_exec_running` (`status`, `started_at`);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
ALTER TABLE `job_executions`
    DROP KEY `idx_exec_running`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `sync_runs`
    DROP KEY `idx_sync_runs_kind_market_started`,
    ADD KEY `idx_sync_runs_kind_market` (`kind`, `market`);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `analysis_tasks`
    DROP KEY `idx_tasks_user_created`,
    DROP KEY `idx_tasks_user_status_created`,
    ADD KEY `idx_tasks_user_status` (`user_id`, `status`);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `analysis_batches`
    DROP KEY `idx_batches_stale`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `notifications`
    DROP KEY `idx_notifications_created_at`;
-- +goose StatementEnd
