-- In-app notices: what the apps show their users when they open (maintenance, news, an update
-- they must install). They are not tied to bills: apps says which app each is for.
CREATE TABLE IF NOT EXISTS notices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('info', 'warning', 'maintenance')),
    lang        TEXT NOT NULL DEFAULT 'en',   -- only 'en' for now; the column is here for other languages
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    link_url    TEXT NOT NULL DEFAULT '',
    link_label  TEXT NOT NULL DEFAULT '',
    starts_at   INTEGER NOT NULL,             -- users see it from here
    ends_at     INTEGER NOT NULL DEFAULT 0,   -- ... until here; 0 means until someone ends it
    ended_at    INTEGER NOT NULL DEFAULT 0,   -- set when someone ends it by hand
    dismissible INTEGER NOT NULL DEFAULT 1,
    blocking    INTEGER NOT NULL DEFAULT 0,   -- while the block window is open, new orders are refused
    block_from  INTEGER NOT NULL DEFAULT 0,   -- 0 means starts_at
    block_until INTEGER NOT NULL DEFAULT 0,   -- 0 means the notice's own end
    apps        TEXT NOT NULL DEFAULT '',     -- comma separated; empty means every app
    platforms   TEXT NOT NULL DEFAULT '',     -- comma separated (android, ios, ...); empty means all
    min_version TEXT NOT NULL DEFAULT '',     -- the oldest app version it applies to; empty means no limit
    max_version TEXT NOT NULL DEFAULT '',     -- the newest app version it applies to; empty means no limit
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS notices_window ON notices (starts_at, ends_at);
