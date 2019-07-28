package main

import (
	"io/ioutil"
	"os"

	"github.com/spf13/viper"
)

func setDefaultConfigValues() {

	// Setup tempdir
	dir, err := ioutil.TempDir("", "cashcow")
	if err != nil {
		dir = os.TempDir()
	}

	viper.SetDefault("scribbledb.path", dir)
}
