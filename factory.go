package main

import (
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
)

func newSpreadPlayerProcessor(symbol binance.Symbol) coinfactory.SymbolStreamProcessor {
	proc := SpreadPlayerProcessor{symbol: symbol}
	return &proc
}
