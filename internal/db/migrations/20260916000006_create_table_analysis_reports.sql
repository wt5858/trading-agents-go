-- +goose Up
-- +goose StatementBegin
-- 分析报告表。
--
-- 正文（title / summary / sections）是中文，而且大模型产出的结论里常带 emoji，
-- 所以字符集必须是 utf8mb4 而不是 utf8——后者只有 3 字节，emoji 会直接写失败。
CREATE TABLE `analysis_reports`
(
    `id`           varchar(40)     NOT NULL,

    -- task_id 上的唯一索引是本上下文最重要的一条约束，它同时干两件事：
    --  1. 表达不变式：一次分析只该产出一份报告。同一个任务出现第二份，
    --     意味着任务被重复消费了——那是必须暴露出来的 bug，不是可以静默覆盖的小事。
    --  2. 提供幂等：领域事件是至少一次投递的。重放时第二次 INSERT 会撞唯一键，
    --     仓储把 1062 翻成 AlreadyExists，事件处理器据此认定「已经做过了」。
    --     整条链路因此不需要任何「先查再插」——那种写法在并发重放下根本挡不住重复。
    `task_id`      varchar(40)     NOT NULL,

    `user_id`      bigint unsigned NOT NULL,
    `created_at`   datetime(3)     NOT NULL,

    -- symbol / market / trade_date 是从聚合拍平出来的列，供运维排查与看板聚合
    -- （「本月 600519 出了多少份报告」不该去 JSON 里捞）。
    `symbol`       varchar(16)     NOT NULL DEFAULT '',
    `market`       varchar(8)      NOT NULL DEFAULT '',
    `symbol_raw`   varchar(24)     NOT NULL DEFAULT '' COMMENT '数据源原始写法（600519.SH）',
    `trade_date`   char(10)        NOT NULL DEFAULT '' COMMENT 'YYYY-MM-DD',

    `title`        varchar(128)    NOT NULL DEFAULT '',
    `summary`      text            NULL,

    -- 章节整体读写，用 JSON 列而不是另开一张 report_sections 表：
    -- 章节是值对象，没有独立身份，永远随报告整体产生、整体消失，也从不被单独更新。
    -- 给它一张表就等于给了它一个它不该有的生命周期，还会让「读一份报告」
    -- 从一条主键查询变成一次 join + 一次排序。
    `sections`     json            NULL COMMENT '章节数组，含生成时固化的 order；正文含中文与 emoji',

    -- 下面六列是报告生成那一刻的既成事实，一律定点数落库、读路径直接取。
    -- 任何形式的读时重算都会让同一份报告在两次刷新之间给出不同的数字。
    `action`       varchar(16)     NOT NULL DEFAULT '' COMMENT 'buy / hold / sell 等最终建议',
    `confidence`   decimal(5, 4)   NOT NULL DEFAULT 0 COMMENT '置信度 0.0000 - 1.0000',
    `risk_score`   decimal(4, 2)   NOT NULL DEFAULT 0 COMMENT '风险评分 0.00 - 10.00',
    `target_price` decimal(18, 4)  NOT NULL DEFAULT 0 COMMENT '目标价；用定点数而非 float，避免看板聚合累积误差',
    `stop_loss`    decimal(18, 4)  NOT NULL DEFAULT 0 COMMENT '止损价',
    `position`     decimal(6, 2)   NOT NULL DEFAULT 0 COMMENT '建议仓位百分比 0.00 - 100.00',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_reports_task_id` (`task_id`),
    -- (user_id, created_at DESC) 覆盖唯一的热查询：用户报告列表按时间倒序翻页。
    -- 复合索引而不是两个单列索引：单列索引下 MySQL 只能用其一，
    -- 剩下的排序要落到 filesort，报告一多翻页就会肉眼可见地变慢。
    KEY `idx_reports_user_created` (`user_id`, `created_at` DESC)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `analysis_reports`;
-- +goose StatementEnd
