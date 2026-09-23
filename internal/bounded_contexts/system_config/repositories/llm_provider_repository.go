// Package repositories 是配置中心唯一知道 GORM 存在、也唯一允许开启事务的一层。
//
// DTO 不出本包：上层拿到的永远是 entities 里的聚合，
// 这样换存储引擎不会波及任何一个 domain_service。
package repositories

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	shared_vo "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// LLMProviderRepository 持久化 LLMProviderConfig 聚合。
type LLMProviderRepository struct {
	db *gorm.DB
}

func NewLLMProviderRepository(db *gorm.DB) *LLMProviderRepository {
	return &LLMProviderRepository{db: db}
}

// Create 插入一条供应商配置。
//
// 刻意不做「这个名字是否已存在」的前置查询：那是一个 TOCTOU 窗口
// （两个并发请求都查到「没占用」，然后都插入），也白白多一次往返。
// uk_llm_providers_name 这个唯一索引才是真正的保证，
// 冲突时驱动返回 1062，translate 把它翻成 AlreadyExists。
func (repo *LLMProviderRepository) Create(ctx context.Context, p *entities.LLMProviderConfig) error {
	dto := dtos.FromDomainLLMProvider(p)
	if err := repo.db.WithContext(ctx).Create(dto).Error; err != nil {
		return translate(err, "创建供应商配置")
	}
	// 回填自增 ID：调用方紧接着要用它发领域事件。
	p.ID = dto.ID
	return nil
}

// Update 全量更新可变列。
//
// api_key 也在更新列里：轮换密钥走的是同一条写路径。代价是并发编辑会互相覆盖
// （本仓库整体没有乐观锁），但配置中心是低频的管理操作，为它引入版本号
// 不划算——启用/停用这类真正有竞态风险的操作走的是下面的条件更新。
func (repo *LLMProviderRepository) Update(ctx context.Context, p *entities.LLMProviderConfig) error {
	dto := dtos.FromDomainLLMProvider(p)
	res := repo.db.WithContext(ctx).Model(&dtos.LLMProviderDto{}).Where("id = ?", dto.ID).Updates(map[string]any{
		"base_url":   dto.BaseURL,
		"api_key":    dto.APIKey,
		"models":     dto.Models,
		"priority":   dto.Priority,
		"updated_at": dto.UpdatedAt,
	})
	if res.Error != nil {
		return translate(res.Error, "更新供应商配置")
	}
	// RowsAffected == 0 有歧义：可能行不存在，也可能值恰好没变。
	// 只在这个分支上多付一次查询，正常路径仍然只有一条语句。
	if res.RowsAffected == 0 {
		exists, err := repo.existsByID(ctx, dto.ID)
		if err != nil {
			return err
		}
		if !exists {
			return custom_errors.NotFound("供应商配置不存在: %d", dto.ID)
		}
	}
	return nil
}

func (repo *LLMProviderRepository) Delete(ctx context.Context, id uint64) error {
	res := repo.db.WithContext(ctx).Delete(&dtos.LLMProviderDto{}, id)
	if res.Error != nil {
		return translate(res.Error, "删除供应商配置")
	}
	if res.RowsAffected == 0 {
		return custom_errors.NotFound("供应商配置不存在: %d", id)
	}
	return nil
}

func (repo *LLMProviderRepository) FindByID(ctx context.Context, id uint64) (*entities.LLMProviderConfig, error) {
	var dto dtos.LLMProviderDto
	if err := repo.db.WithContext(ctx).First(&dto, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("供应商配置不存在: %d", id)
		}
		return nil, translate(err, "查询供应商配置")
	}
	return dto.ToDomain(), nil
}

func (repo *LLMProviderRepository) FindByName(ctx context.Context, name value_objects.ProviderName) (*entities.LLMProviderConfig, error) {
	var dto dtos.LLMProviderDto
	if err := repo.db.WithContext(ctx).Where("name = ?", name.String()).First(&dto).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("供应商配置不存在: %s", name.String())
		}
		return nil, translate(err, "查询供应商配置")
	}
	return dto.ToDomain(), nil
}

// ListEnabled 取全部启用中的供应商，按优先级降序。
//
// 只过滤 enabled，不在 SQL 里判断「有没有密钥、有没有模型」：
// 可用性是 Usable() 这个业务不变式，把它拆成一半 SQL 一半 Go，
// 两处早晚会漂移（比如 ollama 的例外只在其中一处生效）。
// SQL 负责取数，判定留给实体。
func (repo *LLMProviderRepository) ListEnabled(ctx context.Context) ([]*entities.LLMProviderConfig, error) {
	var rows []*dtos.LLMProviderDto
	if err := repo.db.WithContext(ctx).
		Where("enabled = ?", true).
		Order("priority DESC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, translate(err, "查询启用中的供应商")
	}
	return dtos.ToDomainLLMProviders(rows), nil
}

func (repo *LLMProviderRepository) List(ctx context.Context, page shared_vo.Page) ([]*entities.LLMProviderConfig, int64, error) {
	q := repo.db.WithContext(ctx).Model(&dtos.LLMProviderDto{})

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, translate(err, "统计供应商配置")
	}
	if total == 0 {
		// 返回分配好的空切片而不是 nil：nil 切片会被序列化成 JSON 的 null，
		// 而本项目其余分页仓储一律给 []。
		return []*entities.LLMProviderConfig{}, 0, nil
	}

	var rows []*dtos.LLMProviderDto
	if err := q.Order("priority DESC, id ASC").
		Offset(page.Offset()).Limit(page.Limit()).
		Find(&rows).Error; err != nil {
		return nil, 0, translate(err, "查询供应商配置列表")
	}
	return dtos.ToDomainLLMProviders(rows), total, nil
}

// Enable 把供应商置为启用。
func (repo *LLMProviderRepository) Enable(ctx context.Context, p *entities.LLMProviderConfig) error {
	return repo.setEnabled(ctx, p, true)
}

// Disable 把供应商置为停用。
func (repo *LLMProviderRepository) Disable(ctx context.Context, p *entities.LLMProviderConfig) error {
	return repo.setEnabled(ctx, p, false)
}

// setEnabled 用条件更新完成启停，把「检查当前状态」和「改状态」合并成一条语句。
//
// 为什么不是「先查 enabled 再更新」：那是典型的 check-then-act。
// 两个管理员同时点「停用」，都读到 enabled=true，都执行更新，
// 第二次其实是在覆盖一个已经完成的操作，却同样返回成功——
// 调用方以为自己的操作生效了，实际发生了什么无从分辨。
// 把当前状态写进 WHERE，数据库会告诉我们「这一步是不是真的由你完成的」：
// RowsAffected==1 表示状态确实由你翻转，0 表示前置条件不成立。
//
// 附带的好处是幂等：重试一次成功的停用只会影响 0 行，不会白白刷新 updated_at。
func (repo *LLMProviderRepository) setEnabled(ctx context.Context, p *entities.LLMProviderConfig, want bool) error {
	action := "启用供应商"
	if !want {
		action = "停用供应商"
	}

	err := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&dtos.LLMProviderDto{}).
			Where("id = ? AND enabled = ?", p.ID, !want).
			Updates(map[string]any{"enabled": want, "updated_at": p.UpdatedAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}

		// 影响 0 行有歧义，在同一个事务里读一次，让调用方拿到可行动的错误。
		var current dtos.LLMProviderDto
		if err := tx.First(&current, p.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return custom_errors.NotFound("供应商配置不存在: %d", p.ID)
			}
			return err
		}
		if current.Enabled == want {
			if want {
				return custom_errors.Conflict("供应商已处于启用状态: %s", current.Name)
			}
			return custom_errors.Conflict("供应商已处于停用状态: %s", current.Name)
		}
		return custom_errors.Conflict("%s失败：状态已被其他操作变更，请刷新后重试", action)
	})
	return asDomainError(err, action)
}

func (repo *LLMProviderRepository) existsByID(ctx context.Context, id uint64) (bool, error) {
	var n int64
	if err := repo.db.WithContext(ctx).Model(&dtos.LLMProviderDto{}).
		Where("id = ?", id).Count(&n).Error; err != nil {
		return false, translate(err, "查询供应商配置")
	}
	return n > 0, nil
}
