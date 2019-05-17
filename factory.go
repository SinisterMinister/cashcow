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

func newFollowTheLeaderProcessor(symbol *coinfactory.Symbol) coinfactory.SymbolStreamProcessor {
	proc := FollowTheLeaderProcessor{
		symbol:             symbol,
		openOrders:         []*coinfactory.Order{},
		staleOrders:        []*coinfactory.Order{},
		firstOrderCount:    0,
		secondOrderCount:   0,
		janitorQuitChannel: make(chan bool),
		openOrdersMux:      &sync.RWMutex{},
		staleOrdersMux:     &sync.RWMutex{},
		firstOrderMux:      &sync.RWMutex{},
		secondOrderMux:     &sync.RWMutex{},
		readyMutex:         &sync.RWMutex{},
	}
	proc.init()
	return &proc
}
