package main

import (
	"os"
	"os/signal"

	"github.com/sinisterminister/coinfactory"
)

func main() {
	// First thing's first, load config
	setDefaultConfigValues()

	// Create a stop channel to handle exiting
	stopChan := make(chan bool)

	// Now, let's get some symbols to play with
	symbolNames := fetchWatchedSymbols()

	// For each symbol name, we'll spin up a go routine to process its trade data
	for _, name := range symbolNames {
		// Create the symbol so we can fetch its data
		symbol := coinfactory.GetSymbolService().GetSymbol(name)

		// Create the processor to do the work
		processor := &Processor{Symbol: symbol}

		// Kick off the ticker stream processor routine
		go processor.handleTickerStreamData(stopChan)
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
