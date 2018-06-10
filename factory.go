package main

import (
	"sync"

	"github.com/sinisterminister/coinfactory"
)

func newSpreadPlayerProcessor(symbol *coinfactory.Symbol) coinfactory.SymbolStreamProcessor {
	proc := SpreadPlayerProcessor{
		symbol:             symbol,
		openOrders:         []*coinfactory.Order{},
		staleOrders:        []*coinfactory.Order{},
		janitorQuitChannel: make(chan bool),
		openOrdersMux:      &sync.Mutex{},
	}

	proc.startOpenOrderJanitor()
	return &proc
}
