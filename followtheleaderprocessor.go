package main

import (
	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Processor does stuff. Obviously.
type Processor interface {
	// Launch starts the various sub-processors of the processor
	Launch(stopChan <-chan bool)
}

// FollowTheLeaderProcessor processes various data streams and executes orders when it determines to do so.
type FollowTheLeaderProcessor struct {
	Symbol        *coinfactory.Symbol
	lastOrderPair *OrderPair
}

// Launch starts the various sub-processors of the processor
func (p *FollowTheLeaderProcessor) Launch(stopChan <-chan bool) {
	p.handleTickerStreamData(stopChan)
}

// handleTickerStreamData launches a goroutine that grabs a ticker stream and feeds the data
// to Processor.processTickerData.
func (p *FollowTheLeaderProcessor) handleTickerStreamData(stopChan <-chan bool) {
	go func(stopChan <-chan bool) {
		dataChan := p.Symbol.GetTickerStream(stopChan)
		for {
			// Bail out of the routine
			select {
			case <-stopChan:
				return
			default:
			}

			select {
			case <-stopChan:
				return
			case data := <-dataChan:
				p.processTickerData(data)
			}
		}
	}(stopChan)
}

// Process ticker data from Binanace
func (p *FollowTheLeaderProcessor) processTickerData(data binance.SymbolTickerData) {
	log.Debugf("Ticker data received: %s", data)
	if p.isReadyForData() {
		p.attemptOrder(data)
	}
}

// Check to see if the processor is ready to work with new data
func (p *FollowTheLeaderProcessor) isReadyForData() bool {
	// We're ready for data if there are no open orders or if the
	// last order pair has completed or stalled out
	if p.lastOrderPair == nil || p.lastOrderPair.IsDone() || p.lastOrderPair.IsStalled() {
		return true
	}

	// We're not ready to process any data
	return false
}

func (p *FollowTheLeaderProcessor) attemptOrder(data binance.SymbolTickerData) {
	// Figure out if there is an open order to follow or if we should just wing it
	var orderPair *OrderPair
	if p.lastOrderPair == nil || p.lastOrderPair.IsDone() {
		// No orders to follow, so lets wing it based on TRIX indicators
		firstRequest, secondRequest, err := p.buildTrixBasedOrderRequests(data)
		if err != nil {
			if e, ok := err.(UnstableTradeConditionError); ok {
				log.WithError(e).Debug("order bailed")
			} else {
				log.WithError(err).Error("could not build trix based order requests")
			}
			return
		}
		orderPair = buildOrderPair(firstRequest, secondRequest)
	} else {
		// There's already an open order to follow, so lets follow it
		firstRequest, secondRequest := p.buildLeaderBasedOrderRequests(data)
		orderPair = buildOrderPair(firstRequest, secondRequest)
	}

	orderPair.Execute()
	p.lastOrderPair = orderPair
}

func (p *FollowTheLeaderProcessor) buildLeaderBasedOrderRequests(data binance.SymbolTickerData) (firstRequest coinfactory.OrderRequest, secondRequest coinfactory.OrderRequest) {
	// Get latest stale order
	staleOrder := p.lastOrderPair.GetStalledOrder()

	// Build the appropriate trending order based on the stale order
	if staleOrder.Side == "BUY" {
		firstRequest, secondRequest = p.buildUpwardTrendingOrders(data)
	} else {
		secondRequest, firstRequest = p.buildDownwardTrendingOrders(data)
	}
	return firstRequest, secondRequest
}

func (p *FollowTheLeaderProcessor) buildTrixBasedOrderRequests(data binance.SymbolTickerData) (firstRequest coinfactory.OrderRequest, secondRequest coinfactory.OrderRequest, err error) {
	// Get trix indicator
	movingAverage, trix, err := p.Symbol.GetCurrentTrixIndicator("1m", 5)
	if err != nil {
		log.WithError(err).Error("could not get TRIX indicator")
		return
	}

	// If trix is positive, the price is increasing
	if trix > 0 {
		// If the current price is greater than the moving average, the trend should be continuing upward
		if data.CurrentClosePrice.GreaterThan(decimal.NewFromFloat(movingAverage)) {
			// Place upward trend orders
			firstRequest, secondRequest = p.buildUpwardTrendingOrders(data)
		} else { // Things have gotten too unpredictable. Bail.
			err = UnstableTradeConditionError{"skipping unpredictable upward trend"}
			return
		}
	} else {
		// If the current price is less than the moving average, the trend should be continuing downward
		if data.CurrentClosePrice.LessThan(decimal.NewFromFloat(movingAverage)) {
			// Place downward trend orders
			secondRequest, firstRequest = p.buildDownwardTrendingOrders(data)
		} else { // Things have gotten too unpredictable. Bail.
			err = UnstableTradeConditionError{"skipping unpredictable downward trend"}
			return
		}
	}

	return firstRequest, secondRequest, err
}

func (p *FollowTheLeaderProcessor) buildUpwardTrendingOrders(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Target spread is the trade fee + percent return target
	targetSpread := tradeFee.Add(decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.percentReturnTarget")))

	// Base quantity is the average quantity per trade
	baseQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))

	// Buy price based on current asking price
	buyPrice := data.AskPrice.Mul(decimal.NewFromFloat(1.0001))

	// Sell price based on current asking price plus target spread
	sellPrice := buyPrice.Add(data.AskPrice.Mul(targetSpread))

	// Buy quantity is double the base quantity
	buyQty := baseQty.Add(baseQty)

	// Sell quantity is adjusted to give return on both currencies
	sellQty := baseQty.Mul(buyPrice).Div(sellPrice).Add(baseQty)

	buyOrder = coinfactory.OrderRequest{
		Symbol:   p.Symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder = coinfactory.OrderRequest{
		Symbol:   p.Symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

func (p *FollowTheLeaderProcessor) buildDownwardTrendingOrders(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Target spread is the trade fee + percent return target
	targetSpread := tradeFee.Add(decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.percentReturnTarget")))

	// Base quantity is the average quantity per trade
	baseQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))

	// Sell price is based on the current bid price
	sellPrice := data.BidPrice.Mul(decimal.NewFromFloat(0.9999))

	// Buy price is based on the current bid price less the target spread
	buyPrice := sellPrice.Div(decimal.NewFromFloat(1).Add(targetSpread))

	// Buy quantity is double the base quantity
	buyQty := baseQty.Add(baseQty)

	// Sell quantity is adjusted to give return on both currencies
	sellQty := baseQty.Mul(buyPrice).Div(sellPrice).Add(baseQty)

	buyOrder = coinfactory.OrderRequest{
		Symbol:   p.Symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder = coinfactory.OrderRequest{
		Symbol:   p.Symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}
