package main

import (
	"os"
	"os/signal"

	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"
)

func main() {
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
	viper.SetDefault("spreadprocessor.bufferpercent", .50)
	viper.SetDefault("spreadprocessor.fallbackQuantityBalancePercent", .9)
}
