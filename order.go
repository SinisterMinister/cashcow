package main

import (
	"github.com/sinisterminister/coinfactory"
	"github.com/spf13/viper"
)

type Order struct {
	coinfactory.Order
}

func (o *Order) IsStale() bool {
	maxAge := viper.GetDuration("followTheLeaderProcessor.markOrderAsStaleAfter").Nanoseconds()
	return o.GetAge().Nanoseconds() > maxAge
}
