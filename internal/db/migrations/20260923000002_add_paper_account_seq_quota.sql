-- +goose Up

-- +goose StatementBegin
-- seq 是账户在该用户名下的槽位号，从 0 开始。它存在的唯一目的是把
-- 「每个用户最多 10 个模拟账户」这条上限变成**数据库约束**。
--
-- 为什么应用层那句 CountByUser >= 10 挡不住：那是典型的 TOCTOU。两个并发请求
-- 都读到 9，都判定「还能开」，然后双双插入，用户得到 11 个账户。查询和插入
-- 是两条语句，中间那个窗口没有任何东西守着。
--
-- 槽位号 + 唯一键把检查和写入合成了一个原子动作：两个并发请求算出同一个 seq，
-- 插入时只有一个能成，另一个拿到 1062。而 CHECK 封住了槽位总数——
-- 即便应用层的检查被绕过（脚本直连、将来新增的入口忘了检查），也开不出第 11 个。
--
-- 上限 10 在这里和 domain_services 的 maxAccountsPerUser 各写了一遍。这是刻意的
-- 重复而不是疏忽：一个是给用户的友好提示，一个是最后一道闸。改的时候两边一起改，
-- 两边的注释互指。
ALTER TABLE `paper_accounts`
    ADD COLUMN `seq` tinyint unsigned NOT NULL DEFAULT 0 COMMENT '该用户名下的账户槽位号，从 0 开始；与 user_id 组成唯一键，配合 CHECK 封住账户数上限';
-- +goose StatementEnd

-- +goose StatementBegin
-- 回填存量数据：按开户先后给每个用户的账户编号。
-- 派生表里有窗口函数，MySQL 必然物化它，因此不会踩到「不能在子查询里引用被更新的表」。
UPDATE `paper_accounts` pa
    JOIN (SELECT `id`,
                 ROW_NUMBER() OVER (PARTITION BY `user_id` ORDER BY `created_at`, `id`) - 1 AS rn
          FROM `paper_accounts`) t ON t.`id` = pa.`id`
SET pa.`seq` = t.rn;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `paper_accounts`
    ADD UNIQUE KEY `uk_paper_accounts_user_seq` (`user_id`, `seq`);
-- +goose StatementEnd

-- +goose StatementBegin
-- 注意：若存量数据里已有用户持有超过 10 个账户，这条会失败。那说明上限确实被绕过过，
-- 应当先人工处理超额账户再迁移，而不是把闸门开大。
ALTER TABLE `paper_accounts`
    ADD CONSTRAINT `ck_paper_accounts_seq` CHECK (`seq` < 10);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
ALTER TABLE `paper_accounts`
    DROP CHECK `ck_paper_accounts_seq`;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE `paper_accounts`
    DROP KEY `uk_paper_accounts_user_seq`,
    DROP COLUMN `seq`;
-- +goose StatementEnd
