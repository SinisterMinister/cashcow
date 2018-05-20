package main

import (
	"math"

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
		buy, sell := p.buildQuoteBasedBuyOrderRequests(data)

		// Bail on zero value trades
		if buy.Quantity.Equals(decimal.Zero) || sell.Quantity.Equals(decimal.Zero) {
			log.WithFields(log.Fields{
				"buy":  buy,
				"sell": sell,
			}).Info("Skipping zero value order")
			return
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
			return
		}

		buyOrder, err := cf.GetOrderManager().AttemptOrder(buy)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			return
		}

		sellOrder, err := cf.GetOrderManager().AttemptOrder(sell)
		if err != nil {
			log.WithError(err).Error("Could not place order!")
			// Cancel buy order
			err := cf.GetOrderManager().CancelOrder(buyOrder)
			if err != nil {
				log.WithError(err).Error("Could not cancel previous order!")
			}
		}

		p.orderPair = OrderPair{buyOrder, sellOrder}
		p.orderInProgress = true
	}
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

func (p *SpreadPlayerProcessor) buildQuoteBasedBuyOrderRequests(data binance.SymbolTickerData) (coinfactory.OrderRequest, coinfactory.OrderRequest) {
	symbol := binance.GetSymbol(data.Symbol)
	askPercent := data.AskPrice.Sub(data.BidPrice).Div(data.AskPrice)
	bidPercent := data.AskPrice.Sub(data.BidPrice).Div(data.BidPrice)
	quoteBid := decimal.NewFromFloat(1).Div(data.AskPrice)
	quoteAsk := decimal.NewFromFloat(1).Div(data.BidPrice)
	quoteSpread := quoteAsk.Sub(quoteBid)
	bufferPercent := decimal.NewFromFloat(.50)
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
	}).Info("Ticker for ", symbol.Symbol)

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
		adjPercent := quoteBalance.Mul(decimal.NewFromFloat(.9)).Div(buyPrice).Div(buyQty)
		buyQty = buyQty.Mul(adjPercent)
		sellQty = sellQty.Mul(adjPercent)
	}

	if sellQty.GreaterThan(baseBalance) {
		adjPercent := baseBalance.Mul(decimal.NewFromFloat(.9)).Div(sellQty)
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
