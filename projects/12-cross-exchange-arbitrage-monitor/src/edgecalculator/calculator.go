// Package edgecalculator performs volume-aware, fixed-point cross-exchange
// arbitrage edge estimation with freshness, skew, fee, FX, slippage, and inventory constraints.
package edgecalculator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"arbmonitor/src/domain"
	"arbmonitor/src/fixed"
)

// FeeSchedule stores taker fee policies per (venue, instrument) with wildcard fallback.
type FeeSchedule struct {
	mu   sync.RWMutex
	fees map[string]domain.VenueFee // lower(venue)|upper(instrument)
}

// NewFeeSchedule constructs a FeeSchedule from a slice of VenueFee records.
func NewFeeSchedule(fees []domain.VenueFee) *FeeSchedule {
	fs := &FeeSchedule{fees: make(map[string]domain.VenueFee, len(fees))}
	for _, f := range fees {
		fs.Set(f)
	}
	return fs
}

// LoadFeeScheduleFile loads taker fees from a JSON file.
func LoadFeeScheduleFile(path string) (*FeeSchedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fees file: %w", err)
	}
	var fees []domain.VenueFee
	if err := json.Unmarshal(data, &fees); err != nil {
		return nil, fmt.Errorf("parse fees json: %w", err)
	}
	return NewFeeSchedule(fees), nil
}

// Set upserts a VenueFee entry.
func (fs *FeeSchedule) Set(f domain.VenueFee) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f.Venue = strings.ToLower(strings.TrimSpace(f.Venue))
	f.Instrument = strings.ToUpper(strings.TrimSpace(f.Instrument))
	if f.Instrument == "" {
		f.Instrument = "*"
	}
	fs.fees[feeKey(f.Venue, f.Instrument)] = f
}

// Lookup returns the fee policy for (venue, instrument), falling back to (venue, "*").
func (fs *FeeSchedule) Lookup(venue, instrument string) (domain.VenueFee, bool) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	v := strings.ToLower(strings.TrimSpace(venue))
	inst := strings.ToUpper(strings.TrimSpace(instrument))
	if f, ok := fs.fees[feeKey(v, inst)]; ok {
		return f, true
	}
	if f, ok := fs.fees[feeKey(v, "*")]; ok {
		return f, true
	}
	return domain.VenueFee{}, false
}

func feeKey(venue, instrument string) string {
	return venue + "|" + instrument
}

// BalanceBook tracks pre-funded per-venue asset balances.
// Balances are never assumed to teleport between venues.
type BalanceBook struct {
	mu       sync.RWMutex
	balances map[string]domain.VenueBalance // lower(venue)|upper(asset)
}

// NewBalanceBook creates a BalanceBook from initial balances.
func NewBalanceBook(items []domain.VenueBalance) *BalanceBook {
	bb := &BalanceBook{balances: make(map[string]domain.VenueBalance, len(items))}
	for _, b := range items {
		bb.Set(b)
	}
	return bb
}

// LoadBalanceBookFile loads pre-funded balances from a JSON file.
func LoadBalanceBookFile(path string) (*BalanceBook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read balances file: %w", err)
	}
	var items []domain.VenueBalance
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse balances json: %w", err)
	}
	return NewBalanceBook(items), nil
}

// Set updates the available balance for (venue, asset).
func (bb *BalanceBook) Set(b domain.VenueBalance) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	b.Venue = strings.ToLower(strings.TrimSpace(b.Venue))
	b.Asset = strings.ToUpper(strings.TrimSpace(b.Asset))
	bb.balances[b.Venue+"|"+b.Asset] = b
}

// Available returns the available balance for (venue, asset), defaulting to zero.
func (bb *BalanceBook) Available(venue, asset string) fixed.Decimal {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	key := strings.ToLower(strings.TrimSpace(venue)) + "|" + strings.ToUpper(strings.TrimSpace(asset))
	if b, ok := bb.balances[key]; ok {
		return b.Available
	}
	return fixed.Zero
}

// All returns all balances sorted deterministically by (venue, asset).
func (bb *BalanceBook) All() []domain.VenueBalance {
	bb.mu.RLock()
	defer bb.mu.RUnlock()
	out := make([]domain.VenueBalance, 0, len(bb.balances))
	for _, b := range bb.balances {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Venue != out[j].Venue {
			return out[i].Venue < out[j].Venue
		}
		return out[i].Asset < out[j].Asset
	})
	return out
}

// TransferSchedule holds illustrative withdrawal/network cost estimates, kept strictly
// separate from instantaneous pre-funded arbitrage edge calculations.
type TransferSchedule struct {
	fees       map[string]fixed.Decimal // base asset -> withdrawal fee in base units
	confirmSec map[string]int64         // base asset -> typical confirmation seconds
}

// DefaultTransferSchedule returns conservative illustrative on-chain withdrawal estimates.
func DefaultTransferSchedule() *TransferSchedule {
	return &TransferSchedule{
		fees: map[string]fixed.Decimal{
			"BTC": fixed.MustParse("0.0002"),
			"ETH": fixed.MustParse("0.003"),
			"SOL": fixed.MustParse("0.01"),
		},
		confirmSec: map[string]int64{
			"BTC": 1800, // ~3 blocks
			"ETH": 180,  // ~15 blocks
			"SOL": 15,
		},
	}
}

// Estimate returns an off-band TransferCostEstimate for rebalancing baseAsset.
func (ts *TransferSchedule) Estimate(baseAsset string, refPrice fixed.Decimal) *domain.TransferCostEstimate {
	if ts == nil {
		return nil
	}
	asset := strings.ToUpper(strings.TrimSpace(baseAsset))
	fee, ok := ts.fees[asset]
	if !ok {
		return nil
	}
	return &domain.TransferCostEstimate{
		Asset:          asset,
		WithdrawalFee:  fee,
		EstimatedQuote: fee.Mul(refPrice),
		EstConfirmSec:  ts.confirmSec[asset],
		IncludedInEdge: false,
		Note:           "Separated from instantaneous pre-funded edge; rebalancing carries transfer fee and settlement latency risk.",
	}
}

// FXLookup resolves FX conversion rates between quote currencies.
type FXLookup interface {
	GetFXRate(from, to string) (domain.FXRate, bool)
}

// EvalInput bundles all inputs required for a deterministic pair evaluation.
type EvalInput struct {
	BuyBook  domain.VenueBook
	SellBook domain.VenueBook
	BuySpec  domain.InstrumentSpec
	SellSpec domain.InstrumentSpec
	Config   domain.AlertConfig
	Now      time.Time
}

// EvalResult captures either a valid Opportunity or a structured SuppressionReason.
type EvalResult struct {
	Opportunity *domain.Opportunity
	BuyFee      domain.VenueFee
	SellFee     domain.VenueFee
	FXRateUsed  *domain.FXRate
	Suppressed  domain.SuppressionReason
	Detail      string
}

// Calculator performs exact fixed-point cross-book net-edge calculations.
type Calculator struct {
	fees        *FeeSchedule
	balances    *BalanceBook
	fx          FXLookup
	transfers   *TransferSchedule
	ruleVersion string
}

// New creates a Calculator with the given fee schedule, balance book, and FX source.
func New(fees *FeeSchedule, balances *BalanceBook, fx FXLookup, transfers *TransferSchedule) *Calculator {
	if transfers == nil {
		transfers = DefaultTransferSchedule()
	}
	return &Calculator{
		fees:        fees,
		balances:    balances,
		fx:          fx,
		transfers:   transfers,
		ruleVersion: domain.DefaultRuleVersion,
	}
}

// RuleVersion returns the version identifier of the calculation policy.
func (c *Calculator) RuleVersion() string {
	return c.ruleVersion
}

// Evaluate checks freshness, health, skew, FX, depth, fees, slippage, and inventory
// to calculate the achievable buy-on-BuyBook / sell-on-SellBook arbitrage opportunity.
func (c *Calculator) Evaluate(in EvalInput) EvalResult {
	now := in.Now.UTC()
	cfg := in.Config
	buy := in.BuyBook
	sell := in.SellBook

	if buy.Venue == sell.Venue {
		return EvalResult{Suppressed: domain.SuppressNoCross, Detail: "same venue"}
	}

	// 1. Connectivity / status gate.
	if buy.Status != domain.VenueStatusHealthy || sell.Status != domain.VenueStatusHealthy {
		return EvalResult{
			Suppressed: domain.SuppressDisconnected,
			Detail:     fmt.Sprintf("buy_status=%s sell_status=%s", buy.Status, sell.Status),
		}
	}

	// 2. Missing side gate.
	if len(buy.Asks) == 0 || len(sell.Bids) == 0 {
		return EvalResult{
			Suppressed: domain.SuppressMissingSide,
			Detail:     fmt.Sprintf("buy_asks=%d sell_bids=%d", len(buy.Asks), len(sell.Bids)),
		}
	}

	// 3. Clock skew & quote freshness gates.
	buyReceiveAgeMs := now.Sub(buy.ReceivedAt).Milliseconds()
	sellReceiveAgeMs := now.Sub(sell.ReceivedAt).Milliseconds()
	buyEventAgeMs := now.Sub(buy.Asof).Milliseconds()
	sellEventAgeMs := now.Sub(sell.Asof).Milliseconds()

	buyClockSkewMs := absInt64(buy.ReceivedAt.Sub(buy.Asof).Milliseconds())
	sellClockSkewMs := absInt64(sell.ReceivedAt.Sub(sell.Asof).Milliseconds())

	if buyReceiveAgeMs < 0 || sellReceiveAgeMs < 0 {
		return EvalResult{
			Suppressed: domain.SuppressClockSkew,
			Detail:     fmt.Sprintf("future receive timestamp buy_age=%dms sell_age=%dms", buyReceiveAgeMs, sellReceiveAgeMs),
		}
	}
	if cfg.MaxClockSkewMs > 0 && (buyClockSkewMs > cfg.MaxClockSkewMs || sellClockSkewMs > cfg.MaxClockSkewMs) {
		return EvalResult{
			Suppressed: domain.SuppressClockSkew,
			Detail:     fmt.Sprintf("clock_skew buy=%dms sell=%dms limit=%dms", buyClockSkewMs, sellClockSkewMs, cfg.MaxClockSkewMs),
		}
	}
	if cfg.MaxQuoteAgeMs > 0 {
		if buyReceiveAgeMs > cfg.MaxQuoteAgeMs || sellReceiveAgeMs > cfg.MaxQuoteAgeMs ||
			buyEventAgeMs > cfg.MaxQuoteAgeMs || sellEventAgeMs > cfg.MaxQuoteAgeMs {
			return EvalResult{
				Suppressed: domain.SuppressStaleQuote,
				Detail: fmt.Sprintf("buy_rx_age=%dms sell_rx_age=%dms buy_ev_age=%dms sell_ev_age=%dms limit=%dms",
					buyReceiveAgeMs, sellReceiveAgeMs, buyEventAgeMs, sellEventAgeMs, cfg.MaxQuoteAgeMs),
			}
		}
	}

	// 4. Inter-leg synchronization gate (never treat desynchronized quotes as simultaneously executable).
	legSkewMs := absInt64(buy.Asof.Sub(sell.Asof).Milliseconds())
	if cfg.MaxLegSkewMs > 0 && legSkewMs > cfg.MaxLegSkewMs {
		return EvalResult{
			Suppressed: domain.SuppressLegSkew,
			Detail:     fmt.Sprintf("leg_skew=%dms limit=%dms", legSkewMs, cfg.MaxLegSkewMs),
		}
	}

	// 5. Currency conversion resolution.
	commonQuote := in.BuySpec.Quote
	fxRate := fixed.One
	fxSpreadBps := fixed.Zero
	var fxUsed *domain.FXRate
	if in.SellSpec.Quote != commonQuote {
		if c.fx == nil {
			return EvalResult{Suppressed: domain.SuppressMissingFXRate, Detail: "no fx provider configured"}
		}
		rate, ok := c.fx.GetFXRate(in.SellSpec.Quote, commonQuote)
		if !ok || !rate.Rate.IsPositive() {
			return EvalResult{
				Suppressed: domain.SuppressMissingFXRate,
				Detail:     fmt.Sprintf("missing FX rate %s->%s", in.SellSpec.Quote, commonQuote),
			}
		}
		fxMaxAgeMs := cfg.MaxQuoteAgeMs * 10
		if fxMaxAgeMs < 5000 {
			fxMaxAgeMs = 5000
		}
		if !rate.ReceivedAt.IsZero() && now.Sub(rate.ReceivedAt).Milliseconds() > fxMaxAgeMs {
			return EvalResult{
				Suppressed: domain.SuppressMissingFXRate,
				Detail:     fmt.Sprintf("stale FX rate %s->%s age=%dms", in.SellSpec.Quote, commonQuote, now.Sub(rate.ReceivedAt).Milliseconds()),
			}
		}
		fxRate = rate.Rate
		fxSpreadBps = rate.SpreadBps
		rateCopy := rate
		fxUsed = &rateCopy
	}

	// 6. Fee schedule lookup.
	buyFeeSpec, ok := c.fees.Lookup(buy.Venue, buy.Instrument)
	if !ok {
		buyFeeSpec = domain.VenueFee{Venue: buy.Venue, Instrument: buy.Instrument, TakerBps: fixed.FromInt(10), Tier: "default", Version: "v1"}
	}
	sellFeeSpec, ok := c.fees.Lookup(sell.Venue, sell.Instrument)
	if !ok {
		sellFeeSpec = domain.VenueFee{Venue: sell.Venue, Instrument: sell.Instrument, TakerBps: fixed.FromInt(10), Tier: "default", Version: "v1"}
	}

	// 7. Quick top-of-book cross check.
	topBuyAsk := buy.Asks[0].Price
	topSellBid := sell.Bids[0].Price.Mul(fxRate)
	if topSellBid.LessThanOrEqual(topBuyAsk) {
		return EvalResult{
			BuyFee:     buyFeeSpec,
			SellFee:    sellFeeSpec,
			FXRateUsed: fxUsed,
			Suppressed: domain.SuppressNoCross,
			Detail:     fmt.Sprintf("top_ask=%s >= top_bid=%s", topBuyAsk, topSellBid),
		}
	}

	// 8. Inventory limits (pre-funded per venue; no balance teleportation).
	lotStep := fixed.Max(in.BuySpec.LotSize, in.SellSpec.LotSize)
	maxBaseQty := fixed.FromRaw(1<<62 - 1) // unconstrained sentinel
	maxBuyQuote := fixed.FromRaw(1<<62 - 1)
	if cfg.EnforceInventory && c.balances != nil {
		maxBaseQty = c.balances.Available(sell.Venue, in.SellSpec.Base).FloorToStep(lotStep)
		maxBuyQuote = c.balances.Available(buy.Venue, in.BuySpec.Quote)
		if !maxBaseQty.IsPositive() || !maxBuyQuote.IsPositive() {
			return EvalResult{
				BuyFee:     buyFeeSpec,
				SellFee:    sellFeeSpec,
				FXRateUsed: fxUsed,
				Suppressed: domain.SuppressInsufficientInventory,
				Detail:     fmt.Sprintf("sell_base_avail=%s buy_quote_avail=%s", maxBaseQty, maxBuyQuote),
			}
		}
	}

	// 9. Volume-aware depth walking across buy asks (ascending) and sell bids (descending).
	maxDepth := cfg.MaxDepthLevels
	if maxDepth <= 0 {
		maxDepth = 10
	}
	buyLimit := len(buy.Asks)
	if buyLimit > maxDepth {
		buyLimit = maxDepth
	}
	sellLimit := len(sell.Bids)
	if sellLimit > maxDepth {
		sellLimit = maxDepth
	}

	bi, si := 0, 0
	remBuy := buy.Asks[0].Qty
	remSell := sell.Bids[0].Qty

	var slices []domain.DepthSlice
	totalQty := fixed.Zero
	totalBuyNotional := fixed.Zero
	totalSellNotional := fixed.Zero
	totalSellNativeNotional := fixed.Zero
	inventoryCapped := false
	maxBuyIdx := -1
	maxSellIdx := -1

	for bi < buyLimit && si < sellLimit {
		rawBuyPrice := buy.Asks[bi].Price
		rawSellPrice := sell.Bids[si].Price
		normSellPrice := rawSellPrice.Mul(fxRate)

		// Stop immediately if current price levels no longer cross.
		if normSellPrice.LessThanOrEqual(rawBuyPrice) {
			break
		}

		// Stop walking deeper levels if marginal unit edge after variable fees/FX/slippage <= 0.
		marginalBuyFee := rawBuyPrice.MulBps(buyFeeSpec.TakerBps, true)
		marginalSellFee := normSellPrice.MulBps(sellFeeSpec.TakerBps, true)
		marginalFXCost := normSellPrice.MulBps(fxSpreadBps, true)
		marginalSlip := rawBuyPrice.MulBps(cfg.ModeledSlippageBps, true)
		marginalNet := normSellPrice.Sub(rawBuyPrice).Sub(marginalBuyFee).Sub(marginalSellFee).Sub(marginalFXCost).Sub(marginalSlip)
		if !marginalNet.IsPositive() {
			break
		}

		sliceQty := fixed.Min(remBuy, remSell)

		// Enforce pre-funded base inventory on sell venue.
		if cfg.EnforceInventory && c.balances != nil {
			remBaseAvail := maxBaseQty.Sub(totalQty)
			if sliceQty.GreaterThan(remBaseAvail) {
				sliceQty = remBaseAvail
				inventoryCapped = true
			}
			// Enforce pre-funded quote inventory on buy venue including taker fee buffer.
			remQuoteAvail := maxBuyQuote.Sub(totalBuyNotional).Sub(totalBuyNotional.MulBps(buyFeeSpec.TakerBps, true))
			if !remQuoteAvail.IsPositive() {
				inventoryCapped = true
				break
			}
			unitBuyCostWithFee := rawBuyPrice.Add(rawBuyPrice.MulBps(buyFeeSpec.TakerBps, true))
			maxQtyByQuote, err := remQuoteAvail.Div(unitBuyCostWithFee)
			if err != nil {
				break
			}
			if sliceQty.GreaterThan(maxQtyByQuote) {
				sliceQty = maxQtyByQuote
				inventoryCapped = true
			}
		}

		sliceQty = sliceQty.FloorToStep(lotStep)
		if !sliceQty.IsPositive() {
			if inventoryCapped {
				break
			}
			// Advance whichever level has sub-lot dust left.
			if remBuy.LessThan(lotStep) {
				bi++
				if bi < buyLimit {
					remBuy = buy.Asks[bi].Qty
				}
			}
			if remSell.LessThan(lotStep) {
				si++
				if si < sellLimit {
					remSell = sell.Bids[si].Qty
				}
			}
			continue
		}

		buyNotional := sliceQty.Mul(rawBuyPrice)
		sellNativeNotional := sliceQty.Mul(rawSellPrice)
		sellNotional := sliceQty.Mul(normSellPrice)

		slices = append(slices, domain.DepthSlice{
			Qty:          sliceQty,
			BuyPrice:     rawBuyPrice,
			SellPrice:    normSellPrice,
			RawBuyPrice:  rawBuyPrice,
			RawSellPrice: rawSellPrice,
			BuyLevelIdx:  bi,
			SellLevelIdx: si,
		})

		totalQty = totalQty.Add(sliceQty)
		totalBuyNotional = totalBuyNotional.Add(buyNotional)
		totalSellNotional = totalSellNotional.Add(sellNotional)
		totalSellNativeNotional = totalSellNativeNotional.Add(sellNativeNotional)
		if bi > maxBuyIdx {
			maxBuyIdx = bi
		}
		if si > maxSellIdx {
			maxSellIdx = si
		}

		remBuy = remBuy.Sub(sliceQty)
		remSell = remSell.Sub(sliceQty)

		if inventoryCapped {
			break
		}
		if remBuy.LessThan(lotStep) {
			bi++
			if bi < buyLimit {
				remBuy = buy.Asks[bi].Qty
			}
		}
		if remSell.LessThan(lotStep) {
			si++
			if si < sellLimit {
				remSell = sell.Bids[si].Qty
			}
		}
	}

	if !totalQty.IsPositive() {
		reason := domain.SuppressNoCross
		if inventoryCapped {
			reason = domain.SuppressInsufficientInventory
		}
		return EvalResult{
			BuyFee:     buyFeeSpec,
			SellFee:    sellFeeSpec,
			FXRateUsed: fxUsed,
			Suppressed: reason,
			Detail:     "zero executable profitable quantity",
		}
	}

	// 10. Minimum notional check.
	minNotional := in.BuySpec.MinNotional
	sellMinInCommon := in.SellSpec.MinNotional.Mul(fxRate)
	if sellMinInCommon.GreaterThan(minNotional) {
		minNotional = sellMinInCommon
	}
	if minNotional.IsPositive() && totalBuyNotional.LessThan(minNotional) {
		return EvalResult{
			BuyFee:     buyFeeSpec,
			SellFee:    sellFeeSpec,
			FXRateUsed: fxUsed,
			Suppressed: domain.SuppressBelowMinNotional,
			Detail:     fmt.Sprintf("notional=%s < min_notional=%s", totalBuyNotional, minNotional),
		}
	}

	// 11. Aggregate fees, FX cost, modeled slippage, VWAP, and gross/net edge.
	buyFeeAmt := fixed.Max(totalBuyNotional.MulBps(buyFeeSpec.TakerBps, true), buyFeeSpec.MinFee)
	sellFeeNative := fixed.Max(totalSellNativeNotional.MulBps(sellFeeSpec.TakerBps, true), sellFeeSpec.MinFee)
	sellFeeAmt := sellFeeNative.Mul(fxRate)
	fxCost := totalSellNotional.MulBps(fxSpreadBps, true)
	modeledSlipCost := totalBuyNotional.MulBps(cfg.ModeledSlippageBps, true)

	grossProfit := totalSellNotional.Sub(totalBuyNotional)
	netProfit := grossProfit.Sub(buyFeeAmt).Sub(sellFeeAmt).Sub(fxCost).Sub(modeledSlipCost)

	grossEdgeBps, _ := fixed.RatioToBps(grossProfit, totalBuyNotional)
	netEdgeBps, _ := fixed.RatioToBps(netProfit, totalBuyNotional)
	buyVWAP, _ := totalBuyNotional.Div(totalQty)
	sellVWAP, _ := totalSellNotional.Div(totalQty)

	// Depth-walking slippage vs top-of-book touch.
	depthSlipQuote := buyVWAP.Sub(topBuyAsk).Add(topSellBid.Sub(sellVWAP))
	if depthSlipQuote.IsNegative() {
		depthSlipQuote = fixed.Zero
	}
	depthSlipBps, _ := fixed.RatioToBps(depthSlipQuote, topBuyAsk)

	// Canonical evaluation timestamp = newer of both venue timestamps (UTC).
	asof := buy.Asof
	if sell.Asof.After(asof) {
		asof = sell.Asof
	}

	oppID := deterministicOppID(in.BuySpec.Canonical, buy.Venue, sell.Venue, buy.Seq, sell.Seq, buy.Asof, sell.Asof, totalQty, netEdgeBps)

	opp := &domain.Opportunity{
		ID:                  oppID,
		RuleVersion:         c.ruleVersion,
		Instrument:          in.BuySpec.Canonical,
		CommonQuote:         commonQuote,
		BuyVenue:            buy.Venue,
		SellVenue:           sell.Venue,
		BuyInstrument:       buy.Instrument,
		SellInstrument:      sell.Instrument,
		Qty:                 totalQty,
		BuyVWAP:             buyVWAP,
		SellVWAP:            sellVWAP,
		BuyTopAsk:           topBuyAsk,
		SellTopBid:          topSellBid,
		ExecutableNotional:  totalBuyNotional,
		GrossEdgeBps:        grossEdgeBps,
		NetEdgeBps:          netEdgeBps,
		GrossProfit:         grossProfit,
		NetProfit:           netProfit,
		BuyFee:              buyFeeAmt,
		SellFee:             sellFeeAmt,
		FXCost:              fxCost,
		ModeledSlippageCost: modeledSlipCost,
		DepthSlippageBps:    depthSlipBps,
		BuyLevelsConsumed:   maxBuyIdx + 1,
		SellLevelsConsumed:  maxSellIdx + 1,
		Slices:              slices,
		InventoryCapped:     inventoryCapped,
		BuyBookAsof:         buy.Asof,
		BuyBookReceivedAt:   buy.ReceivedAt,
		BuyBookSeq:          buy.Seq,
		SellBookAsof:        sell.Asof,
		SellBookReceivedAt:  sell.ReceivedAt,
		SellBookSeq:         sell.Seq,
		LegSkewMs:           legSkewMs,
		BuyAgeMs:            buyReceiveAgeMs,
		SellAgeMs:           sellReceiveAgeMs,
		Asof:                asof,
		TransferCost:        c.transfers.Estimate(in.BuySpec.Base, buyVWAP),
		Disclaimer:          domain.PaperDisclaimer,
	}

	if !netProfit.IsPositive() || netEdgeBps.LessThan(cfg.MinNetBps) {
		return EvalResult{
			Opportunity: opp,
			BuyFee:      buyFeeSpec,
			SellFee:     sellFeeSpec,
			FXRateUsed:  fxUsed,
			Suppressed:  domain.SuppressBelowNetThreshold,
			Detail:      fmt.Sprintf("net_edge_bps=%s < min_net_bps=%s", netEdgeBps, cfg.MinNetBps),
		}
	}

	return EvalResult{
		Opportunity: opp,
		BuyFee:      buyFeeSpec,
		SellFee:     sellFeeSpec,
		FXRateUsed:  fxUsed,
		Suppressed:  domain.SuppressNone,
	}
}

func deterministicOppID(instrument, buyVenue, sellVenue string, buySeq, sellSeq uint64, buyAsof, sellAsof time.Time, qty, netEdgeBps fixed.Decimal) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%d|%d|%d|%d",
		instrument, buyVenue, sellVenue,
		buySeq, sellSeq,
		buyAsof.UTC().UnixNano(), sellAsof.UTC().UnixNano(),
		qty.Raw(), netEdgeBps.Raw(),
	)
	return "opp-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
