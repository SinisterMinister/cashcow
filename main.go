package main

import (
	"io/ioutil"
	"os"
	"os/signal"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"

	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"
)

var (
	SymbolService coinfactory.SymbolService
	cf            coinfactory.Coinfactory
	makerFee      decimal.Decimal
	takerFee      decimal.Decimal
	tradeFee      decimal.Decimal
)

func main() {
	setDefaultConfigValues()
	SymbolService = coinfactory.GetSymbolService()
	cf = coinfactory.NewCoinFactory(newFollowTheLeaderProcessor)
	data, err := binance.GetUserData()
	if err != nil {
		tradeFee = decimal.NewFromFloat(.002)
	}
	makerFee = decimal.NewFromFloat(float64(data.MakerCommission) / float64(10000))
	takerFee = decimal.NewFromFloat(float64(data.TakerCommission) / float64(10000))
	tradeFee = makerFee.Add(takerFee)
	log.WithField("tradeFee", tradeFee).Info("trade fee percentage")
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

	viper.SetDefault("followtheleaderprocessor.percentReturnTarget", .001)
	viper.SetDefault("followtheleaderprocessor.fallbackQuantityBalancePercent", .20)
	viper.SetDefault("followtheleaderprocessor.markOrderAsStaleAfter", "1m")
	viper.SetDefault("followtheleaderprocessor.cancelOrderAfter", "4h")
	viper.SetDefault("followtheleaderprocessor.maxStaleOrders", 4)

	// Setup tempdir
	dir, err := ioutil.TempDir("", "cashcow")
	if err != nil {
		dir = os.TempDir()
	}

	viper.SetDefault("scribbledb.dir", dir)
}
