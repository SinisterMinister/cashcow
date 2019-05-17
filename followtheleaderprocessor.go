package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type FollowTheLeaderProcessor struct {
	symbol             *coinfactory.Symbol
	openOrders         []*coinfactory.Order
	staleOrders        []*coinfactory.Order
	janitorQuitChannel chan bool
	openOrdersMux      *sync.RWMutex
	staleOrdersMux     *sync.RWMutex
	readyMutex         *sync.RWMutex
	working            bool
}

func (processor *FollowTheLeaderProcessor) ProcessData(data binance.SymbolTickerData) {
	// Resurrect dead orders if possible
	go func() {
		orders := getOrderNecromancerInstance().ResurrectOrders(data.Symbol, data.AskPrice, data.BidPrice)
		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, orders...)
		processor.openOrdersMux.Unlock()
	}()

	// Check if ready for data
	if !processor.isReady() {
		// Not ready. Skip.
		return
	}

	// Attempt the order
	processor.attemptOrder(data)
}

func (processor *FollowTheLeaderProcessor) init() {
	// Get open orders
	orders, err := cf.GetOrderManager().GetOpenOrders(processor.symbol)
	if err != nil {
		log.WithError(err).Error("could not fetch open orders")
	}
	processor.openOrdersMux.Lock()
	processor.openOrders = orders
	processor.openOrdersMux.Unlock()
	go processor.startJanitor()
}

func (processor *FollowTheLeaderProcessor) lockProcessor() {
	processor.readyMutex.Lock()
	processor.working = true
	processor.readyMutex.Unlock()
}

func (processor *FollowTheLeaderProcessor) unlockProcessor() {
	processor.readyMutex.Lock()
	processor.working = false
	processor.readyMutex.Unlock()
}

func (processor *FollowTheLeaderProcessor) attemptOrder(data binance.SymbolTickerData) {
	var (
		buyRequest, sellRequest coinfactory.OrderRequest
		buyFirst                bool
	)

	// Lock the processor while we work
	processor.lockProcessor()

	// Unlock when we're finished
	defer processor.unlockProcessor()

	// Get count of stale orders
	processor.openOrdersMux.RLock()
	numStaleOrders := len(processor.staleOrders)
	processor.openOrdersMux.RUnlock()

	// If stale orders exist, use them as the leader to follow, otherwise use trix
	if numStaleOrders > 0 {
		// Throttle based on stalled orders
		if numStaleOrders >= viper.GetInt("followtheleaderprocessor.maxStaleOrders") {
			log.Debug("throttling due to too many stale orders")
			return
		}

		buyRequest, sellRequest, buyFirst = processor.buildLeaderBasedOrderRequests(data)
	} else {
		// Get TRIX based orders
		var err error
		buyRequest, sellRequest, buyFirst, err = processor.buildTrixBasedOrderRequests(data)
		if err != nil {
			if e, ok := err.(UnstableTradeConditionError); ok {
				log.WithError(e).Debug("order bailed")
			} else {
				log.WithError(err).Error("could not build trix based order requests")
			}

			return
		}
	}

	// Adjust orders to not go over available balances
	adjustOrdersQuantityBasedOnAvailableFunds(&buyRequest, &sellRequest, processor.symbol)

	// Normalize orders
	normalizeOrders(&buyRequest, &sellRequest, processor.symbol)

	// Abort if order pair will fail or have a negative return
	if ok := validateOrderPair(buyRequest, sellRequest); !ok {
		return
	}

	go func() {
		var request0, request1 coinfactory.OrderRequest
		if buyFirst {
			request0 = buyRequest
			request1 = sellRequest
		} else {
			request0 = sellRequest
			request1 = buyRequest
		}

		order0, err := cf.GetOrderManager().AttemptOrder(request0)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			// Nothing to do
			return
		}

		// Lock up the funds for the second order
		funds := request1.Price.Mul(request1.Quantity)
		var asset string
		if request1.Side == "BUY" {
			asset = processor.symbol.BaseAsset
		} else {
			asset = processor.symbol.QuoteAsset
		}
		cf.GetBalanceManager().AddReservedBalance(asset, funds)

		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, order0)
		processor.openOrdersMux.Unlock()

		// Intercept the interrupt signal and pass it along
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)

		select {
		case <-order0.GetDoneChan():
			break
		case <-interrupt:
			cf.GetOrderManager().CancelOrder(order0)
			return
		}

		processor.pruneOpenOrders()
		cf.GetBalanceManager().SubReservedBalance(request1.Symbol, funds)

		// Don't do the next order if this one was canceled
		if order0.GetStatus().Status == "CANCELED" {
			log.Warn("first ordered canceled. skipping second order")
			return
		}

		order1, err := cf.GetOrderManager().AttemptOrder(request1)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			// Nothing to do
			return
		}

		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, order1)
		processor.openOrdersMux.Unlock()

		select {
		case <-order1.GetDoneChan():
			// If canceled, give to order necromancer to handle
			if order1.GetStatus().Status == "CANCELED" || order1.GetStatus().Status == "PARTIALLY_FILLED" {
				log.WithField("order", order1).Info("order canceled. sending to necromancer for burial")
				oerr := getOrderNecromancerInstance().BuryOrder(order1)
				if oerr != nil {
					log.WithError(oerr).Error("could not burry order")
				}
			}
			processor.pruneOpenOrders()
		case <-interrupt:
			cf.GetOrderManager().CancelOrder(order1)
			getOrderNecromancerInstance().BuryOrder(order1)
			return
		}

	}()
}

func (processor *FollowTheLeaderProcessor) buildLeaderBasedOrderRequests(data binance.SymbolTickerData) (buyRequest coinfactory.OrderRequest, sellRequest coinfactory.OrderRequest, buyFirst bool) {
	// Get latest stale order
	processor.openOrdersMux.RLock()
	staleOrderCount := len(processor.staleOrders)
	staleOrder := processor.staleOrders[staleOrderCount-1]
	processor.openOrdersMux.RUnlock()

	// Build the appropriate trending order based on the stale order
	if staleOrder.Side == "BUY" {
		buyRequest, sellRequest = processor.buildUpwardTrendingOrders(data)
		buyFirst = true
	} else {
		buyRequest, sellRequest = processor.buildDownwardTrendingOrders(data)
		buyFirst = false
	}
	return buyRequest, sellRequest, buyFirst
}

func (processor *FollowTheLeaderProcessor) buildTrixBasedOrderRequests(data binance.SymbolTickerData) (buyRequest coinfactory.OrderRequest, sellRequest coinfactory.OrderRequest, buyFirst bool, err error) {
	// Get trix indicator
	movingAverage, trix, err := processor.symbol.GetCurrentTrixIndicator("1m", 5)
	if err != nil {
		log.WithError(err).Error("could not get TRIX indicator")
		return
	}

	// If trix is positive, the price is increasing
	if trix > 0 {
		// If the current price is greater than the moving average, the trend should be continuing upward
		if data.CurrentClosePrice.GreaterThan(decimal.NewFromFloat(movingAverage)) {
			// Place upward trend orders
			buyRequest, sellRequest = processor.buildUpwardTrendingOrders(data)
			buyFirst = true
		} else { // Things have gotten too unpredictable. Bail.
			err = UnstableTradeConditionError{"skipping unpredictable upward trend"}
			return
		}
	} else {
		// If the current price is less than the moving average, the trend should be continuing downward
		if data.CurrentClosePrice.LessThan(decimal.NewFromFloat(movingAverage)) {
			// Place downward trend orders
			buyRequest, sellRequest = processor.buildDownwardTrendingOrders(data)
			buyFirst = false
		} else { // Things have gotten too unpredictable. Bail.
			err = UnstableTradeConditionError{"skipping unpredictable downward trend"}
			return
		}
	}

	return buyRequest, sellRequest, buyFirst, err
}

func (processor *FollowTheLeaderProcessor) buildUpwardTrendingOrders(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Target spread is the trade fee + percent return target
	targetSpread := tradeFee.Add(decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.percentReturnTarget")))

	// Base quantity is the average quantity per trade
	baseQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))

	// Buy price based on current asking price
	buyPrice := data.AskPrice.Mul(decimal.NewFromFloat(1.0005))

	// Sell price based on current asking price plus target spread
	sellPrice := buyPrice.Add(data.AskPrice.Mul(targetSpread))

	// Buy quantity is double the base quantity
	buyQty := baseQty.Add(baseQty)

	// Sell quantity is adjusted to give return on both currencies
	sellQty := baseQty.Mul(buyPrice).Div(sellPrice).Add(baseQty)

	buyOrder = coinfactory.OrderRequest{
		Symbol:   processor.symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder = coinfactory.OrderRequest{
		Symbol:   processor.symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

func (processor *FollowTheLeaderProcessor) buildDownwardTrendingOrders(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Target spread is the trade fee + percent return target
	targetSpread := tradeFee.Add(decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.percentReturnTarget")))

	// Base quantity is the average quantity per trade
	baseQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))

	// Sell price is based on the current bid price
	sellPrice := data.BidPrice.Mul(decimal.NewFromFloat(0.9995))

	// Buy price is based on the current bid price less the target spread
	buyPrice := sellPrice.Div(decimal.NewFromFloat(1).Add(targetSpread))

	// Buy quantity is double the base quantity
	buyQty := baseQty.Add(baseQty)

	// Sell quantity is adjusted to give return on both currencies
	sellQty := baseQty.Mul(buyPrice).Div(sellPrice).Add(baseQty)

	buyOrder = coinfactory.OrderRequest{
		Symbol:   processor.symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder = coinfactory.OrderRequest{
		Symbol:   processor.symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

func (processor *FollowTheLeaderProcessor) isReady() bool {
	processor.readyMutex.RLock()
	defer processor.readyMutex.RUnlock()
	if processor.working {
		return false
	}
	processor.openOrdersMux.RLock()
	defer processor.openOrdersMux.RUnlock()
	if len(processor.openOrders) > 0 {
		return false
	}
	return true
}

func (processor *FollowTheLeaderProcessor) startJanitor() {
	log.Info("Order janitor started successfully")
	// Clean up orphaned orders
	processor.cleanUpOrders()
	timer := time.NewTicker(15 * time.Second)

	// Intercept the interrupt signal and pass it along
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Start the loop
	for {
		select {
		case <-timer.C:
			processor.cleanUpOrders()
		case <-interrupt:
			processor.janitorQuitChannel <- true
		case <-processor.janitorQuitChannel:
			return
		}
	}
}

func (processor *FollowTheLeaderProcessor) cleanUpOrders() {
	processor.staleOrdersMux.Lock()
	processor.openOrdersMux.Lock()
	defer processor.staleOrdersMux.Unlock()
	defer processor.openOrdersMux.Unlock()
	processor.pruneOpenOrders()
	processor.pruneStaleOrders()
	processor.cancelExpiredStaleOrders()
	log.WithField("open", len(processor.openOrders)).WithField("stale", len(processor.staleOrders)).Info(fmt.Sprintf("order counts for symbol %s", processor.symbol.Symbol))
}

func (processor *FollowTheLeaderProcessor) pruneOpenOrders() {
	log.Debug("Pruning open orders")
	openOrders := []*coinfactory.Order{}
	for _, o := range processor.openOrders {
		// Update open orders
		go cf.GetOrderManager().UpdateOrderStatus(o)
		// Remove any closed orders
		if !isOrderOpen(o) {
			log.WithField("order", o).Debug("Removing closed order")
			continue
		}

		// Remove stale orders
		if isOrderStale(o) {
			// Mark order as stale
			log.WithField("order", o).Debug("Marking order as stale")
			processor.staleOrders = append(processor.staleOrders, o)
			continue
		}

		openOrders = append(openOrders, o)
	}

	processor.openOrders = openOrders
}

func (processor *FollowTheLeaderProcessor) pruneStaleOrders() {
	log.Debug("Pruning stale orders")
	staleOrders := []*coinfactory.Order{}
	for _, o := range processor.staleOrders {
		go cf.GetOrderManager().UpdateOrderStatus(o)
		// Remove any closed orders
		if !isOrderOpen(o) {
			log.WithField("order", o).Debug("Removing closed order")
			continue
		}

		staleOrders = append(staleOrders, o)
	}

	processor.staleOrders = staleOrders
}

func (processor *FollowTheLeaderProcessor) cancelExpiredStaleOrders() {
	staleOrders := []*coinfactory.Order{}
	for _, o := range processor.staleOrders {
		// Cancel expired orders
		if isOrderExpired(o) {
			log.WithField("order", o).Warn("Cancelling expired order")
			go cancelOrder(o)
			continue
		}
		staleOrders = append(staleOrders, o)
	}
	processor.staleOrders = staleOrders
}
