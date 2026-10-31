package store

import (
	"context"
	"strings"
	"time"

	"stonks/analyzer"
	"stonks/announce"
)

// Row is an announcement together with its database id and when we first saw it.
type Row struct {
	ID        int64
	A         announce.Announcement
	FirstSeen time.Time
	Backlog   bool
}

// SignalRef is the part of a stored signal needed to recognise duplicates.
type SignalRef struct {
	ID             int64
	AnnouncementID int64
	A              announce.Announcement // source, company, attachment and exchange time only
	TextHash       string
}

// SignalRecord is one model answer to store.
type SignalRecord struct {
	AnnouncementID int64
	Sig            analyzer.Signal
	TextHash       string
	TextChars      int
	Created        time.Time
}

const annCols = `a.id, a.source, a.source_id, COALESCE(a.symbol,''), COALESCE(a.scrip_code,''), COALESCE(a.isin,''),
 COALESCE(a.company,''), COALESCE(a.category,''), COALESCE(a.subcategory,''), COALESCE(a.headline,''), COALESCE(a.subject,''),
 COALESCE(a.attachment_url,''), COALESCE(a.attachment_name,''), COALESCE(a.submitted_at_ms,0), a.disseminated_ms, a.first_seen_ms, a.backlog`

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner) (Row, error) {
	var r Row
	var sub, dis, seen int64
	var bl int
	err := sc.Scan(&r.ID, &r.A.Source, &r.A.SourceID, &r.A.Symbol, &r.A.ScripCode, &r.A.ISIN, &r.A.Company, &r.A.Category,
		&r.A.Subcategory, &r.A.Headline, &r.A.Subject, &r.A.AttachmentURL, &r.A.AttachmentName, &sub, &dis, &seen, &bl)
	if err != nil {
		return r, err
	}
	r.A.SubmittedAt = time.UnixMilli(sub).In(announce.IST)
	r.A.DisseminatedAt = time.UnixMilli(dis).In(announce.IST)
	r.FirstSeen = time.UnixMilli(seen).In(announce.IST)
	r.Backlog = bl != 0
	return r, nil
}

func (s *SQLite) queryRows(ctx context.Context, q string, args ...any) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Untriaged returns filings published since sinceMs that the filter has not looked at yet, oldest first.
func (s *SQLite) Untriaged(ctx context.Context, sinceMs int64, limit int) ([]Row, error) {
	return s.queryRows(ctx, `SELECT `+annCols+` FROM announcements a
 LEFT JOIN triage t ON t.announcement_id = a.id
 WHERE t.announcement_id IS NULL AND a.disseminated_ms >= ?
 ORDER BY a.disseminated_ms ASC LIMIT ?`, sinceMs, limit)
}

// SetTriage records (or updates) the filter's decision for a filing.
func (s *SQLite) SetTriage(ctx context.Context, id int64, decision, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO triage (announcement_id, decision, reason, updated_ms) VALUES (?,?,?,?)
 ON CONFLICT(announcement_id) DO UPDATE SET decision=excluded.decision, reason=excluded.reason, updated_ms=excluded.updated_ms`,
		id, decision, reason, now.UnixMilli())
	return err
}

// PendingAnalysis returns ANALYZE-class filings that have no signal from this model yet, oldest first.
func (s *SQLite) PendingAnalysis(ctx context.Context, model string, sinceMs int64, limit int) ([]Row, error) {
	return s.queryRows(ctx, `SELECT `+annCols+` FROM announcements a
 JOIN triage t ON t.announcement_id = a.id
 WHERE t.decision = 'ANALYZE' AND a.disseminated_ms >= ?
   AND NOT EXISTS (SELECT 1 FROM signals s WHERE s.announcement_id = a.id AND s.model = ?)
 ORDER BY a.disseminated_ms ASC LIMIT ?`, sinceMs, model, limit)
}

// RecentSignals lists signals for filings published since sinceMs, for duplicate detection.
func (s *SQLite) RecentSignals(ctx context.Context, sinceMs int64) ([]SignalRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.announcement_id, s.text_hash, a.source, COALESCE(a.company,''),
 COALESCE(a.attachment_url,''), COALESCE(a.attachment_name,''), a.disseminated_ms
 FROM signals s JOIN announcements a ON a.id = s.announcement_id WHERE a.disseminated_ms >= ?`, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalRef
	for rows.Next() {
		var r SignalRef
		var dis int64
		if err := rows.Scan(&r.ID, &r.AnnouncementID, &r.TextHash, &r.A.Source, &r.A.Company, &r.A.AttachmentURL, &r.A.AttachmentName, &dis); err != nil {
			return nil, err
		}
		r.A.DisseminatedAt = time.UnixMilli(dis).In(announce.IST)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveSignal stores a signal and returns its id (0 if this announcement already has one from this model).
func (s *SQLite) SaveSignal(ctx context.Context, rec SignalRecord) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO signals
 (announcement_id, model, direction, confidence, materiality, reason, text_hash, text_chars, input_tokens, output_tokens, created_ms)
 VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(announcement_id, model) DO NOTHING`,
		rec.AnnouncementID, rec.Sig.Model, rec.Sig.Direction, rec.Sig.Confidence, rec.Sig.Materiality, strings.TrimSpace(rec.Sig.Reason),
		rec.TextHash, rec.TextChars, rec.Sig.InputTokens, rec.Sig.OutputTokens, rec.Created.UnixMilli())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	return res.LastInsertId()
}

// CountSignalsSince counts real model calls (signals with token counts) by this model since sinceMs.
func (s *SQLite) CountSignalsSince(ctx context.Context, sinceMs int64, model string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signals WHERE created_ms >= ? AND model = ? AND input_tokens > 0`, sinceMs, model).Scan(&n)
	return n, err
}

// MarkNotified records that the Telegram message for a signal was sent.
func (s *SQLite) MarkNotified(ctx context.Context, signalID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE signals SET notified_ms = ? WHERE id = ?`, now.UnixMilli(), signalID)
	return err
}
