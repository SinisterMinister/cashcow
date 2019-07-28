package main

import (
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

// Processor processes various data streams and executes orders when it determines to do so.
type Processor struct {
	Symbol *coinfactory.Symbol
}

// HandleTickerStreamData launches a goroutine that grabs a ticker stream and feeds the data
// to Processor.processTickerData.
func (p *Processor) HandleTickerStreamData(stopChan <-chan bool) {
	go func(stopChan <-chan bool) {
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
	}(stopChan)
}

func (p *Processor) processTickerData(data binance.SymbolTickerData) {
	log.Infof("Ticker data received: %s", data)
}
