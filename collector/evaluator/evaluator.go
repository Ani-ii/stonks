// Package evaluator scores a signal against daily price bars (M4, v1).
//
// v1 handles filings published after the close, before the open, or on a
// non-trading day: entry is the first open the market offers after the filing.
// Filings published during market hours return StatusUnsupported; that case
// needs intraday bars and is added later as one more branch in Evaluate.
// The index bars are the trading calendar, so a stock that simply did not trade on a day is
// not mistaken for a holiday. Pure logic: no database, no network. Feed it bars from any source.
package evaluator

import (
	"sort"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+30*60)

const (
	openMin  = 9*60 + 15  // 09:15 IST
	closeMin = 15*60 + 30 // 15:30 IST
)

const (
	StatusOK          = "OK"
	StatusUnsupported = "UNSUPPORTED_BUCKET"
	StatusNoPrices    = "NO_PRICES"      // price data missing or not loaded yet; retried later
	StatusNotTraded   = "STOCK_NOT_TRADED" // market was open on the entry day but the stock had no trades
	StatusIlliquid    = "ILLIQUID"         // entry day too thin to trust; metrics are filled but not scored
)

const (
	BucketAfterClose  = "after_close"
	BucketPreOpen     = "pre_open"
	BucketMarketHours = "market_hours"
	BucketNonTrading  = "non_trading_day"
)

// Bar is one trading day. Date is YYYY-MM-DD (exchange date).
type Bar struct {
	Date                   string
	Open, High, Low, Close float64
	Volume                 int64
}

type Signal struct {
	Symbol      string
	Direction   string // UP, DOWN, NEUTRAL
	PublishedAt time.Time
}

type Config struct {
	TargetPct   float64 // take profit, % from entry open (e.g. 2.5)
	StopPct     float64 // stop loss, % from entry open (e.g. 1.5)
	CostPct     float64 // round-trip cost in % (brokerage, taxes, slippage)
	NeutralBand float64 // |excess move| below this counts as NEUTRAL (e.g. 1.0)
	VolDays     int     // days used for average volume (e.g. 20)

	MinRelVolume float64 // entry-day volume below this multiple of the average marks the signal ILLIQUID (e.g. 0.2)
}

func DefaultConfig() Config {
	return Config{TargetPct: 2.5, StopPct: 1.5, CostPct: 0.3, NeutralBand: 1.0, VolDays: 20, MinRelVolume: 0.2}
}

type Result struct {
	Status    string
	Bucket    string
	EntryDate string

	// Stock moves in %, entry day versus previous close.
	GapPct         float64 // previous close -> entry open (not capturable)
	OpenToClosePct float64 // entry open -> entry close (capturable)
	TotalPct       float64 // previous close -> entry close

	// Same moves minus the index move.
	ExcessGapPct         float64
	ExcessOpenToClosePct float64
	ExcessTotalPct       float64

	RelVolume float64 // entry-day volume / average of prior VolDays days

	Label string // actual direction from ExcessTotalPct and NeutralBand
	Hit   bool   // signal direction == Label

	// Trade simulation: enter at entry open, exit by target/stop/close.
	ExitReason string  // target, stop, ambiguous, close, no_trade
	TradePct   float64 // result after costs; 0 for no_trade
}

// Evaluate scores one signal. stock and index are daily bars in any order.
func Evaluate(sig Signal, stock, index []Bar, cfg Config) Result {
	stock, index = sorted(stock), sorted(index)
	if len(stock) == 0 || len(index) == 0 { // nothing to measure against: do not guess a bucket
		return Result{Status: StatusNoPrices}
	}
	d := sig.PublishedAt.In(ist).Format("2006-01-02")
	m := minutesIST(sig.PublishedAt)
	di := indexOfDate(index, d) // trading day = the index has a bar for it

	r := Result{Status: StatusOK}
	var ei int // position in index of the entry day
	switch {
	case di >= 0 && m < openMin:
		r.Bucket, ei = BucketPreOpen, di
	case di >= 0 && m >= closeMin:
		r.Bucket, ei = BucketAfterClose, di+1
	case di >= 0:
		r.Bucket, r.Status = BucketMarketHours, StatusUnsupported
		return r
	default:
		r.Bucket = BucketNonTrading
		ei = firstAfter(index, d)
	}
	if ei < 0 || ei >= len(index) { // entry day not published yet
		r.Status = StatusNoPrices
		return r
	}
	entryDate := index[ei].Date
	si := indexOfDate(stock, entryDate)
	if si < 0 { // the market traded that day, this stock did not
		r.Status = StatusNotTraded
		return r
	}
	if si == 0 { // no earlier close to measure the gap from
		r.Status = StatusNoPrices
		return r
	}

	// Previous close is the stock's last traded close, which can be several days back for thin stocks.
	s, p := stock[si], stock[si-1]
	ip := indexOfDate(index, p.Date)
	if ip < 0 || p.Close <= 0 || s.Open <= 0 {
		r.Status = StatusNoPrices
		return r
	}
	x, y := index[ei], index[ip]

	r.EntryDate = s.Date
	r.GapPct = pct(s.Open, p.Close)
	r.OpenToClosePct = pct(s.Close, s.Open)
	r.TotalPct = pct(s.Close, p.Close)
	r.ExcessGapPct = r.GapPct - pct(x.Open, y.Close)
	r.ExcessOpenToClosePct = r.OpenToClosePct - pct(x.Close, x.Open)
	r.ExcessTotalPct = r.TotalPct - pct(x.Close, y.Close)
	r.RelVolume = relVolume(stock, si, cfg.VolDays)
	r.Label = label(r.ExcessTotalPct, cfg.NeutralBand)
	r.Hit = sig.Direction == r.Label
	r.ExitReason, r.TradePct = trade(sig.Direction, s, cfg)
	if s.High <= s.Low || (cfg.MinRelVolume > 0 && r.RelVolume > 0 && r.RelVolume < cfg.MinRelVolume) {
		r.Status = StatusIlliquid
	}
	return r
}

// trade simulates entry at the open. If the day's range touches both target
// and stop, daily bars cannot tell which came first: count it as a stop.
func trade(dir string, b Bar, cfg Config) (string, float64) {
	if dir != "UP" && dir != "DOWN" {
		return "no_trade", 0
	}
	var fav, adv, closeMove float64 // % in the trade's direction
	if dir == "UP" {
		fav, adv, closeMove = pct(b.High, b.Open), pct(b.Low, b.Open), pct(b.Close, b.Open)
	} else {
		fav, adv, closeMove = -pct(b.Low, b.Open), -pct(b.High, b.Open), -pct(b.Close, b.Open)
	}
	hitT, hitS := fav >= cfg.TargetPct, adv <= -cfg.StopPct
	switch {
	case hitT && hitS:
		return "ambiguous", -cfg.StopPct - cfg.CostPct
	case hitT:
		return "target", cfg.TargetPct - cfg.CostPct
	case hitS:
		return "stop", -cfg.StopPct - cfg.CostPct
	}
	return "close", closeMove - cfg.CostPct
}

func label(excess, band float64) string {
	switch {
	case excess > band:
		return "UP"
	case excess < -band:
		return "DOWN"
	}
	return "NEUTRAL"
}

func relVolume(bars []Bar, entry, n int) float64 {
	if n <= 0 {
		n = 20
	}
	start := entry - n
	if start < 0 {
		start = 0
	}
	var sum float64
	cnt := 0
	for i := start; i < entry; i++ {
		sum += float64(bars[i].Volume)
		cnt++
	}
	if cnt == 0 || sum == 0 {
		return 0
	}
	return float64(bars[entry].Volume) / (sum / float64(cnt))
}

func pct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return (a/b - 1) * 100
}

func minutesIST(t time.Time) int {
	t = t.In(ist)
	return t.Hour()*60 + t.Minute()
}

func sorted(b []Bar) []Bar {
	c := append([]Bar(nil), b...)
	sort.Slice(c, func(i, j int) bool { return c[i].Date < c[j].Date })
	return c
}

func indexOfDate(bars []Bar, date string) int {
	for i, b := range bars {
		if b.Date == date {
			return i
		}
	}
	return -1
}

func firstAfter(bars []Bar, date string) int {
	for i, b := range bars {
		if b.Date > date {
			return i
		}
	}
	return -1
}
