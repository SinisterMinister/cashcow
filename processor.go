package main

import (
	"os"
	"os/signal"
	"sync"
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
	openOrdersMux      *sync.Mutex
}

var (
	tradeFee = decimal.NewFromFloat(0.001)
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
		for {
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

func (p *SpreadPlayerProcessor) buildAssetBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	spread := getSpread(data)
	bufferPercent := decimal.NewFromFloat(viper.GetFloat64("spreadprocessor.bufferpercent"))
	txQty := data.BaseVolume.Div(decimal.NewFromFloat(float64(data.TotalNumberOfTrades)))
	targetSpread := spread.Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
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
	targetSpread := getSpread(data).Sub(tradeFee.Mul(decimal.NewFromFloat(2))).Mul(bufferPercent).Add(tradeFee.Mul(decimal.NewFromFloat(2)))
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
	sellPrice := decimal.NewFromFloat(1).Div(sellAt)
	buyPrice := decimal.NewFromFloat(1).Div(buyAt)
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
