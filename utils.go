package main

import (
	"math"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

func normalizePrice(price decimal.Decimal, symbol *coinfactory.Symbol) decimal.Decimal {
	// Get decimal places and round to that precision
	ts, _ := symbol.Filters.Price.TickSize.Float64()
	places := int32(math.Log10(ts)) * -1
	return price.Round(places)
}

func normalizeQuantity(qty decimal.Decimal, symbol *coinfactory.Symbol) decimal.Decimal {
	// Get decimal places and round to that precision
	ss, _ := symbol.Filters.LotSize.StepSize.Float64()
	places := int32(math.Log10(ss)) * -1
	return qty.Round(places)
}

func isViable(data binance.SymbolTickerData) bool {
	return getSpread(data).GreaterThan(tradeFee.Mul(decimal.NewFromFloat(2)))
}

func getSpread(data binance.SymbolTickerData) decimal.Decimal {
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	if askPercent.LessThan(bidPercent) {
		return askPercent
	}
	return bidPercent
}

func validateOrderPair(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) bool {
	// Bail on zero value trades
	if buy.Quantity.Equals(decimal.Zero) || sell.Quantity.Equals(decimal.Zero) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
		}).Info("Skipping zero value order")
		return false
	}

	// Validate the orders are still viable
	bv := buy.Price.Mul(buy.Quantity)
	sv := sell.Price.Mul(sell.Quantity)
	tc := bv.Mul(tradeFee).Add(sv.Mul(tradeFee))
	r := sv.Sub(bv)

	// Bail out if we make no money
	if r.LessThan(tc) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
		}).Info("Skipping negative return order")
		return false
	}

	// Bail out if not nominal order
	mn := binance.GetSymbol(buy.Symbol).Filters.MinimumNotional.MinNotional

	if bv.LessThan(mn) || sv.LessThan(mn) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
			"mn":   mn,
		}).Info("Skipping sub-notional order")
		return false
	}

	return true
}

func executeTestOrders(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) {
	var err error
	err = cf.GetOrderManager().AttemptTestOrder(buy)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
	}

	err = cf.GetOrderManager().AttemptTestOrder(sell)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
	}
}

func executeOrders(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) (*coinfactory.Order, *coinfactory.Order, error) {
	buyOrder, err := cf.GetOrderManager().AttemptOrder(buy)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
		return &coinfactory.Order{}, &coinfactory.Order{}, err
	}

	sellOrder, err0 := cf.GetOrderManager().AttemptOrder(sell)
	if err0 != nil {
		log.WithError(err0).Error("Could not place order! Cancelling previous order")
		// Cancel buy order
		err1 := cf.GetOrderManager().CancelOrder(buyOrder)
		if err1 != nil {
			log.WithError(err1).Error("Could not cancel previous order!")
			return &coinfactory.Order{}, &coinfactory.Order{}, err1
		}
		return &coinfactory.Order{}, &coinfactory.Order{}, err0
	}

	return buyOrder, sellOrder, nil
}

func logTicker(data binance.SymbolTickerData) {
	// askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	// bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
}
