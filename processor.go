package main

import (
	"math"

	"github.com/spf13/viper"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

type SpreadPlayerProcessor struct {
	symbol          binance.Symbol
	orderInProgress bool
	orderPair       OrderPair
}

type OrderPair struct {
	buyOrder  *coinfactory.Order
	sellOrder *coinfactory.Order
}

var (
	tradeFee = decimal.NewFromFloat(0.002)
	cf       coinfactory.Coinfactory
)

func (p *SpreadPlayerProcessor) ProcessData(data binance.SymbolTickerData) {

	if !p.orderInProgress && isViable(data) {

		go p.placeQuoteBasedOrders(data)
		go p.placeAssetBasedOrders(data)
		p.orderInProgress = true
	}
}

func (p *SpreadPlayerProcessor) placeQuoteBasedOrders(data binance.SymbolTickerData) (OrderPair, error) {
	buy, sell := p.buildQuoteBasedBuyOrderRequests(data)

	if ok := validateOrderPair(buy, sell); !ok {
		return OrderPair{}, nil
	}

	executeTestOrders(buy, sell)
	return OrderPair{}, nil

	// buyOrder, sellOrder, err := executeOrders(buy, sell)
	// if err != nil {
	// 	// Nothing to do
	// 	return OrderPair{}, nil
	// }

	// return OrderPair{buyOrder, sellOrder}, nil
}

func (p *SpreadPlayerProcessor) placeAssetBasedOrders(data binance.SymbolTickerData) (OrderPair, error) {
	buy, sell := p.buildAssetBasedBuyOrderRequests(data)

	if ok := validateOrderPair(buy, sell); !ok {
		return OrderPair{}, nil
	}

	executeTestOrders(buy, sell)
	return OrderPair{}, nil
	// buyOrder, sellOrder, err := executeOrders(buy, sell)
	// if err != nil {
	// 	// Nothing to do
	// 	return OrderPair{}, nil
	// }

	// return OrderPair{buyOrder, sellOrder}, nil
}

func executeOrders(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) (*coinfactory.Order, *coinfactory.Order, error) {
	buyOrder, err := cf.GetOrderManager().AttemptOrder(buy)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
		return &coinfactory.Order{}, &coinfactory.Order{}, err
	}

	sellOrder, err0 := cf.GetOrderManager().AttemptOrder(sell)
	if err0 != nil {
		log.WithError(err0).Error("Could not place order!")
		// Cancel buy order
		err1 := cf.GetOrderManager().CancelOrder(buyOrder)
		if err1 != nil {
			log.WithError(err1).Error("Could not cancel previous order!")
			return &coinfactory.Order{}, &coinfactory.Order{}, err1
		}
		return &coinfactory.Order{}, &coinfactory.Order{}, err0
	}

	return buyOrder, sellOrder, nil
}

func executeTestOrders(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) {
	var err error
	err = cf.GetOrderManager().AttemptTestOrder(buy)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
	}

	err = cf.GetOrderManager().AttemptTestOrder(sell)
	if err != nil {
		log.WithError(err).Error("Could not place order!")
	}
}

func validateOrderPair(buy coinfactory.OrderRequest, sell coinfactory.OrderRequest) bool {
	// Bail on zero value trades
	if buy.Quantity.Equals(decimal.Zero) || sell.Quantity.Equals(decimal.Zero) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
		}).Info("Skipping zero value order")
		return false
	}

	// Validate the orders are still viable
	bv := buy.Price.Mul(buy.Quantity)
	sv := sell.Price.Mul(sell.Quantity)
	tc := bv.Mul(tradeFee).Add(sv.Mul(tradeFee))
	r := sv.Sub(bv)

	// Bail out if we make no money
	if r.LessThan(tc) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
		}).Info("Skipping negative return order")
		return false
	}

	return true
}

func newSpreadPlayerProcessor(symbol binance.Symbol) coinfactory.SymbolStreamProcessor {
	proc := SpreadPlayerProcessor{symbol: symbol}
	return &proc
}

func getExchangeList() {
	// Get exchange info from the API
	info := binance.GetExchangeInfo()

	// Extract the symbols from the list for printing
	log.Info("Total Exchanges: ", len(info.Symbols))
	log.Info("Symbols: ", binance.GetSymbols())
}

func main() {
	cf = coinfactory.NewCoinFactory(newSpreadPlayerProcessor)

	getExchangeList()
	cf.Start()
}

func setDefaultConfigValues() {
	viper.SetDefault("spreadprocessor.bufferpercent", .50)
	viper.SetDefault("spreadprocessor.fallbackQuantityBalancePercent", .9)
}

func isViable(data binance.SymbolTickerData) bool {
	return getSpread(data).GreaterThan(tradeFee)
}

func getSpread(data binance.SymbolTickerData) decimal.Decimal {
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	if askPercent.LessThan(bidPercent) {
		return askPercent
	}
	return bidPercent
}

func logTicker(data binance.SymbolTickerData) {
	// askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	// bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
}

func (p *SpreadPlayerProcessor) buildAssetBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	spread := getSpread(data)
	bufferPercent := decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.bufferpercent"))
	txQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := spread.Sub(tradeFee).Mul(bufferPercent).Add(tradeFee).Mul(data.BidPrice)
	txMargin := spread.Sub(targetSpread).Div(decimal.NewFromFloat(2))
	buyAt := data.BidPrice.Add(txMargin)
	sellAt := data.AskPrice.Sub(txMargin)
	potentialReturn := sellAt.Sub(buyAt).Mul(txQty)
	returnInQuote := potentialReturn.Mul(data.AskPrice)

	log.WithFields(log.Fields{
		"B":  data.BidPrice.Round(8).StringFixed(6),
		"A":  data.AskPrice.Round(8).StringFixed(6),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"R":  potentialReturn.Round(8).StringFixed(6),
		"r":  returnInQuote.Round(8).StringFixed(6),
		"Q":  txQty.Round(8).StringFixed(6),
		"B@": buyAt.Round(8).StringFixed(6),
		"S@": sellAt.Round(8).StringFixed(6),
	}).Info("Ticker for ", symbol.BaseAsset)

	buyQty := txQty
	sellQty := txQty
	buyPrice := normalizePrice(decimal.NewFromFloat(1).Div(sellAt), p.symbol)
	sellPrice := normalizePrice(decimal.NewFromFloat(1).Div(buyAt), p.symbol)

	// Check balances and adjust quantity if necessary
	sym := binance.GetSymbol(symbol.Symbol)
	quoteBalance := cf.GetBalanceManager().GetAvailableBalance(sym.QuoteAsset)
	baseBalance := cf.GetBalanceManager().GetAvailableBalance(sym.BaseAsset)

	log.WithFields(log.Fields{
		sym.BaseAsset:  baseBalance,
		sym.QuoteAsset: quoteBalance,
	}).Debug("Wallet balances")

	if buyQty.Mul(buyPrice).GreaterThan(quoteBalance) {
		adjPercent := quoteBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(buyPrice).Div(buyQty)
		buyQty = buyQty.Mul(adjPercent)
		sellQty = sellQty.Mul(adjPercent)
	}

	if sellQty.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(sellQty)
		buyQty = buyQty.Mul(adjPercent)
		sellQty = sellQty.Mul(adjPercent)
	}

	// Normalize quantities
	buyQty = normalizeQuantity(buyQty, p.symbol)
	sellQty = normalizeQuantity(sellQty, p.symbol)

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
	quoteSpread := quoteAsk.Sub(quoteBid)
	bufferPercent := decimal.NewFromFloat((viper.GetFloat64("spreadprocessor.bufferpercent")))
	txQty := data.QuoteVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := getSpread(data).Sub(tradeFee).Mul(bufferPercent).Add(tradeFee).Mul(quoteBid)
	txMargin := quoteSpread.Sub(targetSpread).Div(decimal.NewFromFloat(2))
	buyAt := quoteBid.Add(txMargin)
	sellAt := quoteAsk.Sub(txMargin)
	potentialReturn := sellAt.Sub(buyAt).Mul(txQty)
	returnInQuote := potentialReturn.Mul(data.AskPrice)

	log.WithFields(log.Fields{
		"B":  quoteBid.Round(8).StringFixed(6),
		"A":  quoteAsk.Round(8).StringFixed(6),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"R":  potentialReturn.Round(8).StringFixed(6),
		"r":  returnInQuote.Round(8).StringFixed(6),
		"Q":  txQty.Round(8).StringFixed(6),
		"B@": buyAt.Round(8).StringFixed(6),
		"S@": sellAt.Round(8).StringFixed(6),
	}).Info("Ticker for ", symbol.QuoteAsset)

	buyQty := txQty.Mul(buyAt)
	sellQty := txQty.Mul(sellAt)
	buyPrice := normalizePrice(decimal.NewFromFloat(1).Div(sellAt), p.symbol)
	sellPrice := normalizePrice(decimal.NewFromFloat(1).Div(buyAt), p.symbol)

	// Check balances and adjust quantity if necessary
	sym := binance.GetSymbol(symbol.Symbol)
	quoteBalance := cf.GetBalanceManager().GetAvailableBalance(sym.QuoteAsset)
	baseBalance := cf.GetBalanceManager().GetAvailableBalance(sym.BaseAsset)

	log.WithFields(log.Fields{
		sym.BaseAsset:  baseBalance,
		sym.QuoteAsset: quoteBalance,
	}).Debug("Wallet balances")

	if buyQty.Mul(buyPrice).GreaterThan(quoteBalance) {
		adjPercent := quoteBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(buyPrice).Div(buyQty)
		buyQty = buyQty.Mul(adjPercent)
		sellQty = sellQty.Mul(adjPercent)
	}

	if sellQty.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.fallbackQuantityBalancePercent"))).Div(sellQty)
		buyQty = buyQty.Mul(adjPercent)
		sellQty = sellQty.Mul(adjPercent)
	}

	// Normalize quantities
	buyQty = normalizeQuantity(buyQty, p.symbol)
	sellQty = normalizeQuantity(sellQty, p.symbol)

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

func normalizePrice(price decimal.Decimal, symbol binance.Symbol) decimal.Decimal {
	// Get decimal places and round to that precision
	ts, _ := symbol.Filters.Price.TickSize.Float64()
	places := int32(math.Log10(ts)) * -1
	return price.Round(places)
}

func normalizeQuantity(qty decimal.Decimal, symbol binance.Symbol) decimal.Decimal {
	// Get decimal places and round to that precision
	ss, _ := symbol.Filters.LotSize.StepSize.Float64()
	places := int32(math.Log10(ss)) * -1
	return qty.Round(places)
}
