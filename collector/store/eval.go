package store

import (
	"context"
	"database/sql"
	"time"

	"stonks/evaluator"
	"stonks/prices"
)

// Eval holds the M4 tables (prices, outcomes) and their queries. It works on its
// own *sql.DB, so the collector and runner code is not touched.
type Eval struct{ db *sql.DB }

func NewEval(db *sql.DB) *Eval { return &Eval{db: db} }

const evalSchema = `
CREATE TABLE IF NOT EXISTS prices (
  src    TEXT NOT NULL,            -- NSE | BSE | IDX (index)
  code   TEXT NOT NULL,            -- NSE symbol | BSE scrip code | index name
  date   TEXT NOT NULL,            -- YYYY-MM-DD
  isin   TEXT NOT NULL DEFAULT '',
  open   REAL NOT NULL,
  high   REAL NOT NULL,
  low    REAL NOT NULL,
  close  REAL NOT NULL,
  volume INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (src, code, date)
);
CREATE INDEX IF NOT EXISTS idx_prices_isin ON prices (isin, date);
CREATE TABLE IF NOT EXISTS price_days (
  src        TEXT NOT NULL,
  date       TEXT NOT NULL,
  status     TEXT NOT NULL,        -- OK | NOT_FOUND
  rows       INTEGER NOT NULL DEFAULT 0,
  fetched_ms INTEGER NOT NULL,
  PRIMARY KEY (src, date)
);
CREATE TABLE IF NOT EXISTS outcomes (
  signal_id             INTEGER PRIMARY KEY REFERENCES signals(id),
  evaluated_ms          INTEGER NOT NULL,
  status                TEXT NOT NULL,   -- OK | UNSUPPORTED_BUCKET | NO_PRICES
  bucket                TEXT NOT NULL DEFAULT '',
  entry_date            TEXT NOT NULL DEFAULT '',
  price_src             TEXT NOT NULL DEFAULT '',
  gap_pct               REAL NOT NULL DEFAULT 0,
  open_close_pct        REAL NOT NULL DEFAULT 0,
  total_pct             REAL NOT NULL DEFAULT 0,
  excess_gap_pct        REAL NOT NULL DEFAULT 0,
  excess_open_close_pct REAL NOT NULL DEFAULT 0,
  excess_total_pct      REAL NOT NULL DEFAULT 0,
  rel_volume            REAL NOT NULL DEFAULT 0,
  label                 TEXT NOT NULL DEFAULT '',
  hit                   INTEGER NOT NULL DEFAULT 0,
  exit_reason           TEXT NOT NULL DEFAULT '',
  trade_pct             REAL NOT NULL DEFAULT 0
);`

func (e *Eval) EnsureSchema(ctx context.Context) error {
	_, err := e.db.ExecContext(ctx, evalSchema)
	return err
}

// HasDay reports whether a price file for this source and date was already handled.
func (e *Eval) HasDay(ctx context.Context, src, date string) (bool, error) {
	var n int
	err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_days WHERE src = ? AND date = ?`, src, date).Scan(&n)
	return n > 0, err
}

// SaveDay stores one day's bars and records the day as handled (status OK or NOT_FOUND).
func (e *Eval) SaveDay(ctx context.Context, src, date, status string, bars []prices.DayBar, now time.Time) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO prices (src, code, date, isin, open, high, low, close, volume) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, b := range bars {
		if _, err := st.ExecContext(ctx, b.Src, b.Code, b.Date, b.ISIN, b.Open, b.High, b.Low, b.Close, b.Volume); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO price_days (src, date, status, rows, fetched_ms) VALUES (?,?,?,?,?)`,
		src, date, status, len(bars), now.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Eval) bars(ctx context.Context, where string, args []any, from, to string) ([]evaluator.Bar, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT date, open, high, low, close, volume FROM prices WHERE `+where+
		` AND date >= ? AND date <= ? ORDER BY date`, append(args, from, to)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []evaluator.Bar
	for rows.Next() {
		var b evaluator.Bar
		if err := rows.Scan(&b.Date, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Bars returns daily bars for one code on one source between two dates (inclusive).
func (e *Eval) Bars(ctx context.Context, src, code, from, to string) ([]evaluator.Bar, error) {
	return e.bars(ctx, `src = ? AND code = ?`, []any{src, code}, from, to)
}

// BarsByISIN returns daily bars for an ISIN on one source.
func (e *Eval) BarsByISIN(ctx context.Context, src, isin, from, to string) ([]evaluator.Bar, error) {
	return e.bars(ctx, `src = ? AND isin = ? AND isin <> ''`, []any{src, isin}, from, to)
}

// ScoreRow is a signal with the filing details needed to find its prices.
type ScoreRow struct {
	SignalID    int64
	Model       string
	Direction   string
	Confidence  float64
	Materiality string
	Source      string
	Symbol      string
	ScripCode   string
	ISIN        string
	Company     string
	PublishedMs int64
}

// ToScore lists signals that have no outcome yet, or only a NO_PRICES one (so they are
// retried after more price files arrive). redo lists every signal. model "" means all.
func (e *Eval) ToScore(ctx context.Context, model string, redo bool) ([]ScoreRow, error) {
	flag := 0
	if redo {
		flag = 1
	}
	rows, err := e.db.QueryContext(ctx, `SELECT s.id, s.model, s.direction, s.confidence, s.materiality, a.source,
 COALESCE(a.symbol,''), COALESCE(a.scrip_code,''), COALESCE(a.isin,''), COALESCE(a.company,''), a.disseminated_ms
 FROM signals s JOIN announcements a ON a.id = s.announcement_id
 LEFT JOIN outcomes o ON o.signal_id = s.id
 WHERE (? = '' OR s.model = ?) AND (? = 1 OR o.signal_id IS NULL OR o.status = 'NO_PRICES')
 ORDER BY a.disseminated_ms, s.id`, model, model, flag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoreRow
	for rows.Next() {
		var r ScoreRow
		if err := rows.Scan(&r.SignalID, &r.Model, &r.Direction, &r.Confidence, &r.Materiality, &r.Source,
			&r.Symbol, &r.ScripCode, &r.ISIN, &r.Company, &r.PublishedMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveOutcome stores (or replaces) the outcome of one signal.
func (e *Eval) SaveOutcome(ctx context.Context, signalID int64, priceSrc string, r evaluator.Result, now time.Time) error {
	hit := 0
	if r.Hit {
		hit = 1
	}
	_, err := e.db.ExecContext(ctx, `INSERT OR REPLACE INTO outcomes (signal_id, evaluated_ms, status, bucket, entry_date, price_src,
 gap_pct, open_close_pct, total_pct, excess_gap_pct, excess_open_close_pct, excess_total_pct, rel_volume, label, hit, exit_reason, trade_pct)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		signalID, now.UnixMilli(), r.Status, r.Bucket, r.EntryDate, priceSrc,
		r.GapPct, r.OpenToClosePct, r.TotalPct, r.ExcessGapPct, r.ExcessOpenToClosePct, r.ExcessTotalPct, r.RelVolume,
		r.Label, hit, r.ExitReason, r.TradePct)
	return err
}

// ScoreLine is one group of the scorecard: signals of one direction in one timing bucket.
type ScoreLine struct {
	Direction   string // UP | DOWN | NEUTRAL
	Bucket      string
	N, Hits     int
	Trades      int // signals that produced a simulated trade (not NEUTRAL)
	Wins        int // trades with a positive result after costs
	AvgTradePct float64
}

type Scorecard struct {
	Lines  []ScoreLine
	Labels map[string]int // actual direction of all scored signals (UP | DOWN | NEUTRAL)
	Status map[string]int // outcome status counts (OK, NO_PRICES, UNSUPPORTED_BUCKET)
}

// Scorecard summarises stored outcomes. model "" means all models (do not mix models in a real scorecard).
func (e *Eval) Scorecard(ctx context.Context, model string) (Scorecard, error) {
	sc := Scorecard{Labels: map[string]int{}, Status: map[string]int{}}
	rows, err := e.db.QueryContext(ctx, `SELECT UPPER(s.direction), o.bucket, COUNT(*), SUM(o.hit),
 SUM(CASE WHEN o.exit_reason <> 'no_trade' THEN 1 ELSE 0 END),
 SUM(CASE WHEN o.exit_reason <> 'no_trade' AND o.trade_pct > 0 THEN 1 ELSE 0 END),
 AVG(CASE WHEN o.exit_reason <> 'no_trade' THEN o.trade_pct END)
 FROM outcomes o JOIN signals s ON s.id = o.signal_id
 WHERE o.status = 'OK' AND (? = '' OR s.model = ?)
 GROUP BY UPPER(s.direction), o.bucket ORDER BY 1, 2`, model, model)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	for rows.Next() {
		var l ScoreLine
		var avg sql.NullFloat64
		if err := rows.Scan(&l.Direction, &l.Bucket, &l.N, &l.Hits, &l.Trades, &l.Wins, &avg); err != nil {
			return sc, err
		}
		l.AvgTradePct = avg.Float64
		sc.Lines = append(sc.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return sc, err
	}
	for _, q := range []struct {
		sql string
		dst map[string]int
	}{
		{`SELECT o.label, COUNT(*) FROM outcomes o JOIN signals s ON s.id = o.signal_id WHERE o.status = 'OK' AND (? = '' OR s.model = ?) GROUP BY o.label`, sc.Labels},
		{`SELECT o.status, COUNT(*) FROM outcomes o JOIN signals s ON s.id = o.signal_id WHERE (? = '' OR s.model = ?) GROUP BY o.status`, sc.Status},
	} {
		r2, err := e.db.QueryContext(ctx, q.sql, model, model)
		if err != nil {
			return sc, err
		}
		for r2.Next() {
			var k string
			var n int
			if err := r2.Scan(&k, &n); err != nil {
				r2.Close()
				return sc, err
			}
			q.dst[k] = n
		}
		if err := r2.Err(); err != nil {
			r2.Close()
			return sc, err
		}
		r2.Close()
	}
	return sc, nil
}
