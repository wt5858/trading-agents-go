package repositories

import (
	"context"

	"github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/repositories/dtos"
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (r *MarketDataRepository) SaveMarketContexts(ctx context.Context, items []vo.MarketContext) error {
	if r.db == nil {
		return custom_errors.Unavailable("行情数据库未配置")
	}
	var models []mongo.WriteModel
	for _, v := range items {
		if err := v.Validate(); err != nil {
			return err
		}
		d := dtos.FromDomainMarketContext(v)
		models = append(models, mongo.NewUpdateOneModel().SetFilter(bson.M{"market": v.Code.Market.String(), "symbol": v.Code.Symbol, "scope": v.Scope, "reference": v.Reference, "as_of": v.AsOf.String(), "available_on": v.AvailableOn.String(), "source": v.Source}).SetUpdate(bson.M{"$set": d}).SetUpsert(true))
	}
	return r.bulkWrite(ctx, "market_contexts", models, "板块/指数背景")
}
func (r *MarketDataRepository) MarketContexts(ctx context.Context, code shared.StockCode, asOf shared.TradeDate) ([]vo.MarketContext, error) {
	if r.db == nil {
		return nil, custom_errors.Unavailable("行情数据库未配置")
	}
	if code.IsZero() || asOf.IsZero() {
		return nil, custom_errors.Invalid("背景查询需要明确标的和日期")
	}
	cur, err := r.db.Collection("market_contexts").Find(ctx, bson.M{"market": code.Market.String(), "symbol": code.Symbol, "as_of": bson.M{"$lte": asOf.String()}, "available_on": bson.M{"$lte": asOf.String()}}, options.Find().SetSort(bson.D{{Key: "as_of", Value: -1}, {Key: "available_on", Value: -1}, {Key: "reference", Value: 1}, {Key: "source", Value: 1}}).SetLimit(20))
	if err != nil {
		return nil, custom_errors.Internal("读取市场背景失败").Wrap(err)
	}
	defer cur.Close(ctx)
	var rows []dtos.MarketContextDTO
	if err := cur.All(ctx, &rows); err != nil {
		return nil, custom_errors.Internal("解析市场背景失败").Wrap(err)
	}
	var out []vo.MarketContext
	for _, d := range rows {
		v := d.ToDomain()
		if v.VisibleTo(code, asOf) {
			out = append(out, v)
		}
	}
	return out, nil
}
