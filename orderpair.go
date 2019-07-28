package main

import (
	"sync"

	"github.com/sinisterminister/coinfactory"
	log "github.com/sirupsen/logrus"
)

// OrderPair contains the Buy/Sell order pair that we'll attempt to make
type OrderPair struct {
	FirstOrderRequest  coinfactory.OrderRequest
	SecondOrderRequest coinfactory.OrderRequest

	firstOrder  *Order
	secondOrder *Order
	orderMutex  *sync.RWMutex

	doneChan chan bool
}

func buildOrderPair(firstRequest coinfactory.OrderRequest, secondRequest coinfactory.OrderRequest) *OrderPair {
	return &OrderPair{
		FirstOrderRequest:  firstRequest,
		SecondOrderRequest: secondRequest,
		orderMutex:         &sync.RWMutex{},
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

func (op *OrderPair) Execute() {
	log.Warnf("Placing order %s", op)
}
