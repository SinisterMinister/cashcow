package main

import (
	"sync"

	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
)

func newSpreadPlayerProcessor(symbol binance.Symbol) coinfactory.SymbolStreamProcessor {
	proc := SpreadPlayerProcessor{
		symbol:             symbol,
		openOrders:         []*coinfactory.Order{},
		janitorQuitChannel: make(chan bool),
		openOrdersMux:      &sync.Mutex{},
	}
	return &proc
}
