package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/smira/go-statsd"

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
	firstOrderCount    int
	secondOrderCount   int
	janitorQuitChannel chan bool
	openOrdersMux      *sync.RWMutex
	staleOrdersMux     *sync.RWMutex
	firstOrderMux      *sync.RWMutex
	secondOrderMux     *sync.RWMutex
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
		metrics.Incr(fmt.Sprintf("processors.%s.process-data-events.skips", processor.symbol.Symbol), 1)
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

	stsd := metrics.CloneWithPrefix(fmt.Sprintf("processors.%s.process-data-events.", processor.symbol.Symbol))
	ostsd := metrics.CloneWithPrefix(fmt.Sprintf("processors.%s.orders.", processor.symbol.Symbol))

	// Lock the processor while we work
	processor.lockProcessor()
	stsd.Incr("attempts", 1)
	stsd.Gauge("locked", 1)

	// Unlock when we're finished
	defer processor.unlockProcessor()
	defer stsd.Gauge("locked", 0)

	// Get count of stale orders
	processor.openOrdersMux.RLock()
	numStaleOrders := len(processor.staleOrders)
	processor.openOrdersMux.RUnlock()

	// If stale orders exist, use them as the leader to follow, otherwise use trix
	if numStaleOrders > 0 {
		// Throttle based on stalled orders
		if numStaleOrders >= viper.GetInt("followtheleaderprocessor.maxStaleOrders") {
			log.Debug("throttling due to too many stale orders")
			stsd.Incr("fails", 1, statsd.StringTag("type", "throttled"))
			return
		}

		buyRequest, sellRequest, buyFirst = processor.buildLeaderBasedOrderRequests(data)
	} else {
		// Get TRIX based orders
		var err error
		buyRequest, sellRequest, buyFirst, err = processor.buildTrixBasedOrderRequests(data)
		if err != nil {
			if e, ok := err.(UnstableTradeConditionError); ok {
				stsd.Incr("fails", 1, statsd.StringTag("type", "trix-unstable"))
				log.WithError(e).Debug("order bailed")
			} else {
				stsd.Incr("fails", 1, statsd.StringTag("type", "error"))
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
		stsd.Incr("fails", 1, statsd.StringTag("type", "validation"))
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
			stsd.Incr("fails", 1, statsd.StringTag("type", "error"))

			// Nothing to do
			return
		}
		// Add to firstOrders
		processor.firstOrderMux.Lock()
		processor.firstOrderCount++
		ostsd.Incr("placed", 1, statsd.StringTag("order", "first"),statsd.StringTag("side", order0.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
		processor.firstOrderMux.Unlock()

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
			processor.firstOrderMux.Lock()
			processor.firstOrderCount--
			processor.firstOrderMux.Unlock()
			break
		case <-interrupt:
			ostsd.Incr("fails", 1, statsd.StringTag("order", "first"),statsd.StringTag("side", order0.Side), statsd.StringTag("type",  "interrupt"), statsd.StringTag("symbol", processor.symbol.Symbol))
			stsd.Incr("fails", 1, statsd.StringTag("type", "interrupt"))
			cf.GetOrderManager().CancelOrder(order0)
			return
		}

		processor.pruneOpenOrders()
		cf.GetBalanceManager().SubReservedBalance(asset, funds)

		// Don't do the next order if this one was canceled
		if order0.GetStatus().Status == "CANCELED" {
			ostsd.Incr("fails", 1, statsd.StringTag("order", "first"),statsd.StringTag("side", order0.Side), statsd.StringTag("type",  "cancelled"), statsd.StringTag("symbol", processor.symbol.Symbol))
			log.Warn("first ordered canceled. skipping second order")
			stsd.Incr("fails", 1, statsd.StringTag("type", "cancelled"))
			return
		} else {
			ostsd.Incr("succeeded", 1, statsd.StringTag("order", "first"),statsd.StringTag("side", order0.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
		}

		stsd.Incr("halfways", 1)

		order1, err := cf.GetOrderManager().AttemptOrder(request1)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			stsd.Incr("fails", 1, statsd.StringTag("type", "error"))

			// Try to bury
			oerr := getOrderNecromancerInstance().BuryRequest(request1)
			if oerr != nil {
				log.WithError(oerr).Error("could not burry order")
				stsd.Incr("fails", 1, statsd.StringTag("type", "unrecoverable"))
			}
			ostsd.Incr("buried", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", request1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
			return
		}

		processor.secondOrderMux.Lock()
		processor.secondOrderCount++
		ostsd.Incr("placed", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
		processor.secondOrderMux.Unlock()

		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, order1)
		processor.openOrdersMux.Unlock()

		select {
		case <-order1.GetDoneChan():
			processor.secondOrderMux.Lock()
			processor.secondOrderCount--
			processor.secondOrderMux.Unlock()
			// If canceled, give to order necromancer to handle
			if order1.GetStatus().Status == "CANCELED" || order1.GetStatus().Status == "PARTIALLY_FILLED" {
				log.WithField("order", order1).Info("order canceled. sending to necromancer for burial")
				ostsd.Incr("cancelled", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
				oerr := getOrderNecromancerInstance().BuryOrder(order1)
				if oerr != nil {
					log.WithError(oerr).Error("could not burry order")
					stsd.Incr("fails", 1, statsd.StringTag("type", "unrecoverable"))
				}
				ostsd.Incr("buried", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
			} else {
				ostsd.Incr("succeeded", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
				stsd.Incr("successes", 1)
			}
			
			processor.pruneOpenOrders()
		case <-interrupt:
			stsd.Incr("fails", 1, statsd.StringTag("type", "second-interrupt"))
			cf.GetOrderManager().CancelOrder(order1)
			oerr := getOrderNecromancerInstance().BuryOrder(order1)
			if oerr != nil {
					ostsd.Incr("fails", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("type",  "interrupt"), statsd.StringTag("symbol", processor.symbol.Symbol))
					log.WithError(oerr).Error("could not burry order")
				stsd.Incr("fails", 1, statsd.StringTag("type", "unrecoverable"))
			}
			ostsd.Incr("buried", 1, statsd.StringTag("order", "second"),statsd.StringTag("side", order1.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
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
	buyPrice := data.AskPrice.Mul(decimal.NewFromFloat(1.0001))

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
	sellPrice := data.BidPrice.Mul(decimal.NewFromFloat(0.9999))

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
	omets := metrics.CloneWithPrefix(fmt.Sprintf("processors.%s.orders.", processor.symbol.Symbol))
	omets.Gauge("open", int64(len(processor.openOrders)))
	omets.Gauge("stale", int64(len(processor.staleOrders)))
	omets.Gauge("first", int64(processor.firstOrderCount))
	omets.Gauge("second", int64(processor.secondOrderCount))
	log.WithFields(log.Fields{
		"open":   len(processor.openOrders),
		"stale":  len(processor.staleOrders),
		"first":  processor.firstOrderCount,
		"second": processor.secondOrderCount,
	}).Info(fmt.Sprintf("order counts for symbol %s", processor.symbol.Symbol))
}

func (processor *FollowTheLeaderProcessor) pruneOpenOrders() {
	log.Debug("Pruning open orders")
	openOrders := []*coinfactory.Order{}
	omets := metrics.CloneWithPrefix(fmt.Sprintf("processors.%s.orders.", processor.symbol.Symbol))
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
			omets.Incr("stalled", 1, statsd.StringTag("side", o.Side), statsd.StringTag("symbol", processor.symbol.Symbol))
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
	omets := metrics.CloneWithPrefix(fmt.Sprintf("processors.%s.orders.", processor.symbol.Symbol))
	for _, o := range processor.staleOrders {
		// Cancel expired orders
		if isOrderExpired(o) {
			log.WithField("order", o).Warn("Cancelling expired order")
			omets.Incr("cancelled", 1, statsd.StringTag("side", o.Side),statsd.StringTag("type", "expired"), statsd.StringTag("symbol", processor.symbol.Symbol))

			go cancelOrder(o)
			continue
		}
		staleOrders = append(staleOrders, o)
	}
	processor.staleOrders = staleOrders
}
