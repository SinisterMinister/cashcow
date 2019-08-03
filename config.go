package main

import (
	"io/ioutil"
	"os"

	"github.com/spf13/viper"
)

func setupDefaultConfigValues() {
	// Set how long it takes for an order to be marked stale
	viper.SetDefault("followTheLeaderProcessor.markOrderAsStaleAfter", "5m")
	viper.SetDefault("followTheLeaderProcessor.firstOrderTimeout", "30s")
	viper.SetDefault("followtheleaderprocessor.percentReturnTarget", "0.001")
	viper.SetDefault("followtheleaderprocessor.reaperSpreadMultiplier", "3")

	// Setup tempdir
	dir, err := ioutil.TempDir("", "cashcow")
	if err != nil {
		dir = os.TempDir()
	}

	viper.SetDefault("scribbledb.path", dir)
}
