package main

import (
	"os"
	"os/signal"

	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"
)

var SymbolService coinfactory.SymbolService

func main() {
	setDefaultConfigValues()
	SymbolService = coinfactory.GetSymbolService()
	cf = coinfactory.NewCoinFactory(newSpreadPlayerProcessor)
	cf.Start()

	// Intercept the interrupt signal and pass it along
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	select {
	case <-interrupt:
		cf.Stop()
	}
}

func setDefaultConfigValues() {
	viper.SetDefault("spreadprocessor.bufferPercent", .50)
	viper.SetDefault("spreadprocessor.fallbackQuantityBalancePercent", .45)
	viper.SetDefault("spreadprocessor.markOrderAsStaleAfter", "5m")
	viper.SetDefault("spreadprocessor.cancelOrderAfter", "4h")
}
