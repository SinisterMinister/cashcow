package main

import (
	"math"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
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
		}).Debug("Skipping zero value order")
		return false
	}

	// Validate the orders are still viable
	baseAssetFee := buy.Quantity.Mul(takerFee)
	quoteAssetFee := sell.Quantity.Mul(sell.Price).Mul(makerFee)
	baseReturn := buy.Quantity.Sub(sell.Quantity)
	quoteReturn := sell.Price.Mul(sell.Quantity).Sub(buy.Price.Mul(buy.Quantity))
	combinedReturn := baseReturn.Mul(sell.Price).Add(quoteReturn)
	combinedFee := baseAssetFee.Mul(buy.Price).Add(quoteAssetFee)

	if combinedReturn.LessThan(combinedFee) {
		// Bail out if we make no money
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
		}).Debug("Skipping negative return order")
		return false
	}
	bv := buy.Price.Mul(buy.Quantity)
	sv := sell.Price.Mul(sell.Quantity)

	// Bail out if not nominal order
	mn := binance.GetSymbol(buy.Symbol).Filters.MinimumNotional.MinNotional

	if bv.LessThan(mn) || sv.LessThan(mn) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
			"mn":   mn,
		}).Debug("Skipping sub-notional order")
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

func executeOrdersSequentially(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) (*coinfactory.Order, *coinfactory.Order, error) {
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

func adjustOrdersQuantityBasedOnAvailableFunds(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest, symbol *coinfactory.Symbol) {
	// Check balances and adjust quantity if necessary
	quoteBalance := cf.GetBalanceManager().GetUsableBalance(symbol.QuoteAsset)
	baseBalance := cf.GetBalanceManager().GetUsableBalance(symbol.BaseAsset)

	log.WithFields(log.Fields{
		symbol.BaseAsset:  baseBalance,
		symbol.QuoteAsset: quoteBalance,
	}).Debug("Wallet balances")

	if buyOrder.Quantity.Mul(buyOrder.Price).GreaterThan(quoteBalance) {
		adjPercent := quoteBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(buyOrder.Price).Div(buyOrder.Quantity)
		buyOrder.Quantity = buyOrder.Quantity.Mul(adjPercent)
		sellOrder.Quantity = sellOrder.Quantity.Mul(adjPercent)
	}

	if sellOrder.Quantity.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(sellOrder.Quantity)
		buyOrder.Quantity = buyOrder.Quantity.Mul(adjPercent)
		sellOrder.Quantity = sellOrder.Quantity.Mul(adjPercent)
	}
}

func normalizeOrders(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest, symbol *coinfactory.Symbol) {
	// Normalize the price
	buyOrder.Price = normalizePrice(buyOrder.Price, symbol)
	sellOrder.Price = normalizePrice(sellOrder.Price, symbol)

	// Normalize quantities
	buyOrder.Quantity = normalizeQuantity(buyOrder.Quantity, symbol)
	sellOrder.Quantity = normalizeQuantity(sellOrder.Quantity, symbol)
}

func isOrderOpen(o *coinfactory.Order) (open bool) {
	// Remove any closed orders
	open = true
	switch o.GetStatus().Status {
	// These are the open cases
	case "NEW":
	case "PARTIALLY_FILLED":
	case "PENDING_CANCEL":
	case "":
	// The rest are closed
	default:
		open = false
	}
	return
}

func isOrderStale(o *coinfactory.Order) bool {
	return o.GetAge().Nanoseconds() > viper.GetDuration("followtheleaderprocessor.markOrderAsStaleAfter").Nanoseconds()
}

func isOrderExpired(o *coinfactory.Order) bool {
	return o.GetAge().Nanoseconds() > viper.GetDuration("followtheleaderprocessor.cancelOrderAfter").Nanoseconds()
}
