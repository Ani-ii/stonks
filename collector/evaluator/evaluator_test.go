package evaluator

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, ist)
}

func flatIndex(dates ...string) []Bar {
	var out []Bar
	for _, d := range dates {
		out = append(out, Bar{Date: d, Open: 1000, High: 1000, Low: 1000, Close: 1000, Volume: 1})
	}
	return out
}

func stockPair(prevClose float64, o, h, l, c float64, vol int64) []Bar {
	return []Bar{
		{Date: "2026-10-27", Open: prevClose, High: prevClose, Low: prevClose, Close: prevClose, Volume: 1000},
		{Date: "2026-10-28", Open: o, High: h, Low: l, Close: c, Volume: vol},
	}
}

var afterClose = Signal{Symbol: "X", Direction: "UP", PublishedAt: at(2026, 10, 27, 19, 13)}

func TestAfterCloseUpTarget(t *testing.T) {
	stock := stockPair(100, 110, 113, 109.5, 112, 3000)
	idx := []Bar{
		{Date: "2026-10-27", Open: 1000, High: 1000, Low: 1000, Close: 1000},
		{Date: "2026-10-28", Open: 1000, High: 1005, Low: 998, Close: 1001},
	}
	r := Evaluate(afterClose, stock, idx, DefaultConfig())
	if r.Status != StatusOK || r.Bucket != BucketAfterClose || r.EntryDate != "2026-10-28" {
		t.Fatalf("bad status/bucket/date: %+v", r)
	}
	if !near(r.GapPct, 10) || !near(r.TotalPct, 12) || !near(r.ExcessTotalPct, 11.9) {
		t.Fatalf("bad moves: %+v", r)
	}
	if r.Label != "UP" || !r.Hit || r.ExitReason != "target" || !near(r.TradePct, 2.2) {
		t.Fatalf("bad label/trade: %+v", r)
	}
	if !near(r.RelVolume, 3) {
		t.Fatalf("rel volume = %v, want 3", r.RelVolume)
	}
}

func TestDownShortTarget(t *testing.T) {
	sig := afterClose
	sig.Direction = "DOWN"
	r := Evaluate(sig, stockPair(100, 100, 100.5, 97, 97.5, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Label != "DOWN" || !r.Hit || r.ExitReason != "target" || !near(r.TradePct, 2.2) {
		t.Fatalf("%+v", r)
	}
}

func TestAmbiguousCountsAsStop(t *testing.T) {
	r := Evaluate(afterClose, stockPair(100, 100, 103, 98, 100, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.ExitReason != "ambiguous" || !near(r.TradePct, -1.8) {
		t.Fatalf("%+v", r)
	}
}

func TestSmallMoveIsNeutralAndExitsAtClose(t *testing.T) {
	r := Evaluate(afterClose, stockPair(100, 100.2, 100.5, 99.8, 100.4, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Label != "NEUTRAL" || r.Hit || r.ExitReason != "close" {
		t.Fatalf("%+v", r)
	}
}

func TestNeutralSignalMakesNoTrade(t *testing.T) {
	sig := afterClose
	sig.Direction = "NEUTRAL"
	r := Evaluate(sig, stockPair(100, 100.2, 100.5, 99.8, 100.4, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.ExitReason != "no_trade" || r.TradePct != 0 || !r.Hit {
		t.Fatalf("%+v", r)
	}
}

func TestMarketHoursUnsupported(t *testing.T) {
	sig := afterClose
	sig.PublishedAt = at(2026, 10, 27, 11, 0)
	r := Evaluate(sig, stockPair(100, 110, 113, 109, 112, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusUnsupported || r.Bucket != BucketMarketHours {
		t.Fatalf("%+v", r)
	}
}

func TestPreOpenEntersSameDay(t *testing.T) {
	sig := afterClose
	sig.PublishedAt = at(2026, 10, 28, 8, 0)
	r := Evaluate(sig, stockPair(100, 105, 106, 104, 105.5, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Bucket != BucketPreOpen || r.EntryDate != "2026-10-28" || !near(r.GapPct, 5) {
		t.Fatalf("%+v", r)
	}
}

func TestSaturdayUsesNextTradingDay(t *testing.T) {
	stock := []Bar{
		{Date: "2026-10-30", Open: 99, High: 101, Low: 98, Close: 100, Volume: 1000},
		{Date: "2026-11-02", Open: 105, High: 106, Low: 104, Close: 105.5, Volume: 1000},
	}
	sig := Signal{Symbol: "X", Direction: "UP", PublishedAt: at(2026, 10, 31, 20, 0)}
	r := Evaluate(sig, stock, flatIndex("2026-10-30", "2026-11-02"), DefaultConfig())
	if r.Bucket != BucketNonTrading || r.EntryDate != "2026-11-02" || !near(r.GapPct, 5) {
		t.Fatalf("%+v", r)
	}
}

func TestNoNextBarMeansNoPrices(t *testing.T) {
	stock := stockPair(100, 110, 113, 109, 112, 1000)[:1] // only the filing day
	r := Evaluate(afterClose, stock, flatIndex("2026-10-27"), DefaultConfig())
	if r.Status != StatusNoPrices {
		t.Fatalf("%+v", r)
	}
}

func TestMissingIndexMeansNoPrices(t *testing.T) {
	r := Evaluate(afterClose, stockPair(100, 110, 113, 109, 112, 1000), nil, DefaultConfig())
	if r.Status != StatusNoPrices {
		t.Fatalf("%+v", r)
	}
}

func TestNoStockBarsMeansNoPricesAndNoBucket(t *testing.T) {
	r := Evaluate(afterClose, nil, flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusNoPrices || r.Bucket != "" {
		t.Fatalf("%+v", r)
	}
}

func TestStockThatDidNotTradeOnEntryDay(t *testing.T) {
	stock := []Bar{{Date: "2026-10-27", Open: 100, High: 100, Low: 100, Close: 100, Volume: 1000}} // no 10-28 bar
	r := Evaluate(afterClose, stock, flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusNotTraded || r.Bucket != BucketAfterClose {
		t.Fatalf("%+v", r)
	}
}

func TestThinStockUsesLastTradedClose(t *testing.T) {
	stock := []Bar{ // traded on 10-23, then not on the filing day 10-27, then on 10-28
		{Date: "2026-10-23", Open: 99, High: 101, Low: 98, Close: 100, Volume: 1000},
		{Date: "2026-10-28", Open: 110, High: 111, Low: 109, Close: 110.5, Volume: 1000},
	}
	r := Evaluate(afterClose, stock, flatIndex("2026-10-23", "2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusOK || r.Bucket != BucketAfterClose || r.EntryDate != "2026-10-28" || !near(r.GapPct, 10) {
		t.Fatalf("%+v", r)
	}
}

func TestIlliquidEntryDayIsFlaggedButMeasured(t *testing.T) {
	r := Evaluate(afterClose, stockPair(100, 110, 113, 109.5, 112, 100), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusIlliquid || !near(r.GapPct, 10) || !near(r.RelVolume, 0.1) {
		t.Fatalf("%+v", r)
	}
	r = Evaluate(afterClose, stockPair(100, 100, 100, 100, 100, 1000), flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusIlliquid { // no price range at all: nothing really traded
		t.Fatalf("flat bar should be ILLIQUID: %+v", r)
	}
}

func TestNoEarlierBarMeansNoPrices(t *testing.T) {
	stock := []Bar{{Date: "2026-10-28", Open: 110, High: 113, Low: 109, Close: 112, Volume: 1000}}
	r := Evaluate(afterClose, stock, flatIndex("2026-10-27", "2026-10-28"), DefaultConfig())
	if r.Status != StatusNoPrices {
		t.Fatalf("%+v", r)
	}
}
