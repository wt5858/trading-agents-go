-- +goose Up
-- +goose StatementBegin
-- 运行期可调的系统参数。
--
-- 与 llm_providers 同理，基础设施参数（数据库/Redis/Mongo 连接、端口、JWT 密钥）
-- 绝不能作为这里的一行存在：这张表住在 MySQL 里，把连库参数存进来，
-- 一行写坏的配置就会让服务连不上存着这行配置的库。
--
-- 主键是配置键本身，没有自增 ID：同一个键存成两行是不可能出现的状态，
-- 那就让数据库来保证，而不是靠应用层小心。
CREATE TABLE `system_settings`
(
    -- 列名是 setting_key 而不是 key：KEY 是 MySQL 保留字，
    -- 叫 key 的话每一条手写 SQL 都得记得加反引号，迟早有人忘记。
    `setting_key` varchar(128)    NOT NULL,
    `scope`       varchar(16)     NOT NULL COMMENT '配置分组，管理界面按它分区展示',
    -- text 而不是 varchar：配置值里出现一段 JSON（比如某个数据源的字段映射）
    -- 是完全可能的，被长度截断的配置比直接报错更难发现。
    `value`       text            NULL COMMENT '统一存字符串，类型解释由读取方负责',
    `description` varchar(255)    NOT NULL DEFAULT '' COMMENT '面向运维的说明，含中文',
    `updated_by`  bigint unsigned NOT NULL DEFAULT 0 COMMENT 'identity 上下文的用户 ID；0 表示系统写入，刻意不加外键',
    `created_at`  datetime(3)     NOT NULL,
    `updated_at`  datetime(3)     NOT NULL,
    PRIMARY KEY (`setting_key`),
    KEY `idx_system_settings_scope` (`scope`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `system_settings`;
-- +goose StatementEnd
