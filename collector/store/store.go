// Package store persists announcements in SQLite (D-004). It only uses database/sql;
// the driver is imported by the main program.
package store

import (
	"context"
	_ "embed"
	"database/sql"
	"strings"
	"time"

	"stonks/announce"
)

//go:embed schema.sql
var schema string

type SQLite struct{ db *sql.DB }

func New(db *sql.DB) *SQLite { return &SQLite{db: db} }

func (s *SQLite) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Seen returns which of the given source IDs are already stored.
func (s *SQLite) Seen(ctx context.Context, source string, ids []string) (map[string]bool, error) {
	seen := make(map[string]bool)
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		q := "SELECT source_id FROM announcements WHERE source = ? AND source_id IN (?" + strings.Repeat(",?", len(chunk)-1) + ")"
		args := []any{source}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			seen[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return seen, nil
}

const insertSQL = `INSERT INTO announcements
 (source, source_id, symbol, scrip_code, isin, company, category, subcategory, headline, subject,
  attachment_url, attachment_name, submitted_at_ms, disseminated_ms, first_seen_ms, backlog, raw_json)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(source, source_id) DO NOTHING`

// Insert stores the rows and returns only those that were actually new.
func (s *SQLite) Insert(ctx context.Context, rows []announce.Announcement, firstSeen time.Time, backlog bool) ([]announce.Announcement, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	bl := 0
	if backlog {
		bl = 1
	}
	var inserted []announce.Announcement
	for _, a := range rows {
		res, err := stmt.ExecContext(ctx, a.Source, a.SourceID, a.Symbol, a.ScripCode, a.ISIN, a.Company, a.Category,
			a.Subcategory, a.Headline, a.Subject, a.AttachmentURL, a.AttachmentName,
			a.SubmittedAt.UnixMilli(), a.DisseminatedAt.UnixMilli(), firstSeen.UnixMilli(), bl, string(a.Raw))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			inserted = append(inserted, a)
		}
	}
	return inserted, tx.Commit()
}

// Recent returns the newest announcements by exchange time (without the raw JSON).
func (s *SQLite) Recent(ctx context.Context, limit int) ([]announce.Announcement, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, source_id, COALESCE(symbol,''), COALESCE(scrip_code,''),
 COALESCE(isin,''), COALESCE(company,''), COALESCE(category,''), COALESCE(subcategory,''), COALESCE(headline,''),
 COALESCE(subject,''), COALESCE(attachment_url,''), COALESCE(attachment_name,''), COALESCE(submitted_at_ms,0), disseminated_ms
 FROM announcements ORDER BY disseminated_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []announce.Announcement
	for rows.Next() {
		var a announce.Announcement
		var sub, dis int64
		if err := rows.Scan(&a.Source, &a.SourceID, &a.Symbol, &a.ScripCode, &a.ISIN, &a.Company, &a.Category,
			&a.Subcategory, &a.Headline, &a.Subject, &a.AttachmentURL, &a.AttachmentName, &sub, &dis); err != nil {
			return nil, err
		}
		a.SubmittedAt = time.UnixMilli(sub).In(announce.IST)
		a.DisseminatedAt = time.UnixMilli(dis).In(announce.IST)
		out = append(out, a)
	}
	return out, rows.Err()
}
