-- +goose Up
-- +goose StatementBegin
-- 站内通知表。
--
-- 本上下文是一个纯消费者：这张表里的行几乎全部由别的上下文的领域事件写入，
-- 用户对它只有「读、标已读、删」三种操作。
--
-- 标题与正文是中文，且大模型产出的结论里常带 emoji，所以字符集必须是 utf8mb4
-- 而不是 utf8——后者只有 3 字节，emoji 会直接写失败。
CREATE TABLE `notifications`
(
    `id`         bigint unsigned NOT NULL AUTO_INCREMENT,

    -- 收件人。identity 上下文的用户 ID，跨上下文只按 ID 引用，刻意不加外键：
    -- 加了外键，删一个用户就会被他的历史通知挡住，而通知本就该跟着用户一起清理，
    -- 那是清理任务的职责，不是一次 DELETE 的副作用。
    `user_id`    bigint unsigned NOT NULL,

    `kind`       varchar(32)     NOT NULL COMMENT 'analysis_completed / analysis_failed / sync_failed / job_paused / system',
    `level`      varchar(16)     NOT NULL DEFAULT 'info' COMMENT 'info / warning / error，决定前端的图标与配色',
    `title`      varchar(128)    NOT NULL DEFAULT '' COMMENT '列宽按字符数计，放得下 64 个汉字的领域上限',
    `body`       varchar(1024)   NOT NULL DEFAULT '' COMMENT '一句话摘要；完整内容去 link 指向的详情页看',

    -- 去重键：由 (kind, 来源聚合 ID) 推导出的稳定字符串，见 value_objects.DedupeKey。
    -- 长度上限 160 与 VO 里的 MaxDedupeKeyLen 一致，超长的来源标识在构造点就被哈希掉——
    -- 留到这里被数据库静默截断的话，两个不同来源会截出同一个键，
    -- 于是第二条本该独立的通知被当成重放丢弃，且没有任何痕迹。
    `dedupe_key` varchar(160)    NOT NULL COMMENT '幂等键，与 user_id 组成唯一索引',

    -- 只存 (link_type, link_id) 这一对标量，不存来源实体的任何冗余字段。
    -- 通知是「某一刻发生过某件事」的存档：需要详情就拿 link_id 去调那个上下文
    -- 自己的接口，那才是它的职责，也避免了存档与实体各说各话。
    `link_type`  varchar(32)     NOT NULL DEFAULT '' COMMENT 'analysis_task / sync_run / scheduled_job；空表示无跳转',
    `link_id`    varchar(64)     NOT NULL DEFAULT '' COMMENT '来源聚合的 ID，前端据此拼路由',

    -- NULL 即未读。用「已读时刻」而不是一个 read 布尔列：
    -- 「什么时候读的」一旦丢掉就补不回来，而它恰恰是排查「红点为什么消失了」时
    -- 唯一能查的线索；两者并存则必然出现 read=1 而 read_at 为空的脏数据。
    `read_at`    datetime(3)              DEFAULT NULL COMMENT '已读时刻；NULL 表示未读',
    `created_at` datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),

    -- ===================================================================
    -- 幂等的全部保证就在这一行索引上
    -- ===================================================================
    --
    -- 领域事件是**至少一次**投递的：总线重启后的补投、worker 可见性超时导致的
    -- 重复执行、将来换成 MQ 后的重投，都会让同一个事件到达通知处理器不止一次。
    -- 没有这条索引，用户会为同一个分析任务收到三条一模一样的「分析完成」。
    --
    -- 应用层刻意**没有**「先查这个 dedupe_key 在不在」这一步：那是 TOCTOU，
    -- 两次重放并发进来时，先查再插的两边都会查到「还没有」，然后双双插入。
    -- 唯一索引是并发下唯一真正成立的保证——它由数据库在行级别串行执行，
    -- 无论多少个副本同时投递，第二条必然撞键。仓储把 1062 翻成 AlreadyExists，
    -- 事件处理器据此把重放当成功，整条链路因此对重复投递免疫。
    --
    -- user_id 放在前面而不是 dedupe_key：同一个来源事件可能要通知多个人
    -- （将来的协作场景），去重的粒度是「每人一条」而不是「全局一条」。
    UNIQUE KEY `uk_notifications_user_dedupe` (`user_id`, `dedupe_key`),

    -- ===================================================================
    -- 让未读红点这个高频查询变便宜的索引
    -- ===================================================================
    --
    -- SELECT COUNT(*) WHERE user_id = ? AND read_at IS NULL 是全站最高频的查询之一：
    -- 每次进首页、每次轮询都会打一次。它正好命中本索引的前两列，
    -- 因此整个计数只在索引上完成，一行数据都不必回表。
    --
    -- 第三列 created_at 是给未读列表用的：WHERE user_id=? AND read_at IS NULL
    -- ORDER BY created_at DESC 可以直接吃着索引顺序读，不需要额外的 filesort。
    -- 三列合成一个复合索引而不是拆成三个单列索引：单列索引下 MySQL 只能用其一，
    -- 剩下的筛选与排序还是得回表加排序，通知一多翻页就会肉眼可见地变慢。
    KEY `idx_notifications_user_read_created` (`user_id`, `read_at`, `created_at` DESC)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `notifications`;
-- +goose StatementEnd
