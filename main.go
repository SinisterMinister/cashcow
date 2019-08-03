package main

import (
	"os"
	"os/signal"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
)

var (
	makerFee decimal.Decimal
	takerFee decimal.Decimal
	tradeFee decimal.Decimal
)

func main() {
	// First thing's first, load configs
	setupDefaultConfigValues()

	// Fetch the fees
	fetchFees()

	// Create a stop channel to handle exiting
	stopChan := make(chan bool)

	// Now, let's get some symbols to play with
	symbolNames := fetchWatchedSymbols()

	// For each symbol name, we'll spin up a go routine to process its trade data
	for _, name := range symbolNames {
		// Create the symbol so we can fetch its data
		symbol := coinfactory.GetSymbolService().GetSymbol(name)

		// Create the processor to do the work
		processor := &FollowTheLeaderProcessor{Symbol: symbol}

		// Kick off the ticker stream processor routine
		processor.Launch(stopChan)
	}

	// Start coinfactory
	coinfactory.Start()

	// Intercept the interrupt signal and pass it along
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Wait for an interrupt to exit
	select {
	case <-interrupt:
		close(stopChan)
		coinfactory.Shutdown()
	}

}
