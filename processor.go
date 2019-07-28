package main

import (
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

type Processor struct {
	Symbol *coinfactory.Symbol
}

func (p *Processor) handleTickerStreamData(stopChan <-chan bool) {
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
}

func (p *Processor) processTickerData(data binance.SymbolTickerData) {
	log.Infof("Ticker data received: %s", data)
}
