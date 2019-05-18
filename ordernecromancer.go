package main

import (
	"sync"

	"github.com/smira/go-statsd"

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

func (o *orderNecromancer) BuryRequest(req coinfactory.OrderRequest) (err error) {
	// Lock the data for writing
	o.mutex.Lock()
	defer o.mutex.Unlock()

	// Add it and save it to the order collection
	orders := []coinfactory.OrderRequest{}
	err = o.db.Read("deadorders", req.Symbol, &orders)
	if err != nil {
		log.Debug("could not load previous orders. starting new")
	}

	orders = append(orders, req)
	err = o.db.Write("deadorders", req.Symbol, orders)
	metrics.Incr("necromancer.bury", 1, statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side))
	return
}

func (o *orderNecromancer) BuryOrder(order *coinfactory.Order) (err error) {
	// First, create an order request
	req := coinfactory.OrderRequest{
		Symbol:   order.Symbol,
		Side:     order.Side,
		Price:    order.Price,
		Quantity: order.GetStatus().OriginalQuantity.Sub(order.GetStatus().ExecutedQuantity),
	}

	o.BuryRequest(req)

	return
}

func (o *orderNecromancer) ResurrectOrders(symbol string, ask decimal.Decimal, bid decimal.Decimal) (orders []*coinfactory.Order) {
	// Load the orders
	orderReq := []coinfactory.OrderRequest{}
	allOrders := []coinfactory.OrderRequest{}
	savedOrders := []coinfactory.OrderRequest{}

	o.mutex.Lock()
	err := o.db.Read("deadorders", symbol, &allOrders)
	if err != nil {
		o.mutex.Unlock()
		return
	}

	for _, req := range allOrders {
		if (req.Side == "BUY" && req.Price.GreaterThanOrEqual(ask)) || (req.Side == "SELL" && req.Price.LessThanOrEqual(bid)) {
			metrics.Incr("necromancer.resurrect.attempt", 1, statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side))
			orderReq = append(orderReq, req)
			continue
		}
		metrics.Gauge("necromancer.buried", int64(len(savedOrders)), statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side))
		savedOrders = append(savedOrders, req)
	}

	err = o.db.Write("deadorders", symbol, savedOrders)
	if err != nil {
		log.WithError(err).Error("could not save dead orders after resurrection")
	}

	o.mutex.Unlock()

	orderChan := make(chan *coinfactory.Order)
	loopDoneChan := make(chan bool)

	go func() {
		for _, req := range orderReq {
			// Attempt to place the order
			order, err := cf.GetOrderManager().AttemptOrder(req)
			if err != nil {
				log.WithField("order", req).WithError(err).Warn("could not resurrect order")
				metrics.Incr("necromancer.resurrect.failure", 1, statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side), statsd.StringTag("type", "error"))
				o.BuryRequest(req)
				continue
			}

			orderChan <- order

			go func() {
				<-order.GetDoneChan()
				if order.GetStatus().Status == "CANCELED" {
					metrics.Incr("necromancer.resurrect.failure", 1, statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side), statsd.StringTag("type", "canceled"))
					o.BuryOrder(order)
					return
				}
				metrics.Incr("necromancer.resurrect.success", 1, statsd.StringTag("symbol", req.Symbol), statsd.StringTag("side", req.Side))
			}()
		}
		close(loopDoneChan)
	}()

orderLoop:
	for {
		select {
		case o := <-orderChan:
			orders = append(orders, o)
		case <-loopDoneChan:
			break orderLoop
		}
	}

	return
}
