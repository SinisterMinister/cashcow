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
	firstOrderRequest  coinfactory.OrderRequest
	secondOrderRequest coinfactory.OrderRequest

	buyOrders  []*Order
	sellOrders []*Order

	firstOrder  *Order
	secondOrder *Order
	orderMutex  *sync.RWMutex
	symbol      *coinfactory.Symbol

	doneChan chan bool
}

func buildOrderPair(firstRequest coinfactory.OrderRequest, secondRequest coinfactory.OrderRequest) *OrderPair {
	return &OrderPair{
		firstOrderRequest:  firstRequest,
		secondOrderRequest: secondRequest,
		orderMutex:         &sync.RWMutex{},
		symbol:             coinfactory.GetSymbolService().GetSymbol(firstRequest.Symbol),
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
	if op.firstOrderRequest.Side == "BUY" {
		return op.firstOrderRequest
	}
	return op.secondOrderRequest
}

func (op *OrderPair) GetSellRequest() coinfactory.OrderRequest {
	op.orderMutex.RLock()
	defer op.orderMutex.RUnlock()

	if op.firstOrderRequest.Side == "SELL" {
		return op.firstOrderRequest
	}
	return op.secondOrderRequest
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
	firstOrder, err := coinfactory.GetOrderService().AttemptOrder(op.firstOrderRequest)
	if err != nil {
		log.WithError(err).WithField("request", op.firstOrderRequest).Errorf("Order attempt failed!")
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
	op.secondOrderRequest.Quantity = op.firstOrder.GetStatus().ExecutedQuantity

	// Normalize the request
	op.normalizeRequests()

	// Place the second order
	secondOrder, err := coinfactory.GetOrderService().AttemptOrder(op.secondOrderRequest)
	if err != nil {
		log.WithError(err).WithField("request", op.secondOrderRequest).Errorf("Order attempt failed!")
		return
	}

	// Store the order
	op.orderMutex.Lock()
	op.secondOrder = &Order{secondOrder}
	op.orderMutex.Unlock()

	// Wait for the order to finish
	<-op.GetSecondOrder().GetDoneChan()
}

func (op *OrderPair) adjustRequestsBasedOnWalletBalances() {
	var buyRequest, sellRequest *coinfactory.OrderRequest

	// Setup variables to work with
	symbol := op.symbol.Symbol
	quoteBalance := coinfactory.GetBalanceManager().GetUsableBalance(symbol)
	baseBalance := coinfactory.GetBalanceManager().GetUsableBalance(symbol)
	if op.firstOrderRequest.Side == "BUY" {
		buyRequest = &op.firstOrderRequest
		sellRequest = &op.secondOrderRequest
	} else {
		buyRequest = &op.secondOrderRequest
		sellRequest = &op.firstOrderRequest
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
	op.firstOrderRequest.Price = normalizePrice(op.firstOrderRequest.Price, op.symbol)
	op.secondOrderRequest.Price = normalizePrice(op.secondOrderRequest.Price, op.symbol)

	// Normalize quantities
	op.firstOrderRequest.Quantity = normalizeQuantity(op.firstOrderRequest.Quantity, op.symbol)
	op.secondOrderRequest.Quantity = normalizeQuantity(op.secondOrderRequest.Quantity, op.symbol)
}

func (op *OrderPair) validateRequests() bool {
	// Bail on zero value trades
	if op.firstOrderRequest.Quantity.Equals(decimal.Zero) || op.secondOrderRequest.Quantity.Equals(decimal.Zero) {
		log.WithFields(log.Fields{
			"firstRequest": op.firstOrderRequest,
			"secondSecond": op.secondOrderRequest,
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
