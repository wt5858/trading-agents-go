package repositories

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/entities"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/repositories/dtos"
	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/system_config/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
)

// SystemSettingRepository 持久化 SystemSetting 聚合。
type SystemSettingRepository struct {
	db *gorm.DB
}

func NewSystemSettingRepository(db *gorm.DB) *SystemSettingRepository {
	return &SystemSettingRepository{db: db}
}

func (repo *SystemSettingRepository) GetDb() *gorm.DB { return repo.db }

// Upsert 写入一项配置：不存在就插入，存在就更新。
//
// 用 ON DUPLICATE KEY UPDATE 而不是「先查存在性、再决定 INSERT 还是 UPDATE」：
// 后者在两次调用之间留了一个窗口，并发保存同一个键时会有一方拿到 1062 失败，
// 而这本该是一次正常的幂等保存。单条 upsert 语句没有这个窗口。
//
// created_at 不在更新列里：它记录的是这一项第一次出现的时间，
// 每次保存都刷新的话，「这个配置项是什么时候被加进来的」这个审计信息就永远丢了。
func (repo *SystemSettingRepository) Upsert(ctx context.Context, s *entities.SystemSetting) error {
	dto := dtos.FromDomainSystemSetting(s)
	err := repo.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "setting_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"scope", "value", "description", "updated_by", "updated_at",
		}),
	}).Create(dto).Error
	if err != nil {
		return translate(err, "保存配置项")
	}
	return nil
}

func (repo *SystemSettingRepository) FindByKey(ctx context.Context, key value_objects.SettingKey) (*entities.SystemSetting, error) {
	var dto dtos.SystemSettingDto
	if err := repo.db.WithContext(ctx).
		Where("setting_key = ?", key.String()).
		First(&dto).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, custom_errors.NotFound("配置项不存在: %s", key.String())
		}
		return nil, translate(err, "查询配置项")
	}
	return dto.ToDomain(), nil
}

func (repo *SystemSettingRepository) ListByScope(ctx context.Context, scope value_objects.SettingScope) ([]*entities.SystemSetting, error) {
	var rows []*dtos.SystemSettingDto
	if err := repo.db.WithContext(ctx).
		Where("scope = ?", scope.String()).
		Order("setting_key ASC").
		Find(&rows).Error; err != nil {
		return nil, translate(err, "按域查询配置项")
	}
	return dtos.ToDomainSystemSettings(rows), nil
}

// LoadAll 一次性取出全部配置项，按键索引。
//
// 这是启动期覆盖（overlay）的数据来源：装配根先用文件/环境变量里的默认值建好配置，
// 再用这里返回的值逐项覆盖运行期可调的那部分。做成一次全量查询而不是
// 让每个消费方各查各的：几十个消费方各发一条 SELECT 是典型的循环内 RPC，
// 而且它们会在不同时刻读到不同版本的配置，启动出来的就是一个自相矛盾的系统。
func (repo *SystemSettingRepository) LoadAll(ctx context.Context) (map[string]*entities.SystemSetting, error) {
	var rows []*dtos.SystemSettingDto
	if err := repo.db.WithContext(ctx).Order("setting_key ASC").Find(&rows).Error; err != nil {
		return nil, translate(err, "加载全部配置项")
	}
	out := make(map[string]*entities.SystemSetting, len(rows))
	for _, r := range rows {
		out[r.Key] = r.ToDomain()
	}
	return out, nil
}

func (repo *SystemSettingRepository) Delete(ctx context.Context, key value_objects.SettingKey) error {
	res := repo.db.WithContext(ctx).
		Where("setting_key = ?", key.String()).
		Delete(&dtos.SystemSettingDto{})
	if res.Error != nil {
		return translate(res.Error, "删除配置项")
	}
	if res.RowsAffected == 0 {
		return custom_errors.NotFound("配置项不存在: %s", key.String())
	}
	return nil
}
