package main

import (
	"strings"

	"github.com/shopspring/decimal"

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

func getTradeFee() decimal.Decimal {
	var (
		makerFee decimal.Decimal
		takerFee decimal.Decimal
		tradeFee decimal.Decimal
	)
	data, err := binance.GetUserData()
	if err != nil {
		tradeFee = decimal.NewFromFloat(.002)
	}
	makerFee = decimal.NewFromFloat(float64(data.MakerCommission) / float64(10000))
	takerFee = decimal.NewFromFloat(float64(data.TakerCommission) / float64(10000))
	tradeFee = makerFee.Add(takerFee)

	return tradeFee
}
