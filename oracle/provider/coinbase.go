package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ojo-network/price-feeder/oracle/types"
)

const (
	coinbaseWSHost    = "ws-feed.exchange.coinbase.com"
	coinbasePingCheck = time.Second * 28 // should be < 30
	coinbaseRestHost  = "https://api.exchange.coinbase.com"
	coinbaseRestPath  = "/products"
	coinbaseTimeFmt   = "2006-01-02T15:04:05.000000Z"
	unixMinute        = 60000
)

var _ Provider = (*CoinbaseProvider)(nil)

type (
	// CoinbaseProvider defines an Oracle provider implemented by the Coinbase public
	// API.
	//
	// REF: https://www.coinbase.io/docs/websocket/index.html
	CoinbaseProvider struct {
		wsc             *WebsocketController
		logger          zerolog.Logger
		reconnectTimer  *time.Ticker
		mtx             sync.RWMutex
		endpoints       Endpoint
		trades          map[string][]CoinbaseTrade    // Symbol => []CoinbaseTrade
		tickers         map[string]CoinbaseTicker     // Symbol => CoinbaseTicker
		subscribedPairs map[string]types.CurrencyPair // Symbol => types.CurrencyPair
	}

	// CoinbaseSubscriptionMsg Msg to subscribe to all channels.
	CoinbaseSubscriptionMsg struct {
		Type       string   `json:"type"`        // ex. "subscribe"
		ProductIDs []string `json:"product_ids"` // streams to subscribe ex.: ["BOT-USDT", ...]
		Channels   []string `json:"channels"`    // channels to subscribe to ex.: "ticker"
	}

	// CoinbaseMatchResponse defines the response body for coinbase trades.
	CoinbaseTradeResponse struct {
		Type      string `json:"type"`       // "last_match" or "match"
		ProductID string `json:"product_id"` // ex.: ATOM-USDT
		Time      string `json:"time"`       // Time in format 2006-01-02T15:04:05.000000Z
		Size      string `json:"size"`       // Size of the trade ex.: 10.41
		Price     string `json:"price"`      // ex.: 14.02
	}

	// CoinbaseTrade defines the trade info we'd like to save.
	CoinbaseTrade struct {
		ProductID string // ex.: ATOM-USDT
		Time      int64  // Time in unix epoch ex.: 164732388700
		Size      string // Size of the trade ex.: 10.41
		Price     string // ex.: 14.02
	}

	// CoinbaseTicker defines the ticker info we'd like to save.
	CoinbaseTicker struct {
		ProductID string `json:"product_id"` // ex.: ATOM-USDT
		Price     string `json:"price"`      // ex.: 523.0
		Volume    string `json:"volume_24h"` // 24-hour volume
	}

	// CoinbaseErrResponse defines the response body for errors.
	CoinbaseErrResponse struct {
		Type   string `json:"type"`   // should be "error"
		Reason string `json:"reason"` // ex.: "tickers" is not a valid channel
	}

	// CoinbasePairSummary defines the response structure for a Coinbase pair summary.
	CoinbasePairSummary struct {
		Base  string `json:"base_currency"`
		Quote string `json:"quote_currency"`
	}
)

// NewCoinbaseProvider creates a new CoinbaseProvider.
func NewCoinbaseProvider(
	ctx context.Context,
	logger zerolog.Logger,
	endpoints Endpoint,
	pairs ...types.CurrencyPair,
) (*CoinbaseProvider, error) {
	if endpoints.Name != ProviderCoinbase {
		endpoints = Endpoint{
			Name:      ProviderCoinbase,
			Rest:      coinbaseRestHost,
			Websocket: coinbaseWSHost,
		}
	}
	wsURL := url.URL{
		Scheme: "wss",
		Host:   endpoints.Websocket,
	}

	coinbaseLogger := logger.With().Str("provider", string(ProviderCoinbase)).Logger()

	provider := &CoinbaseProvider{
		logger:          coinbaseLogger,
		reconnectTimer:  time.NewTicker(coinbasePingCheck),
		endpoints:       endpoints,
		trades:          map[string][]CoinbaseTrade{},
		tickers:         map[string]CoinbaseTicker{},
		subscribedPairs: map[string]types.CurrencyPair{},
	}

	confirmedPairs, err := ConfirmPairAvailability(
		provider,
		provider.endpoints.Name,
		provider.logger,
		pairs...,
	)
	if err != nil {
		return nil, err
	}

	provider.setSubscribedPairs(confirmedPairs...)

	provider.wsc = NewWebsocketController(
		ctx,
		endpoints.Name,
		wsURL,
		provider.getSubscriptionMsgs(pairs...),
		provider.messageReceived,
		defaultPingDuration,
		websocket.PingMessage,
		coinbaseLogger,
	)

	return provider, nil
}

func (p *CoinbaseProvider) StartConnections() {
	p.wsc.StartConnections()
}

func (p *CoinbaseProvider) getSubscriptionMsgs(cps ...types.CurrencyPair) []interface{} {
	subscriptionMsgs := make([]interface{}, 0, 1)

	topics := make([]string, len(cps))
	index := 0

	for _, cp := range cps {
		topics[index] = currencyPairToCoinbasePair(cp)
		index++
	}
	msg := newCoinbaseSubscription(topics...)
	subscriptionMsgs = append(subscriptionMsgs, msg)
	return subscriptionMsgs
}

// SubscribeCurrencyPairs sends the new subscription messages to the websocket
// and adds them to the providers subscribedPairs array
func (p *CoinbaseProvider) SubscribeCurrencyPairs(cps ...types.CurrencyPair) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	newPairs := []types.CurrencyPair{}
	for _, cp := range cps {
		if _, ok := p.subscribedPairs[cp.String()]; !ok {
			newPairs = append(newPairs, cp)
		}
	}

	confirmedPairs, err := ConfirmPairAvailability(
		p,
		p.endpoints.Name,
		p.logger,
		newPairs...,
	)
	if err != nil {
		return
	}

	newSubscriptionMsgs := p.getSubscriptionMsgs(confirmedPairs...)
	p.wsc.AddWebsocketConnection(
		newSubscriptionMsgs,
		p.messageReceived,
		defaultPingDuration,
		websocket.PingMessage,
	)
	p.setSubscribedPairs(confirmedPairs...)
}

// GetTickerPrices returns the tickerPrices based on the provided pairs.
func (p *CoinbaseProvider) GetTickerPrices(pairs ...types.CurrencyPair) (map[string]types.TickerPrice, error) {
	tickerPrices := make(map[string]types.TickerPrice, len(pairs))

	tickerErrs := 0
	for _, cp := range pairs {
		price, err := p.getTickerPrice(cp)
		if err != nil {
			p.logger.Warn().Err(err)
			tickerErrs++
			continue
		}
		tickerPrices[cp.String()] = price
	}

	if tickerErrs == len(pairs) {
		return nil, fmt.Errorf(
			types.ErrNoTickers.Error(),
			p.endpoints.Name,
			pairs,
		)
	}
	return tickerPrices, nil
}

// GetCandlePrices returns candles based off of the saved trades map.
// Candles need to be cut up into one-minute intervals.
func (p *CoinbaseProvider) GetCandlePrices(pairs ...types.CurrencyPair) (map[string][]types.CandlePrice, error) {
	tradeMap := make(map[string][]CoinbaseTrade, len(pairs))

	tradeErrs := 0
	for _, cp := range pairs {
		key := currencyPairToCoinbasePair(cp)
		tradeSet, err := p.getTradePrices(key)
		if err != nil {
			p.logger.Warn().Err(err)
			tradeErrs++
			continue
		}
		tradeMap[key] = tradeSet
	}
	if tradeErrs == len(pairs) {
		return nil, fmt.Errorf(
			types.ErrNoTickers.Error(),
			p.endpoints.Name,
			pairs,
		)
	}

	candles := make(map[string][]types.CandlePrice)

	for cp := range tradeMap {
		trades := tradeMap[cp]
		// sort oldest -> newest
		sort.Slice(trades, func(i, j int) bool {
			return time.Unix(trades[i].Time, 0).Before(time.Unix(trades[j].Time, 0))
		})

		candleSlice := []types.CandlePrice{
			{
				Price:  sdk.ZeroDec(),
				Volume: sdk.ZeroDec(),
			},
		}
		startTime := trades[0].Time
		index := 0

		// divide into chunks by minute
		for _, trade := range trades {
			// every minute, reset the time period
			if trade.Time-startTime > unixMinute {
				index++
				startTime = trade.Time
				candleSlice = append(candleSlice, types.CandlePrice{
					Price:  sdk.ZeroDec(),
					Volume: sdk.ZeroDec(),
				})
			}

			size, err := sdk.NewDecFromStr(trade.Size)
			if err != nil {
				return nil, err
			}
			price, err := sdk.NewDecFromStr(trade.Price)
			if err != nil {
				return nil, err
			}

			volume := candleSlice[index].Volume.Add(size)
			candleSlice[index] = types.CandlePrice{
				Volume:    volume,     // aggregate size
				Price:     price,      // most recent price
				TimeStamp: trade.Time, // most recent timestamp
			}
		}

		candles[coinbasePairToCurrencyPair(cp)] = candleSlice
	}

	return candles, nil
}

// GetAvailablePairs returns all pairs to which the provider can subscribe.
func (p *CoinbaseProvider) GetAvailablePairs() (map[string]struct{}, error) {
	resp, err := http.Get(p.endpoints.Rest + coinbaseRestPath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var pairsSummary []CoinbasePairSummary
	if err := json.NewDecoder(resp.Body).Decode(&pairsSummary); err != nil {
		return nil, err
	}

	availablePairs := make(map[string]struct{}, len(pairsSummary))
	for _, pair := range pairsSummary {
		cp := types.CurrencyPair{
			Base:  pair.Base,
			Quote: pair.Quote,
		}
		availablePairs[strings.ToUpper(cp.String())] = struct{}{}
	}

	return availablePairs, nil
}

func (p *CoinbaseProvider) getTickerPrice(cp types.CurrencyPair) (types.TickerPrice, error) {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	gp := currencyPairToCoinbasePair(cp)
	if tickerPair, ok := p.tickers[gp]; ok {
		return tickerPair.toTickerPrice()
	}

	return types.TickerPrice{}, fmt.Errorf(
		types.ErrTickerNotFound.Error(),
		p.endpoints.Name,
		gp,
	)
}

func (p *CoinbaseProvider) getTradePrices(key string) ([]CoinbaseTrade, error) {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	trades, ok := p.trades[key]
	if !ok {
		return []CoinbaseTrade{}, fmt.Errorf("failed to get trades for %s", key)
	}

	return trades, nil
}

func (p *CoinbaseProvider) messageReceived(_ int, _ *WebsocketConnection, bz []byte) {
	var coinbaseTrade CoinbaseTradeResponse
	if err := json.Unmarshal(bz, &coinbaseTrade); err != nil {
		p.logger.Error().Err(err).Msg("unable to unmarshal response")
		return
	}

	if coinbaseTrade.Type == "error" {
		var coinbaseErr CoinbaseErrResponse
		if err := json.Unmarshal(bz, &coinbaseErr); err != nil {
			p.logger.Debug().Err(err).Msg("unable to unmarshal error response")
		}
		p.logger.Error().Msg(coinbaseErr.Reason)
		return
	}

	if coinbaseTrade.Type == "subscriptions" { // successful subscription message
		return
	}

	if coinbaseTrade.Type == "ticker" {
		var coinbaseTicker CoinbaseTicker
		if err := json.Unmarshal(bz, &coinbaseTicker); err != nil {
			p.logger.Error().Err(err).Msg("unable to unmarshal response")
			return
		}

		p.setTickerPair(coinbaseTicker)
		telemetryWebsocketMessage(ProviderCoinbase, MessageTypeTicker)
		return
	}

	telemetryWebsocketMessage(ProviderCoinbase, MessageTypeTrade)
	p.setTradePair(coinbaseTrade)
}

// timeToUnix converts a Time in format "2006-01-02T15:04:05.000000Z" to unix
func (tr CoinbaseTradeResponse) timeToUnix() int64 {
	t, err := time.Parse(coinbaseTimeFmt, tr.Time)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func (tr CoinbaseTradeResponse) toTrade() CoinbaseTrade {
	return CoinbaseTrade{
		Time:      tr.timeToUnix(),
		Price:     tr.Price,
		ProductID: tr.ProductID,
		Size:      tr.Size,
	}
}

func (p *CoinbaseProvider) setTickerPair(ticker CoinbaseTicker) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.tickers[ticker.ProductID] = ticker
}

// setTradePair takes a CoinbaseTradeResponse, converts its date into unix epoch,
// and then will add it to a copy of the trade slice. Then it filters out any
// "stale" trades, and sets the trade slice in memory to the copy.
func (p *CoinbaseProvider) setTradePair(tradeResponse CoinbaseTradeResponse) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	staleTime := PastUnixTime(providerCandlePeriod)
	tradeList := []CoinbaseTrade{
		tradeResponse.toTrade(),
	}

	for _, t := range p.trades[tradeResponse.ProductID] {
		if staleTime < t.Time {
			tradeList = append(tradeList, t)
		}
	}
	p.trades[tradeResponse.ProductID] = tradeList
}

// setSubscribedPairs sets N currency pairs to the map of subscribed pairs.
func (p *CoinbaseProvider) setSubscribedPairs(cps ...types.CurrencyPair) {
	for _, cp := range cps {
		p.subscribedPairs[cp.String()] = cp
	}
}

func (ticker CoinbaseTicker) toTickerPrice() (types.TickerPrice, error) {
	return types.NewTickerPrice(
		string(ProviderCoinbase),
		coinbasePairToCurrencyPair(ticker.ProductID),
		ticker.Price,
		ticker.Volume,
	)
}

// currencyPairToCoinbasePair returns the expected pair for Coinbase
// ex.: "ATOM-USDT".
func currencyPairToCoinbasePair(pair types.CurrencyPair) string {
	return pair.Base + "-" + pair.Quote
}

// coinbasePairToCurrencyPair returns the currency pair string
// ex.: "ATOMUSDT".
func coinbasePairToCurrencyPair(coinbasePair string) string {
	return strings.ReplaceAll(coinbasePair, "-", "")
}

// newCoinbaseSubscription returns a new subscription topic for matches/tickers.
func newCoinbaseSubscription(cp ...string) CoinbaseSubscriptionMsg {
	return CoinbaseSubscriptionMsg{
		Type:       "subscribe",
		ProductIDs: cp,
		Channels:   []string{"matches", "ticker"},
	}
}
