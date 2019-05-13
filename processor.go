package main

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/spf13/viper"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

var tradeFee = decimal.NewFromFloat(0.0015)

type SpreadPlayerProcessor struct {
	symbol             *coinfactory.Symbol
	openOrders         []*coinfactory.Order
	staleOrders        []*coinfactory.Order
	janitorQuitChannel chan bool
	openOrdersMux      *sync.Mutex
}

func (p *SpreadPlayerProcessor) ProcessData(data binance.SymbolTickerData) {
	if len(p.openOrders) == 0 && isViable(data) {
		go p.placeCombinedTestOrder(data)
	}
}

func (p *SpreadPlayerProcessor) startOpenOrderJanitor() {
	log.Info("Starting order janitor")
	go startJanitor(p)
}

func (p *SpreadPlayerProcessor) placeQuoteBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildQuoteBasedBuyOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	// executeTestOrders(buy, sell)

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeAssetBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildAssetBasedBuyOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeCombinedOrder(data binance.SymbolTickerData) {
	buy, sell := p.buildCombinedOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeCombinedTestOrder(data binance.SymbolTickerData) {
	buy, sell := p.buildCombinedOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	executeTestOrders(buy, sell)
}

func (p *SpreadPlayerProcessor) adjustOrdersPriceBasedOnTrix(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) (err error) {
	ma, trixOsc, err := p.symbol.GetCurrentTrixIndicator("1m", 5)
	if err != nil {
		return err
	}

	// We're checking to see if the oscillator and the percent change between the
	// moving average and the current price are heading the same direction
	maDec := decimal.NewFromFloat(ma)
	oscDec := decimal.NewFromFloat(trixOsc)
	testOsc := p.symbol.GetTicker().CurrentClosePrice.Sub(maDec).Div(maDec)

	if !(testOsc.GreaterThanOrEqual(decimal.Zero) && oscDec.GreaterThanOrEqual(decimal.Zero)) && !(testOsc.LessThanOrEqual(decimal.Zero) && oscDec.LessThanOrEqual(decimal.Zero)) {
		return errors.New("price too unstable")
	}

	// Adjust order based on oscillator for now
	buyOrder.Price = buyOrder.Price.Add(buyOrder.Price.Mul(oscDec))
	sellOrder.Price = sellOrder.Price.Add(sellOrder.Price.Mul(oscDec))

	log.WithFields(log.Fields{
		"symbol":       p.symbol.Symbol,
		"ma":           maDec.StringFixed(8),
		"oscillator":   oscDec.StringFixed(8),
		"test":         testOsc.StringFixed(8),
		"currentPrice": p.symbol.GetTicker().CurrentClosePrice.StringFixed(8),
		"bid":          p.symbol.GetTicker().BidPrice.StringFixed(8),
		"ask":          p.symbol.GetTicker().AskPrice.StringFixed(8),
		"buyPrice":     buyOrder.Price.StringFixed(8),
		"sellPrice":    sellOrder.Price.StringFixed(8),
	}).Info("TRIX Indicator")

	return err
}

func (p *SpreadPlayerProcessor) adjustOrdersQuantityBasedOnAvailableFunds(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) {
	// Check balances and adjust quantity if necessary
	quoteBalance := cf.GetBalanceManager().GetAvailableBalance(p.symbol.QuoteAsset)
	baseBalance := cf.GetBalanceManager().GetAvailableBalance(p.symbol.BaseAsset)

	log.WithFields(log.Fields{
		p.symbol.BaseAsset:  baseBalance,
		p.symbol.QuoteAsset: quoteBalance,
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

func (p *SpreadPlayerProcessor) normalizeOrders(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) {
	// Normalize the price
	buyOrder.Price = normalizePrice(buyOrder.Price, p.symbol)
	sellOrder.Price = normalizePrice(sellOrder.Price, p.symbol)

	// Normalize quantities
	buyOrder.Quantity = normalizeQuantity(buyOrder.Quantity, p.symbol)
	sellOrder.Quantity = normalizeQuantity(sellOrder.Quantity, p.symbol)
}

func (p *SpreadPlayerProcessor) buildCombinedOrderRequests(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Get orders
	assetBasedBuy, assetBasedSell := p.buildAssetBasedBuyOrderRequests(data)
	quoteBasedBuy, quoteBasedSell := p.buildQuoteBasedBuyOrderRequests(data)

	// Create combined orders
	buyOrder = coinfactory.OrderRequest{}
	buyOrder.Symbol = p.symbol.Symbol
	buyOrder.Side = "BUY"
	buyOrder.Quantity = assetBasedBuy.Quantity.Add(quoteBasedBuy.Quantity)
	buyOrder.Price = assetBasedBuy.Price

	sellOrder = coinfactory.OrderRequest{}
	sellOrder.Symbol = p.symbol.Symbol
	sellOrder.Side = "SELL"
	sellOrder.Quantity = quoteBasedSell.Quantity.Add(assetBasedSell.Quantity)
	sellOrder.Price = assetBasedSell.Price

	return buyOrder, sellOrder
}

func (p *SpreadPlayerProcessor) buildAssetBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	spread := getSpread(data)
	bufferPercent := decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.bufferPercent"))
	txQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := spread.Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
	txMargin := spread.Sub(targetSpread).Mul(data.BidPrice).Div(decimal.NewFromFloat(2))
	buyAt := data.BidPrice.Add(txMargin)
	sellAt := data.AskPrice.Sub(txMargin)
	potentialReturn := sellAt.Sub(buyAt).Mul(txQty)

	log.WithFields(log.Fields{
		"B":  data.BidPrice.Round(8).StringFixed(8),
		"A":  data.AskPrice.Round(8).StringFixed(8),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"R":  potentialReturn.Round(8).StringFixed(8),
		"Q":  txQty.Round(8).StringFixed(8),
		"B@": buyAt.Round(8).StringFixed(8),
		"S@": sellAt.Round(8).StringFixed(8),
	}).Info("Asset based ticker for ", symbol.BaseAsset)

	buyQty := txQty
	sellQty := txQty
	buyPrice := normalizePrice(buyAt, p.symbol)
	sellPrice := normalizePrice(sellAt, p.symbol)

	buyOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

func (p *SpreadPlayerProcessor) buildQuoteBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	quoteBid := decimal.NewFromFloat(1).Div(data.AskPrice)
	quoteAsk := decimal.NewFromFloat(1).Div(data.BidPrice)
	quoteSpread := quoteAsk.Sub(quoteBid).Div(quoteBid)
	bufferPercent := decimal.NewFromFloat((viper.GetFloat64("spreadprocessor.bufferPercent")))
	txQty := data.QuoteVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := getSpread(data).Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
	txMargin := quoteSpread.Sub(targetSpread).Mul(quoteBid).Div(decimal.NewFromFloat(2))
	sellAt := quoteBid.Add(txMargin)
	buyAt := quoteAsk.Sub(txMargin)
	potentialReturn := buyAt.Sub(sellAt).Mul(txQty)
	returnInQuote := potentialReturn.Mul(data.AskPrice)

	log.WithFields(log.Fields{
		"B":  quoteBid.Round(8).StringFixed(8),
		"A":  quoteAsk.Round(8).StringFixed(8),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"R":  potentialReturn.Round(8).StringFixed(8),
		"r":  returnInQuote.Round(8).StringFixed(8),
		"Q":  txQty.Round(8).StringFixed(8),
		"B@": buyAt.Round(8).StringFixed(8),
		"S@": sellAt.Round(8).StringFixed(8),
	}).Info("Quote based ticker for ", symbol.QuoteAsset)

	buyQty := txQty.Mul(buyAt)
	sellQty := txQty.Mul(sellAt)
	sellPrice := decimal.NewFromFloat(1).Div(sellAt)
	buyPrice := decimal.NewFromFloat(1).Div(buyAt)
	buyPrice = normalizePrice(buyPrice, p.symbol)
	sellPrice = normalizePrice(sellPrice, p.symbol)

	buyOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

type FollowTheLeaderProcessor struct {
	symbol             *coinfactory.Symbol
	openOrders         []*coinfactory.Order
	staleOrders        []*coinfactory.Order
	janitorQuitChannel chan bool
	openOrdersMux      *sync.RWMutex
	readyMutex         *sync.RWMutex
	working            bool
}

func (processor *FollowTheLeaderProcessor) ProcessData(data binance.SymbolTickerData) {
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
			log.WithError(err).Error("could not build trix based order requests")
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
		cf.GetBalanceManager().AddReservedBalance(request1.Symbol, funds)

		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, order0)
		processor.openOrdersMux.Unlock()

		select {
		case <-order0.GetDoneChan():
		}
		processor.pruneOpenOrders()

		// Don't do the next order if this one was canceled
		if order0.GetStatus().Status == "CANCELED" {
			log.Warn("first ordered canceled. skipping second order")
			cf.GetBalanceManager().SubReservedBalance(request1.Symbol, funds)
			return
		}

		order1, err := cf.GetOrderManager().AttemptOrder(request1)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			// Nothing to do
			return
		}
		cf.GetBalanceManager().SubReservedBalance(request1.Symbol, funds)

		processor.openOrdersMux.Lock()
		processor.openOrders = append(processor.openOrders, order1)
		processor.openOrdersMux.Unlock()

		select {
		case <-order1.GetDoneChan():
		}
		processor.pruneOpenOrders()
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
			err = errors.New("skipping unpredictable upward trend")
			return
		}
	} else {
		// If the current price is less than the moving average, the trend should be continuing downward
		if data.CurrentClosePrice.LessThan(decimal.NewFromFloat(movingAverage)) {
			// Place downward trend orders
			buyRequest, sellRequest = processor.buildDownwardTrendingOrders(data)
			buyFirst = false
		} else { // Things have gotten too unpredictable. Bail.
			err = errors.New("skipping unpredictable downward trend")
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
	buyPrice := data.AskPrice

	// Sell price based on current asking price plus target spread
	sellPrice := data.AskPrice.Add(data.AskPrice.Mul(targetSpread))

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

	// Buy price is based on the current bid price less the target spread
	buyPrice := data.BidPrice.Div(decimal.NewFromFloat(1).Add(targetSpread))

	// Sell price is based on the current bid price
	sellPrice := data.BidPrice

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
	processor.openOrdersMux.Lock()
	processor.cancelExpiredStaleOrders()
	processor.pruneOpenOrders()
	processor.openOrdersMux.Unlock()
}

func (processor *FollowTheLeaderProcessor) pruneOpenOrders() {
	log.Debug("Pruning open orders")
	openOrders := []*coinfactory.Order{}
	for _, o := range processor.openOrders {
		// Remove any closed orders
		switch o.GetStatus().Status {
		// Skip this cases
		case "NEW":
		case "PARTIALLY_FILLED":
		case "PENDING_CANCEL":
		case "":
		// Delete the rest
		default:
			log.WithField("order", o).Debug("Removing closed order")
			continue
		}

		// Remove stale orders
		if o.GetAge().Nanoseconds() > viper.GetDuration("followtheleaderprocessor.markOrderAsStaleAfter").Nanoseconds() {

			// Mark order as stale
			log.WithField("order", o).Warn("Marking order as stale")
			processor.staleOrders = append(processor.staleOrders, o)
			continue
		}

		openOrders = append(openOrders, o)
	}

	processor.openOrders = openOrders
}

func (processor *FollowTheLeaderProcessor) cancelExpiredStaleOrders() {
	staleOrders := []*coinfactory.Order{}
	for _, o := range processor.staleOrders {
		// Cancel expired orders
		if o.GetAge().Nanoseconds() > viper.GetDuration("followtheleaderprocessor.cancelOrderAfter").Nanoseconds() {
			log.WithField("order", o).Debug("Cancelling expired order")
			go cancelOrder(o)
			continue
		}
		staleOrders = append(staleOrders, o)
	}
	processor.staleOrders = staleOrders
}
