-- +goose Up
-- +goose StatementBegin
-- 分析任务改为「认领式」消费：消费者拿到消息后用一条带状态谓词的 UPDATE
-- 把任务从 queued 推到 running，命中 1 行才算抢到。投递是至少一次的，
-- 没有这条谓词，同一个任务会被两个消费者同时跑完——一次分析是十几轮 LLM 调用，
-- 跑两遍就是账单付两遍。
--
-- 认领本身只需要 status 这一列，但它引出了一个新问题：赢家在执行途中崩掉之后，
-- 那一行会永远停在 running，再也没有人能认领它。原先靠 Redis 的可见性超时 +
-- 心跳续约兜底，这套机制随队列一起下线，兜底改由数据库上的停滞巡检承担。
-- 本次变更加的就是那个巡检唯一缺的东西：一个可索引的「状态是什么时候变的」。
ALTER TABLE `analysis_tasks`
    -- 为什么不复用 started_at：它的语义是「首次启动时刻，重试不覆盖」，
    -- 服务于 Duration() 对外报的总耗时。拿它当巡检依据会误判——
    -- 一个第三次尝试、刚跑了两分钟的健康任务，started_at 可能是一小时前，
    -- 于是巡检会把它当成卡死的判失败。杀掉一个正在跑的付费任务，
    -- 正是这套机制要避免的事，不能让它成为机制自己的副作用。
    --
    -- 为什么一列够用：queued 卡太久（消息丢了）和 running 卡太久（消费者死了）
    -- 问的是同一件事——「这一行维持当前状态多久了」，只是阈值和处置不同。
    ADD COLUMN `state_changed_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
        COMMENT '当前状态是何时进入的；停滞巡检的唯一依据，每次状态迁移都必须更新'
        AFTER `attempts`;
-- +goose StatementEnd

-- +goose StatementBegin
-- 存量数据补一个不会造成误判的值：取「已知的最后一次动作」。
-- 宁可偏新——偏新只会让巡检晚一轮才捡起真正卡住的行，偏旧则会把健康的行判死。
UPDATE `analysis_tasks`
SET `state_changed_at` = COALESCE(`finished_at`, `started_at`, `created_at`);
-- +goose StatementEnd

-- +goose StatementBegin
-- idx_tasks_stale 专供停滞巡检：「哪些任务卡在 queued/running 且已经卡了很久」。
-- 没有它，这个定期巡检就是一次全表扫描。
--
-- 列序是 (status, state_changed_at) 而不是反过来：巡检永远先按状态收窄
-- （queued/running 只占全表的一小部分），再在其中按时间做范围扫描。
-- 反过来会让范围扫描先跑在全表上。
ALTER TABLE `analysis_tasks`
    ADD KEY `idx_tasks_stale` (`status`, `state_changed_at`);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE `analysis_tasks`
    DROP KEY `idx_tasks_stale`,
    DROP COLUMN `state_changed_at`;
-- +goose StatementEnd
