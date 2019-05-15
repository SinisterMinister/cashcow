package main

import (
	"sync"

	"github.com/shopspring/decimal"

	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"

	log "github.com/sirupsen/logrus"

	scribble "github.com/nanobox-io/golang-scribble"
)

type orderNecromancer struct {
	db    *scribble.Driver
	mutex *sync.Mutex
}

var orderNecromancerOnce = sync.Once{}
var orderNecromancerInstance *orderNecromancer

func getOrderNecromancerInstance() *orderNecromancer {
	orderNecromancerOnce.Do(func() {

		db, err := scribble.New(viper.GetString("scribbledb.dir"), nil)
		if err != nil {
			log.WithError(err).Panic("cannot start scribble database")
		}

		orderNecromancerInstance = &orderNecromancer{
			db:    db,
			mutex: &sync.Mutex{},
		}

	})

	return orderNecromancerInstance
}

func (o *orderNecromancer) BuryOrder(order *coinfactory.Order) (err error) {
	// First, create an order request
	req := coinfactory.OrderRequest{
		Symbol:   order.Symbol,
		Side:     order.Side,
		Price:    order.Price,
		Quantity: order.GetStatus().OriginalQuantity.Sub(order.GetStatus().ExecutedQuantity),
	}

	// Lock the data for writing
	o.mutex.Lock()
	defer o.mutex.Unlock()

	// Add it and save it to the order collection
	orders := []coinfactory.OrderRequest{}
	err = o.db.Read("deadorders", order.Symbol, orders)
	if err != nil {
		log.Debug("could not load previous orders. starting new")
	}

	orders = append(orders, req)
	err = o.db.Write("deadorders", order.Symbol, orders)
	return
}

func (o *orderNecromancer) ResurrectOrders(symbol string, ask decimal.Decimal, bid decimal.Decimal) (orders []*coinfactory.Order) {
	// Lock everything until we're done
	o.mutex.Lock()
	defer o.mutex.Unlock()

	// Load the orders
	orderReq := []coinfactory.OrderRequest{}
	allOrders := []coinfactory.OrderRequest{}
	savedOrders := []coinfactory.OrderRequest{}
	err := o.db.Read("deadorders", symbol, allOrders)
	if err != nil {
		return
	}

	for _, req := range allOrders {
		if (req.Side == "BUY" && req.Price.GreaterThanOrEqual(ask)) || (req.Side == "SELL" && req.Price.LessThanOrEqual(bid)) {
			orderReq = append(orderReq, req)
			continue
		}
		savedOrders = append(savedOrders, req)
	}

	for _, req := range orderReq {
		// Attempt to place the order
		order, err := cf.GetOrderManager().AttemptOrder(req)
		if err != nil {
			log.WithField("order", req).WithError(err).Info("could not resurrect order")
			savedOrders = append(savedOrders, req)
			continue
		}

		go func() {
			<-order.GetDoneChan()
			if order.GetStatus().Status == "CANCELED" {
				o.BuryOrder(order)
			}
		}()

		orders = append(orders, order)
	}

	err = o.db.Write("deadorders", symbol, savedOrders)
	if err != nil {
		log.WithError(err).Error("could not save dead orders after resurrection")
	}

	return
}
