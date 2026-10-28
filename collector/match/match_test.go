package match

import (
	"testing"
	"time"

	"stonks/announce"
)

func TestNormalizeCompany(t *testing.T) {
	same := [][2]string{
		{"Schaeffler India Ltd", "Schaeffler India Limited"},
		{"Shah Investors Home Ltd", "Shah Investor's Home Limited"},
		{"Larsen & Toubro Limited", "Larsen and Toubro Ltd"},
		{"Allied Blenders and Distillers Ltd", "ALLIED BLENDERS AND DISTILLERS LIMITED"},
		{"Morepen Laboratories Ltd", "Morepen Laboratories Limited"},
	}
	for _, p := range same {
		if NormalizeCompany(p[0]) != NormalizeCompany(p[1]) {
			t.Errorf("%q and %q should normalise equally: %q vs %q", p[0], p[1], NormalizeCompany(p[0]), NormalizeCompany(p[1]))
		}
	}
	if NormalizeCompany("Tata Motors Ltd") == NormalizeCompany("Tata Motors Passenger Vehicles Ltd") {
		t.Error("different companies must stay different")
	}
}

func TestSameEvent(t *testing.T) {
	at := time.Date(2026, 10, 28, 19, 40, 0, 0, announce.IST)
	bse := announce.Announcement{Source: "BSE", Company: "Schaeffler India Ltd", DisseminatedAt: at}
	nse := announce.Announcement{Source: "NSE", Company: "Schaeffler India Limited", DisseminatedAt: at.Add(3 * time.Minute)}
	if !SameEvent(bse, nse, 15*time.Minute) || !SameEvent(nse, bse, 15*time.Minute) {
		t.Error("same company on both exchanges within 3 minutes should match (both directions)")
	}
	if SameEvent(bse, announce.Announcement{Source: "NSE", Company: "Schaeffler India Limited", DisseminatedAt: at.Add(2 * time.Hour)}, 15*time.Minute) {
		t.Error("two hours apart must not match")
	}
	if SameEvent(bse, announce.Announcement{Source: "BSE", Company: "Schaeffler India Ltd", DisseminatedAt: at}, 15*time.Minute) {
		t.Error("two filings from the same exchange are never merged")
	}
	if SameEvent(bse, announce.Announcement{Source: "NSE", Company: "Other Ltd", DisseminatedAt: at}, 15*time.Minute) {
		t.Error("different company must not match")
	}
}
