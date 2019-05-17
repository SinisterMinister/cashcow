package main

import (
	"errors"
	"sync"

	"github.com/spf13/viper"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

type UnstableTradeConditionError struct {
	msg string
}

func (err UnstableTradeConditionError) Error() string {
	return err.msg
}

type SpreadPlayerProcessor struct {
	symbol             *coinfactory.Symbol
	openOrders         []*coinfactory.Order
	staleOrders        []*coinfactory.Order
	janitorQuitChannel chan bool
	openOrdersMux      *sync.Mutex
}

func (p *SpreadPlayerProcessor) ProcessData(data binance.SymbolTickerData) {
	if len(p.openOrders) == 0 && isViable(data) {
		go p.placeCombinedTestOrder(data)
	}
}

func (p *SpreadPlayerProcessor) startOpenOrderJanitor() {
	log.Info("Starting order janitor")
	go startJanitor(p)
}

func (p *SpreadPlayerProcessor) placeQuoteBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildQuoteBasedBuyOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	// executeTestOrders(buy, sell)

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeAssetBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildAssetBasedBuyOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeCombinedOrder(data binance.SymbolTickerData) {
	buy, sell := p.buildCombinedOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrdersMux.Lock()
	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
	p.openOrdersMux.Unlock()
}

func (p *SpreadPlayerProcessor) placeCombinedTestOrder(data binance.SymbolTickerData) {
	buy, sell := p.buildCombinedOrderRequests(data)

	// Adjust the orders based on the trend, available funds, and order filters
	p.adjustOrdersQuantityBasedOnAvailableFunds(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	p.adjustOrdersPriceBasedOnTrix(&buy, &sell)
	p.normalizeOrders(&buy, &sell)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	executeTestOrders(buy, sell)
}

func (p *SpreadPlayerProcessor) adjustOrdersPriceBasedOnTrix(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) (err error) {
	ma, trixOsc, err := p.symbol.GetCurrentTrixIndicator("1m", 5)
	if err != nil {
		return err
	}

	// We're checking to see if the oscillator and the percent change between the
	// moving average and the current price are heading the same direction
	maDec := decimal.NewFromFloat(ma)
	oscDec := decimal.NewFromFloat(trixOsc)
	testOsc := p.symbol.GetTicker().CurrentClosePrice.Sub(maDec).Div(maDec)

	if !(testOsc.GreaterThanOrEqual(decimal.Zero) && oscDec.GreaterThanOrEqual(decimal.Zero)) && !(testOsc.LessThanOrEqual(decimal.Zero) && oscDec.LessThanOrEqual(decimal.Zero)) {
		return errors.New("price too unstable")
	}

	// Adjust order based on oscillator for now
	buyOrder.Price = buyOrder.Price.Add(buyOrder.Price.Mul(oscDec))
	sellOrder.Price = sellOrder.Price.Add(sellOrder.Price.Mul(oscDec))

	log.WithFields(log.Fields{
		"symbol":       p.symbol.Symbol,
		"ma":           maDec.StringFixed(8),
		"oscillator":   oscDec.StringFixed(8),
		"test":         testOsc.StringFixed(8),
		"currentPrice": p.symbol.GetTicker().CurrentClosePrice.StringFixed(8),
		"bid":          p.symbol.GetTicker().BidPrice.StringFixed(8),
		"ask":          p.symbol.GetTicker().AskPrice.StringFixed(8),
		"buyPrice":     buyOrder.Price.StringFixed(8),
		"sellPrice":    sellOrder.Price.StringFixed(8),
	}).Info("TRIX Indicator")

	return err
}

func (p *SpreadPlayerProcessor) adjustOrdersQuantityBasedOnAvailableFunds(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) {
	// Check balances and adjust quantity if necessary
	quoteBalance := cf.GetBalanceManager().GetAvailableBalance(p.symbol.QuoteAsset)
	baseBalance := cf.GetBalanceManager().GetAvailableBalance(p.symbol.BaseAsset)

	log.WithFields(log.Fields{
		p.symbol.BaseAsset:  baseBalance,
		p.symbol.QuoteAsset: quoteBalance,
	}).Debug("Wallet balances")

	if buyOrder.Quantity.Mul(buyOrder.Price).GreaterThan(quoteBalance) {
		adjPercent := quoteBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(buyOrder.Price).Div(buyOrder.Quantity)
		buyOrder.Quantity = buyOrder.Quantity.Mul(adjPercent)
		sellOrder.Quantity = sellOrder.Quantity.Mul(adjPercent)
	}

	if sellOrder.Quantity.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(sellOrder.Quantity)
		buyOrder.Quantity = buyOrder.Quantity.Mul(adjPercent)
		sellOrder.Quantity = sellOrder.Quantity.Mul(adjPercent)
	}
}

func (p *SpreadPlayerProcessor) normalizeOrders(buyOrder *coinfactory.OrderRequest, sellOrder *coinfactory.OrderRequest) {
	// Normalize the price
	buyOrder.Price = normalizePrice(buyOrder.Price, p.symbol)
	sellOrder.Price = normalizePrice(sellOrder.Price, p.symbol)

	// Normalize quantities
	buyOrder.Quantity = normalizeQuantity(buyOrder.Quantity, p.symbol)
	sellOrder.Quantity = normalizeQuantity(sellOrder.Quantity, p.symbol)
}

func (p *SpreadPlayerProcessor) buildCombinedOrderRequests(data binance.SymbolTickerData) (buyOrder coinfactory.OrderRequest, sellOrder coinfactory.OrderRequest) {
	// Get orders
	assetBasedBuy, assetBasedSell := p.buildAssetBasedBuyOrderRequests(data)
	quoteBasedBuy, quoteBasedSell := p.buildQuoteBasedBuyOrderRequests(data)

	// Create combined orders
	buyOrder = coinfactory.OrderRequest{}
	buyOrder.Symbol = p.symbol.Symbol
	buyOrder.Side = "BUY"
	buyOrder.Quantity = assetBasedBuy.Quantity.Add(quoteBasedBuy.Quantity)
	buyOrder.Price = assetBasedBuy.Price

	sellOrder = coinfactory.OrderRequest{}
	sellOrder.Symbol = p.symbol.Symbol
	sellOrder.Side = "SELL"
	sellOrder.Quantity = quoteBasedSell.Quantity.Add(assetBasedSell.Quantity)
	sellOrder.Price = assetBasedSell.Price

	return buyOrder, sellOrder
}

func (p *SpreadPlayerProcessor) buildAssetBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	spread := getSpread(data)
	bufferPercent := decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.bufferPercent"))
	txQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := spread.Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
	txMargin := spread.Sub(targetSpread).Mul(data.BidPrice).Div(decimal.NewFromFloat(2))
	buyAt := data.BidPrice.Add(txMargin)
	sellAt := data.AskPrice.Sub(txMargin)
	potentialReturn := sellAt.Sub(buyAt).Mul(txQty)

	log.WithFields(log.Fields{
		"B":  data.BidPrice.Round(8).StringFixed(8),
		"A":  data.AskPrice.Round(8).StringFixed(8),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"R":  potentialReturn.Round(8).StringFixed(8),
		"Q":  txQty.Round(8).StringFixed(8),
		"B@": buyAt.Round(8).StringFixed(8),
		"S@": sellAt.Round(8).StringFixed(8),
	}).Info("Asset based ticker for ", symbol.BaseAsset)

	buyQty := txQty
	sellQty := txQty
	buyPrice := normalizePrice(buyAt, p.symbol)
	sellPrice := normalizePrice(sellAt, p.symbol)

	buyOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}

func (p *SpreadPlayerProcessor) buildQuoteBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	quoteBid := decimal.NewFromFloat(1).Div(data.AskPrice)
	quoteAsk := decimal.NewFromFloat(1).Div(data.BidPrice)
	quoteSpread := quoteAsk.Sub(quoteBid).Div(quoteBid)
	bufferPercent := decimal.NewFromFloat((viper.GetFloat64("spreadprocessor.bufferPercent")))
	txQty := data.QuoteVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := getSpread(data).Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
	txMargin := quoteSpread.Sub(targetSpread).Mul(quoteBid).Div(decimal.NewFromFloat(2))
	sellAt := quoteBid.Add(txMargin)
	buyAt := quoteAsk.Sub(txMargin)
	potentialReturn := buyAt.Sub(sellAt).Mul(txQty)
	returnInQuote := potentialReturn.Mul(data.AskPrice)

	log.WithFields(log.Fields{
		"B":  quoteBid.Round(8).StringFixed(8),
		"A":  quoteAsk.Round(8).StringFixed(8),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(8),
		"R":  potentialReturn.Round(8).StringFixed(8),
		"r":  returnInQuote.Round(8).StringFixed(8),
		"Q":  txQty.Round(8).StringFixed(8),
		"B@": buyAt.Round(8).StringFixed(8),
		"S@": sellAt.Round(8).StringFixed(8),
	}).Info("Quote based ticker for ", symbol.QuoteAsset)

	buyQty := txQty.Mul(buyAt)
	sellQty := txQty.Mul(sellAt)
	sellPrice := decimal.NewFromFloat(1).Div(sellAt)
	buyPrice := decimal.NewFromFloat(1).Div(buyAt)
	buyPrice = normalizePrice(buyPrice, p.symbol)
	sellPrice = normalizePrice(sellPrice, p.symbol)

	buyOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "BUY",
		Quantity: buyQty,
		Price:    buyPrice,
	}

	sellOrder := coinfactory.OrderRequest{
		Symbol:   symbol.Symbol,
		Side:     "SELL",
		Quantity: sellQty,
		Price:    sellPrice,
	}

	return buyOrder, sellOrder
}
