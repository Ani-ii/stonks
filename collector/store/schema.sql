-- SQLite schema for the announcement store (D-004, D-009, D-023).
-- All times are stored as Unix milliseconds (UTC); the exchanges' IST strings are converted on ingest.
CREATE TABLE IF NOT EXISTS announcements (
  id               INTEGER PRIMARY KEY,
  source           TEXT NOT NULL CHECK (source IN ('NSE','BSE')),
  source_id        TEXT NOT NULL,              -- NSE seq_id / BSE NEWSID
  symbol           TEXT,                       -- NSE symbol (BSE filled by mapping)
  scrip_code       TEXT,                       -- BSE code (NSE filled by mapping)
  isin             TEXT,
  company          TEXT,
  category         TEXT,
  subcategory      TEXT,
  headline         TEXT,
  subject          TEXT,
  attachment_url   TEXT,
  attachment_name  TEXT,
  submitted_at_ms  INTEGER,                    -- NSE an_dt / BSE News_submission_dt
  disseminated_ms  INTEGER NOT NULL,           -- exchange time (D-021)
  first_seen_ms    INTEGER NOT NULL,           -- our poller's clock (D-009)
  backlog          INTEGER NOT NULL DEFAULT 0, -- 1 = inserted in a run's first cycle; first_seen is NOT a true detection time
  event_id         INTEGER,                    -- cross-exchange grouping (Q-5), filled later
  raw_json         TEXT NOT NULL,              -- untouched row (D-023)
  UNIQUE (source, source_id)
);
CREATE INDEX IF NOT EXISTS idx_ann_time    ON announcements (disseminated_ms);
CREATE INDEX IF NOT EXISTS idx_ann_symbol  ON announcements (symbol, disseminated_ms);
CREATE INDEX IF NOT EXISTS idx_ann_event   ON announcements (event_id);
