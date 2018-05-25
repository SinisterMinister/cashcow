package main

import (
	"os"
	"os/signal"
	"time"

	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"

	log "github.com/sirupsen/logrus"
)

func startJanitor(p *SpreadPlayerProcessor) {
	log.Info("Order janitor started successfully")
	timer := time.NewTicker(15 * time.Second)

	// Intercept the interrupt signal and pass it along
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Start the loop
	for {
		select {
		case <-timer.C:
			p.openOrdersMux.Lock()
			pruneOpenOrders(p)
			cancelExpiredStaleOrders(p)
			p.openOrdersMux.Unlock()
		case <-interrupt:
			p.janitorQuitChannel <- true
		case <-p.janitorQuitChannel:
			return
		}
	}
}

func pruneOpenOrders(p *SpreadPlayerProcessor) {
	log.Debug("Pruning open orders")
	openOrders := []*coinfactory.Order{}
	for _, o := range p.openOrders {
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
		if o.GetAge().Nanoseconds() > viper.GetDuration("spreadprocessor.markOrderAsStaleAfter").Nanoseconds() {
			log.WithField("order", o).Debug("Marking order as stale")
			p.staleOrders = append(p.staleOrders, o)
			continue
		}

		openOrders = append(openOrders, o)
	}

	p.openOrders = openOrders
}

func cancelExpiredStaleOrders(p *SpreadPlayerProcessor) {
	staleOrders := []*coinfactory.Order{}
	for _, o := range p.staleOrders {
		// Cancel expired orders
		if o.GetAge().Nanoseconds() > viper.GetDuration("spreadprocessor.cancelOrderAfter").Nanoseconds() {
			log.WithField("order", o).Debug("Cancelling expired order")
			go cancelOrder(o)
			continue
		}
		staleOrders = append(staleOrders, o)
	}
	p.staleOrders = staleOrders
}

func cancelOrder(o *coinfactory.Order) {
	err := cf.GetOrderManager().CancelOrder(o)
	if err != nil {
		log.WithError(err).Error("Could not cancel expired order")
	}
}
