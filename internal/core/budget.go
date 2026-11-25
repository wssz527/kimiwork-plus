package core

import (
	"errors"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// NormalizeBudget validates one budget document and returns its canonical form.
// A nil input clears the document entirely, which is what the panel sends when
// a user removes every limit.
func NormalizeBudget(value any) (map[string]any, error) {
	policy, err := protocol.ParseBudget(value)
	if err != nil {
		return nil, errors.New("invalid_budget")
	}
	if policy.IsZero() {
		return nil, nil
	}
	return policy.Document(), nil
}

func ModelPrices(rows []any) (map[string]protocol.ModelPrice, error) {
	return modelPrices(rows, "")
}

func modelPrices(rows []any, currency string) (map[string]protocol.ModelPrice, error) {
	out := map[string]protocol.ModelPrice{}
	for _, value := range rows {
		row := Object(value)
		id := String(row["id"])
		if id == "" {
			continue
		}
		price, err := protocol.ParseModelPrice(row["pricing"])
		if err != nil {
			return nil, errors.New("invalid_price")
		}
		if price != nil {
			out[id] = *price
			continue
		}
		if currency == "" {
			continue
		}
		hint := modelCatalogHint(row)
		if hint == nil || hint.ReferencePricing == nil || hint.ReferencePricing.Currency != currency {
			continue
		}
		reference := hint.ReferencePricing
		out[id] = protocol.ModelPrice{InputPerMillion: &reference.Input, OutputPerMillion: &reference.Output, CachedInputPerMillion: reference.CacheRead}
	}
	return out, nil
}

// PriceTable builds the cost table the gateway prices requests with. It is nil
// when the installation prices nothing, which keeps an untouched install on the
// behaviour it had before budget support.
func PriceTable(d Document) (*protocol.PriceTable, error) {
	policy, err := protocol.ParseBudget(d["budget"])
	if err != nil {
		return nil, errors.New("invalid_budget")
	}
	if policy.Currency == "" {
		return nil, nil
	}
	prices, err := modelPrices(Rows(d["models"]), policy.Currency)
	if err != nil {
		return nil, err
	}
	return protocol.NewPriceTable(policy.Currency, prices), nil
}

// modelConnections lists every connection a model row names: its own and each
// explicitly configured failover candidate. It is what keeps a connection in use
// while any model can reach it, however it reaches it.
func modelConnections(row map[string]any) []string {
	out := []string{}
	if connection := String(row["connectionId"]); connection != "" {
		out = append(out, connection)
	}
	for _, item := range Rows(row["failover"]) {
		if connection := String(Object(item)["connectionId"]); connection != "" {
			out = append(out, connection)
		}
	}
	return out
}
