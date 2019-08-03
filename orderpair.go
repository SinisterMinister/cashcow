package main

import (
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// OrderPair contains the Buy/Sell order pair that we'll attempt to make
type OrderPair struct {
	firstRequest         coinfactory.OrderRequest
	originalFirstRequest coinfactory.OrderRequest

	secondRequest         coinfactory.OrderRequest
	originalSecondRequest coinfactory.OrderRequest

	sellOrders []*Order

	firstOrder  *Order
	secondOrder *Order
	orderMutex  *sync.RWMutex
	symbol      *coinfactory.Symbol

	doneChan chan bool
	stopChan <-chan bool
}

func buildOrderPair(stopChan <-chan bool, firstRequest coinfactory.OrderRequest, secondRequest coinfactory.OrderRequest) *OrderPair {
	return &OrderPair{
		firstRequest:          firstRequest,
		originalFirstRequest:  firstRequest,
		secondRequest:         secondRequest,
		originalSecondRequest: secondRequest,
		orderMutex:            &sync.RWMutex{},
		symbol:                coinfactory.GetSymbolService().GetSymbol(firstRequest.Symbol),
		stopChan:              stopChan,
	}
}

func (op *OrderPair) GetDoneChannel() <-chan bool {
	return op.doneChan
}

// Has the order pair finished executing?
func (op *OrderPair) IsDone() bool {
	// If the done channel returns anything, both orders have completed executing
	select {
	case <-op.doneChan:
		return true
	default:
		return false
	}
}

// Has the order pair stalled mid execution
func (op *OrderPair) IsStalled() bool {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()

	// If the first order is stale, return it
	if op.firstOrder != nil && op.firstOrder.IsStale() {
		return true
	}

	if op.secondOrder != nil && op.secondOrder.IsStale() {
		return true
	}
	return false
}

func (op *OrderPair) GetStalledOrder() *Order {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()

	// If the first order is stale, return it
	if op.firstOrder != nil && op.firstOrder.IsStale() {
		return op.firstOrder
	}

	if op.secondOrder != nil && op.secondOrder.IsStale() {
		return op.secondOrder
	}
	return nil
}

func (op *OrderPair) GetFirstOrder() *Order {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()
	return op.firstOrder
}

func (op *OrderPair) GetSecondOrder() *Order {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()
	return op.secondOrder
}

func (op *OrderPair) GetBuyRequest() coinfactory.OrderRequest {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()
	if op.firstRequest.Side == "BUY" {
		return op.firstRequest
	}
	return op.secondRequest
}

func (op *OrderPair) GetSellRequest() coinfactory.OrderRequest {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()

	if op.firstRequest.Side == "SELL" {
		return op.firstRequest
	}
	return op.secondRequest
}

func (op *OrderPair) Execute() {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()

	log.Warnf("Placing order %s", op)
	// Adjust the balances to fit in our wallet if necessary
	op.adjustRequestsBasedOnWalletBalances()

	// Normalize the request values
	op.normalizeRequests()

	// Validate that the request is worth executing and bail if we fail
	if ok := op.validateRequests(); !ok {
		return
	}

	// Attempt to make the orders work
	op.attemptOrders()
}

func (op *OrderPair) attemptOrders() {
	// Spawn a goroutine to attempt the orders
	go func() {
		// Place the first order
		op.handleFirstOrder()

		// Check to see if any of it was fulfilled
		if op.firstOrder.GetStatus().ExecutedQuantity.IsZero() {
			// Nothing was executed. Must be cancelled. Bail
			return
		}

		// Place the second order
		op.handleSecondOrder()

		// Close the done chan since we're finished
		close(op.doneChan)
	}()
}

func (op *OrderPair) handleFirstOrder() {
	firstOrder, err := coinfactory.GetOrderService().AttemptOrder(op.firstRequest)
	if err != nil {
		log.WithError(err).WithField("request", op.firstRequest).Errorf("Order attempt failed!")
		return // Nothing more to do
	}

	// Store the order
	op.orderMutex.Lock()
	op.firstOrder = &Order{firstOrder}
	op.orderMutex.Unlock()

	// Setup the timeout for the first order
	timer := time.NewTimer(viper.GetDuration("followTheLeaderProcessor.firstOrderTimeout"))

	// Wait for the first order to finish and then kick off the second order
	select {
	case <-timer.C:
		// Order timed out, so time to cancel it
		log.WithField("order", op.firstOrder).Warn("First order timed out. Cancelling.")
		err := coinfactory.GetOrderService().CancelOrder(op.firstOrder.Order)
		if err != nil {
			log.WithError(err).WithField("order", op.firstOrder).Errorf("Order cancellation failed!")
		}
	case <-firstOrder.GetDoneChan():
		// Order complete
	}
}

func (op *OrderPair) handleSecondOrder() {
	// Update the second order request with the fulfilled amount in case only partially filled
	if op.firstOrder.GetStatus().ExecutedQuantity.LessThan(op.firstOrder.Quantity) {
		op.secondRequest.Quantity = op.firstOrder.GetStatus().ExecutedQuantity

		// Normalize the request
		op.normalizeRequests()
	}

	// Place the second order
	secondOrder, err := coinfactory.GetOrderService().AttemptOrder(op.secondRequest)
	if err != nil {
		log.WithError(err).WithField("request", op.secondRequest).Errorf("Order attempt failed!")
		return
	}

	// Store the order
	op.orderMutex.Lock()
	op.secondOrder = &Order{secondOrder}
	op.orderMutex.Unlock()

	// Watch for reaping
	op.watchForReaping()

	// Wait for the order to finish
	<-op.GetSecondOrder().GetDoneChan()
}

func (op *OrderPair) watchForReaping() {
	go func() {
		// Get ticker stream
		tickerStream := op.symbol.GetTickerStream(op.doneChan)

		for {
			select {
			case <-op.doneChan:
				// Bail if done
				return
			default:
			}

			select {
			case <-op.doneChan:
				// Bail if done
				return
			case data := <-tickerStream:
				// Reap if necessary
				if op.shouldReap(data) {
					op.reap()
					return
				}
			}
		}
	}()
}

func (op *OrderPair) shouldReap(data binance.SymbolTickerData) bool {
	// Get the reap price
	var reapPrice decimal.Decimal
	spreadMultiplier := decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.reaperSpreadMultiplier"))
	maxSpreadDistance := decimal.NewFromFloat(viper.GetFloat64("followtheleaderprocessor.percentReturnTarget")).Mul(spreadMultiplier)
	reapPriceDistance := op.secondOrder.Price.Mul(maxSpreadDistance)

	if op.secondOrder.Side == "BUY" {
		reapPrice = op.secondOrder.Price.Add(reapPriceDistance)
		if data.AskPrice.GreaterThanOrEqual(reapPrice) {
			return true
		}
	} else {
		reapPrice = op.secondOrder.Price.Sub(reapPriceDistance)
		if data.BidPrice.LessThanOrEqual(reapPrice) {
			return true
		}
	}
	return false
}

func (op *OrderPair) reap() {
	// Cancel the order
	err := coinfactory.GetOrderService().CancelOrder(op.secondOrder.Order)
	if err != nil {
		log.WithError(err).WithField("order", op.secondOrder).Errorf("Order cancellation failed!")
	}

	// update the request
	op.orderMutex.Lock()
	op.secondRequest.Quantity = op.secondRequest.Quantity.Sub(op.secondOrder.GetStatus().ExecutedQuantity)
	op.orderMutex.Unlock()

	// Watch for resurrection
	op.watchForResurrection()
}

func (op *OrderPair) watchForResurrection() {
	go func() {
		tickerStream := op.symbol.GetTickerStream(op.stopChan)

		// Watch ticker stream
		for {
			// bail on stop
			select {
			case <-op.stopChan:
				return
			default:
			}

			select {
			case <-op.stopChan:
				return
			case data := <-tickerStream:
				if op.shouldResurrect(data) {
					op.resurrect()
					return
				}
			}
		}
	}()
}

func (op *OrderPair) shouldResurrect(data binance.SymbolTickerData) bool {
	if op.secondOrder.Side == "BUY" {
		// If the ask price is less than or equal to the request price, we should resurrect
		if data.AskPrice.LessThanOrEqual(op.secondRequest.Price) {
			return true
		}
	} else {
		// If the bid price is greater than or equal to the request price, we should resurrect
		if data.BidPrice.GreaterThanOrEqual(op.secondRequest.Price) {
			return true
		}
	}

	// Not time to resurrect
	return false
}

func (op *OrderPair) resurrect() {
	// Reset the done channel
	op.orderMutex.Lock()
	op.doneChan = make(chan bool)
	op.orderMutex.Unlock()

	// Kick of the second order
	go op.handleSecondOrder()
}

func (op *OrderPair) adjustRequestsBasedOnWalletBalances() {
	var buyRequest, sellRequest *coinfactory.OrderRequest

	// Setup variables to work with
	symbol := op.symbol.Symbol
	quoteBalance := coinfactory.GetBalanceManager().GetUsableBalance(symbol)
	baseBalance := coinfactory.GetBalanceManager().GetUsableBalance(symbol)
	if op.firstRequest.Side == "BUY" {
		buyRequest = &op.firstRequest
		sellRequest = &op.secondRequest
	} else {
		buyRequest = &op.secondRequest
		sellRequest = &op.firstRequest
	}

	// Percentage of the wallet balance to use for the order
	fallBackPercent := decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))

	// First, adjust based on the buy request
	if buyRequest.Quantity.Mul(buyRequest.Price).GreaterThan(quoteBalance) {
		adjPercent := quoteBalance.Mul(fallBackPercent).Div(buyRequest.Price).Div(buyRequest.Quantity)
		buyRequest.Quantity = buyRequest.Quantity.Mul(adjPercent)
		sellRequest.Quantity = sellRequest.Quantity.Mul(adjPercent)
	}

	// Second, adjust based on sell request
	if sellRequest.Quantity.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(fallBackPercent).Div(sellRequest.Quantity)
		buyRequest.Quantity = buyRequest.Quantity.Mul(adjPercent)
		sellRequest.Quantity = sellRequest.Quantity.Mul(adjPercent)
	}
}

func (op *OrderPair) normalizeRequests() {
	// Normalize the price
	op.firstRequest.Price = normalizePrice(op.firstRequest.Price, op.symbol)
	op.secondRequest.Price = normalizePrice(op.secondRequest.Price, op.symbol)

	// Normalize quantities
	op.firstRequest.Quantity = normalizeQuantity(op.firstRequest.Quantity, op.symbol)
	op.secondRequest.Quantity = normalizeQuantity(op.secondRequest.Quantity, op.symbol)
}

func (op *OrderPair) validateRequests() bool {
	// Bail on zero value trades
	if op.firstRequest.Quantity.Equals(decimal.Zero) || op.secondRequest.Quantity.Equals(decimal.Zero) {
		log.WithFields(log.Fields{
			"firstRequest": op.firstRequest,
			"secondSecond": op.secondRequest,
		}).Debug("Skipping zero value order")
		return false
	}

	buy := op.GetBuyRequest()
	sell := op.GetSellRequest()

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
