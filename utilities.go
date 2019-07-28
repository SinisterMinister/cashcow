package main

import (
	"strings"

	"github.com/sinisterminister/coinfactory/pkg/binance"
	"github.com/spf13/viper"
)

func filterSymbols(symbols []string) []string {
	filter := func(symbol string) bool {
		sym := viper.GetStringSlice("watchedSymbols")
		c := 0
		for _, s := range sym {
			if strings.Contains(symbol, s) {
				c++
			}
		}
		return c >= 2
	}
	filtered := []string{}
	for _, s := range symbols {
		if filter(s) {
			filtered = append(filtered, s)
		}
	}

	return filtered
}

func fetchWatchedSymbols() []string {
	return filterSymbols(binance.GetSymbolsAsStrings())
}
