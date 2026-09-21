-- +goose Up
-- +goose StatementBegin
-- 大模型供应商配置。只装运行期可调参数。
--
-- 数据库连接、Redis 地址、Mongo URI、HTTP 端口、JWT 密钥这些基础设施参数
-- 永远留在 config/config.go 的文件与环境变量里，绝不能在这里加列：
-- 这张表本身就住在 MySQL 里，把 mysql.password 存进去意味着一行写坏的配置
-- 会让服务连不上存着这行配置的库——一个自己锁死自己的死结。
--
-- 关于 api_key：密钥目前是明文落库的。库在内网、有独立账号体系，当前阶段可接受，
-- 但这不等于安全：任何一次 SELECT *、一次慢查询日志、一次备份外泄，密钥就跟着出去了。
-- 上生产前必须补静态加密（KMS 信封加密，本列改存密文）。
-- 在这里写清楚而不是默默留个 TODO，是因为「以为已经加密了」比「知道还没加密」危险得多。
CREATE TABLE `llm_providers`
(
    `id`         bigint unsigned NOT NULL AUTO_INCREMENT,
    `name`       varchar(32)     NOT NULL,
    `kind`       varchar(32)     NOT NULL COMMENT '协议族：openai / anthropic / google 等，决定用哪个客户端',
    `base_url`   varchar(255)    NOT NULL DEFAULT '' COMMENT '自定义接入点；空串表示用该 kind 的官方地址',
    `api_key`    varchar(512)    NOT NULL DEFAULT '' COMMENT '当前为明文，上生产前必须改为 KMS 信封加密后的密文',
    `models`     json            NULL COMMENT '可用模型名数组；为 NULL 或解析失败时该供应商被判为不可用，不混进路由表',
    `enabled`    tinyint(1)      NOT NULL DEFAULT 0,
    `priority`   bigint          NOT NULL DEFAULT 0 COMMENT '路由优先级，数值越大越优先（按 priority DESC 排序）',
    `created_at` datetime(3)     NOT NULL,
    `updated_at` datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 唯一索引不只是查询优化：它是「同名供应商只能有一个」这条不变式的实际执行者。
    -- 仓储不做「先查再插」，直接依赖它并把 1062 翻译成 AlreadyExists。
    UNIQUE KEY `uk_llm_providers_name` (`name`),
    -- 启用态的读取是热路径（每次装配路由都要扫一遍 `WHERE enabled = 1 ORDER BY priority DESC`），
    -- (enabled, priority) 让过滤和排序都走索引而不是全表扫 + filesort。
    KEY `idx_llm_providers_enabled_priority` (`enabled`, `priority`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `llm_providers`;
-- +goose StatementEnd
