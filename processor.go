package main

import (
	"math"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/viper"

	"github.com/shopspring/decimal"
	"github.com/sinisterminister/coinfactory"
	"github.com/sinisterminister/coinfactory/pkg/binance"
	log "github.com/sirupsen/logrus"
)

type SpreadPlayerProcessor struct {
	symbol             binance.Symbol
	openOrders         []*coinfactory.Order
	janitorQuitChannel chan bool
}

var (
	tradeFee = decimal.NewFromFloat(0.002)
	cf       coinfactory.Coinfactory
)

func (p *SpreadPlayerProcessor) ProcessData(data binance.SymbolTickerData) {

	if len(p.openOrders) == 0 && isViable(data) {

		go p.placeQuoteBasedOrders(data)
		go p.placeAssetBasedOrders(data)
	}
}

func (p *SpreadPlayerProcessor) startOpenOrderJanitor() {
	timer := time.NewTicker(15 * time.Second)

	// Intercept the interrupt signal and pass it along
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go func() {
		select {
		case <-timer.C:
			for i, o := range p.openOrders {
				switch o.GetStatus().Status {
				// Skip this cases
				case "NEW":
				case "PARTIALLY_FILLED":
				case "PENDING_CANCEL":
				// Delete the rest
				default:
					p.openOrders = append(p.openOrders[:i], p.openOrders[i+1:]...)
				}
			}
		case <-interrupt:
			p.janitorQuitChannel <- true
		case <-p.janitorQuitChannel:
			return
		}
	}()
}

func (p *SpreadPlayerProcessor) placeQuoteBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildQuoteBasedBuyOrderRequests(data)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	// executeTestOrders(buy, sell)

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
}

func (p *SpreadPlayerProcessor) placeAssetBasedOrders(data binance.SymbolTickerData) {
	buy, sell := p.buildAssetBasedBuyOrderRequests(data)

	if ok := validateOrderPair(buy, sell); !ok {
		return
	}

	// executeTestOrders(buy, sell)

	buyOrder, sellOrder, err := executeOrders(buy, sell)
	if err != nil {
		// Nothing to do
		return
	}

	p.openOrders = append(p.openOrders, buyOrder)
	p.openOrders = append(p.openOrders, sellOrder)
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

	// Bail out if not nominal order
	mn := binance.GetSymbol(buy.Symbol).Filters.MinimumNotional.MinNotional

	if bv.LessThan(mn) || sv.LessThan(mn) {
		log.WithFields(log.Fields{
			"buy":  buy,
			"sell": sell,
			"mn":   mn,
		}).Info("Skipping sub-notional order")
		return false
	}

	return true
}

func newSpreadPlayerProcessor(symbol binance.Symbol) coinfactory.SymbolStreamProcessor {
	proc := SpreadPlayerProcessor{symbol: symbol}
	return &proc
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
	targetSpread := spread.Sub(tradeFee).Mul(bufferPercent).Add(tradeFee)
	txMargin := spread.Sub(targetSpread).Mul(data.BidPrice).Div(decimal.NewFromFloat(2))
	buyAt := data.BidPrice.Add(txMargin)
	sellAt := data.AskPrice.Sub(txMargin)
	potentialReturn := sellAt.Sub(buyAt).Mul(txQty)

	log.WithFields(log.Fields{
		"B":  data.BidPrice.Round(8).StringFixed(6),
		"A":  data.AskPrice.Round(8).StringFixed(6),
		"A%": askPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"B%": bidPercent.Mul(decimal.NewFromFloat(100)).Round(8).StringFixed(6),
		"R":  potentialReturn.Round(8).StringFixed(6),
		"Q":  txQty.Round(8).StringFixed(6),
		"B@": buyAt.Round(8).StringFixed(6),
		"S@": sellAt.Round(8).StringFixed(6),
	}).Info("Asset based ticker for ", symbol.BaseAsset)

	buyQty := txQty
	sellQty := txQty
	buyPrice := normalizePrice(buyAt, p.symbol)
	sellPrice := normalizePrice(sellAt, p.symbol)

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
	quoteSpread := quoteAsk.Sub(quoteBid).Div(quoteBid)
	bufferPercent := decimal.NewFromFloat((viper.GetFloat64("spreadprocessor.bufferpercent")))
	txQty := data.QuoteVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := getSpread(data).Sub(tradeFee).Mul(bufferPercent).Add(tradeFee)
	txMargin := quoteSpread.Sub(targetSpread).Mul(quoteBid).Div(decimal.NewFromFloat(2))
	sellAt := quoteBid.Add(txMargin)
	buyAt := quoteAsk.Sub(txMargin)
	potentialReturn := buyAt.Sub(sellAt).Mul(txQty)
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
	}).Info("Quote based ticker for ", symbol.QuoteAsset)

	buyQty := txQty.Mul(buyAt)
	sellQty := txQty.Mul(sellAt)
	buyPrice := decimal.NewFromFloat(1).Div(sellAt)
	sellPrice := decimal.NewFromFloat(1).Div(buyAt)
	buyPrice = normalizePrice(buyPrice, p.symbol)
	sellPrice = normalizePrice(sellPrice, p.symbol)

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
