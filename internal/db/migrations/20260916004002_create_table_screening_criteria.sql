-- +goose Up
-- +goose StatementBegin
-- 选股条件表，**子实体** Criterion 的落地形态。
--
-- 它与 screening_templates 属于同一个聚合：两张表由 ScreeningTemplateRepository.Save
-- 在同一个事务里一起写。这与「不同聚合根之间不共享事务」并不矛盾——
-- 被禁止的是把两个**聚合根**绑进一个事务，而根与它的子实体本来就是一个一致性单元。
--
-- # 为什么没有指向 screening_templates 的外键
--
-- 删除模板时确实必须连它的条件一起删，但那件事由仓储在事务里用一条显式的
-- DELETE 完成，而不是交给 ON DELETE CASCADE。理由是级联**看不见**：
-- 读代码的人不会知道还有一张表被清空了，排查数据丢失时也想不到去翻建表语句。
-- 仓储本来就持有事务，把这一步写成一条摆在明面上的语句成本为零。
CREATE TABLE `screening_criteria`
(
    `id`          bigint unsigned NOT NULL AUTO_INCREMENT,
    `template_id` bigint unsigned NOT NULL COMMENT '所属模板；子实体只能经由聚合根写入，没有独立的仓储',

    -- field 存的是**白名单里的字段名**（pe / roe / industry …），不是任意字符串。
    --
    -- 这一列是本上下文的安全关键列：它的值最终会被用来查
    -- value_objects.fieldRegistry，查出的物理列名才会进入 SQL / BSON。
    -- 库里即便因为迁移脚本或误操作存进了白名单之外的值，读路径也只会得到
    -- 一个零值字段，随后被聚合的 Validate 与查询构造器拒绝——
    -- 绝不会有任何一条路径把这一列的内容拼进语句文本。
    `field`       varchar(32)     NOT NULL COMMENT '可筛选字段名，必须在领域层白名单内',
    `operator`    varchar(16)     NOT NULL COMMENT 'gt/gte/lt/lte/eq/ne/between/in/not_in',

    -- 取值存 JSON 数组，因为个数随比较符变化（eq 一个、between 两个、in 若干）。
    --
    -- 为什么不拆成 value_min / value_max / value_list 三列：那会让「个数与比较符
    -- 相符」这条不变式在表结构里表达成一组互相排斥的 NULL 约束，
    -- 而它在领域层已经由 CompareOperator.Arity 判过了。
    -- 为什么不存成逗号分隔串：行业名里就带逗号，而 JSON 的转义是现成的。
    -- 全部存成字符串、不区分数值与文本：数值的解析口径归属领域层的条件构造器，
    -- 库里存两套类型只会让那个口径出现第二个定义点。
    `values_json` json                     DEFAULT NULL COMMENT '取值数组，形如 ["10"] 或 ["10","20"]',

    -- sort_order 由聚合根维护，恒为模板内 0..n-1 的稠密连续序列。
    -- 这里**刻意不加唯一约束**：重排时必然出现两行同号的中间态（交换两条条件的位置），
    -- 加了唯一约束就得引入临时负数之类的把戏，白白把一条批量 upsert 拆成多步。
    -- 稠密连续是聚合根的不变式，数据库只负责按它排序。
    `sort_order`  int             NOT NULL DEFAULT 0 COMMENT '模板内排序，决定表单与结果表格的列序',

    `created_at`  datetime(3)     NOT NULL,
    `updated_at`  datetime(3)     NOT NULL,
    PRIMARY KEY (`id`),
    -- 「同一模板内 (字段, 比较符) 不重复」的数据库兜底。
    --
    -- 聚合根的 AddCriterion 已经判过一次，为什么还要这一道：内存判断只在
    -- **同一个聚合实例**内成立。同一个用户开两个标签页同时编辑同一个模板，
    -- 会加载出两个各自认为自己没重复的聚合实例。唯一索引是唯一挡得住那一幕的东西。
    --
    -- 为什么键里不含 values_json：同一字段上「pe > 10」和「pe > 20」是同一条规则的
    -- 两个版本，并存时后者恒覆盖前者，前者纯属噪音；而「pe > 10」与「pe < 30」
    -- 比较符不同，是一个合法的区间表达，必须允许并存。
    UNIQUE KEY `uk_screening_criteria_template_field` (`template_id`, `field`, `operator`),
    -- 加载聚合时的热查询是 WHERE template_id IN (...) ORDER BY sort_order，
    -- 这个索引让定位和排序一次吃完，不需要额外的排序开销。
    KEY `idx_screening_criteria_template_sort` (`template_id`, `sort_order`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS `screening_criteria`;
-- +goose StatementEnd
