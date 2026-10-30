package dtos

import (
	vo "github.com/wt5858/trading-agents-go/internal/bounded_contexts/stock/value_objects"
	shared "github.com/wt5858/trading-agents-go/internal/domain_kernel/value_objects"
)

type MarketContextDTO struct {
	codeColumns `bson:",inline"`
	Scope       string `bson:"scope"`
	Reference   string `bson:"reference"`
	AsOf        string `bson:"as_of"`
	AvailableOn string `bson:"available_on"`
	Source      string `bson:"source"`
	Data        string `bson:"data"`
}

func FromDomainMarketContext(v vo.MarketContext) MarketContextDTO {
	return MarketContextDTO{codeColumnsOf(v.Code), v.Scope, v.Reference, v.AsOf.String(), v.AvailableOn.String(), v.Source, v.Data}
}
func (v MarketContextDTO) ToDomain() vo.MarketContext {
	return vo.MarketContext{Code: v.codeColumns.toDomain(), Scope: v.Scope, Reference: v.Reference, AsOf: shared.MustTradeDate(v.AsOf), AvailableOn: shared.MustTradeDate(v.AvailableOn), Source: v.Source, Data: v.Data}
}
