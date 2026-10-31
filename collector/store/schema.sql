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

-- M3: what the filter decided about every filing, and the model's signals (D-008, D-055, D-064).
CREATE TABLE IF NOT EXISTS triage (
  announcement_id INTEGER PRIMARY KEY REFERENCES announcements(id),
  decision        TEXT NOT NULL,            -- ANALYZE | REVIEW | SKIP | DUPLICATE | ERROR
  reason          TEXT NOT NULL,
  updated_ms      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS signals (
  id              INTEGER PRIMARY KEY,
  announcement_id INTEGER NOT NULL REFERENCES announcements(id),
  model           TEXT NOT NULL,            -- e.g. gemini:gemini-3.5-flash-lite (never mix models in a scorecard)
  direction       TEXT NOT NULL,            -- up | down | neutral
  confidence      REAL NOT NULL,
  materiality     TEXT NOT NULL,            -- high | medium | low
  reason          TEXT NOT NULL,
  text_hash       TEXT NOT NULL DEFAULT '', -- hash of the attachment text analysed ('' = metadata only)
  text_chars      INTEGER NOT NULL DEFAULT 0,
  input_tokens    INTEGER NOT NULL DEFAULT 0,
  output_tokens   INTEGER NOT NULL DEFAULT 0,
  created_ms      INTEGER NOT NULL,         -- signal_time (D-009)
  notified_ms     INTEGER,                  -- when the Telegram message was sent
  UNIQUE (announcement_id, model)
);
CREATE INDEX IF NOT EXISTS idx_signals_created ON signals (created_ms);
CREATE INDEX IF NOT EXISTS idx_signals_hash    ON signals (text_hash);
