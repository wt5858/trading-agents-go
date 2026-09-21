-- +goose Up
-- +goose StatementBegin
-- 平台账号表。
--
-- created_at / updated_at 刻意不给数据库默认值：DTO 上 autoCreateTime / autoUpdateTime
-- 都是 false，时间由聚合根自己维护。库里一旦补上 CURRENT_TIMESTAMP，
-- 「导入存量账号」就会被静默改写成导入时刻，而那种错位只有在对账时才会暴露。
CREATE TABLE `users`
(
    `id`               bigint unsigned NOT NULL AUTO_INCREMENT,
    `username`         varchar(32)     NOT NULL,
    `email`            varchar(128)             DEFAULT NULL,
    `password_hash`    varchar(128)    NOT NULL COMMENT 'bcrypt 哈希，永不存明文',
    `role`             varchar(16)     NOT NULL DEFAULT 'user' COMMENT 'admin / user',
    `active`           tinyint(1)      NOT NULL DEFAULT 1,
    `preferences`      json            NULL COMMENT '偏好整体读写；为 NULL 或解析失败时读路径退回默认偏好',
    `concurrent_limit` bigint          NOT NULL DEFAULT 0 COMMENT '并发分析额度覆盖值；0 表示不覆盖，回落到系统默认',
    `last_login_at`    datetime(3)              DEFAULT NULL,
    `created_at`       datetime(3)     NOT NULL,
    `updated_at`       datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 用户名唯一不只是防手滑：登录按用户名查，重名会让「登录成了谁」变成一次赌博。
    UNIQUE KEY `uk_users_username` (`username`),
    KEY `idx_users_email` (`email`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `users`;
-- +goose StatementEnd
