-- agent-fs community schema v2.
-- The operating-system filesystem is the source of truth. This database is a
-- transactional, rebuildable semantic index over that filesystem.

CREATE TABLE IF NOT EXISTS files (
  id            INTEGER PRIMARY KEY,
  parent_id     INTEGER REFERENCES files(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  scan_root     TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  path          TEXT    NOT NULL UNIQUE,
  kind          TEXT    NOT NULL CHECK (kind IN ('file', 'dir', 'symlink', 'other')),
  size          INTEGER NOT NULL CHECK (size >= 0),
  mtime_ns      INTEGER NOT NULL,
  link_target   TEXT    NOT NULL DEFAULT '',
  content_head  TEXT    NOT NULL DEFAULT '',
  content_hash  TEXT    NOT NULL DEFAULT '',
  mime          TEXT    NOT NULL DEFAULT '',
  symbols_text  TEXT    NOT NULL DEFAULT '',
  tags_text     TEXT    NOT NULL DEFAULT '',
  -- CJK bigram 补充索引文本，由 Go 侧 segmentCJKIndex 计算。unicode61 会把整段连写
  -- 的汉字当成一个 token，导致中文无法中缀匹配；把 bigram 预先切开存在这里，中缀
  -- 匹配就退化成 FTS5 本来就支持的整词匹配。纯拉丁内容此列为空。
  search_text   TEXT    NOT NULL DEFAULT '',
  indexed_at_ns INTEGER NOT NULL,
  UNIQUE(parent_id, name)
);

CREATE INDEX IF NOT EXISTS idx_files_parent ON files(parent_id);
CREATE INDEX IF NOT EXISTS idx_files_root   ON files(scan_root);
CREATE INDEX IF NOT EXISTS idx_files_kind   ON files(kind);
CREATE INDEX IF NOT EXISTS idx_files_size   ON files(size);
CREATE INDEX IF NOT EXISTS idx_files_mtime  ON files(mtime_ns);

CREATE TABLE IF NOT EXISTS tags (
  file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  tag     TEXT    NOT NULL CHECK (length(tag) > 0),
  PRIMARY KEY (file_id, tag)
);

CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag);

CREATE TABLE IF NOT EXISTS scan_roots (
  path             TEXT PRIMARY KEY,
  last_scan_ns     INTEGER NOT NULL,
  last_duration_ns INTEGER NOT NULL,
  entry_count      INTEGER NOT NULL CHECK (entry_count >= 0)
);

-- Compact float32 vectors are generated locally by default. bucket is a
-- sign-LSH prefix, making vector candidate lookup sublinear at large scale.
CREATE TABLE IF NOT EXISTS embeddings (
  file_id       INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  model         TEXT    NOT NULL,
  dimensions    INTEGER NOT NULL CHECK (dimensions > 0),
  bucket        INTEGER NOT NULL,
  vector        BLOB    NOT NULL,
  content_hash  TEXT    NOT NULL,
  updated_at_ns INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_embeddings_model_bucket
  ON embeddings(model, bucket);

-- Parser-produced chunks preserve code symbols and source ranges. Search can
-- return a precise declaration instead of paying to inject an entire file.
CREATE TABLE IF NOT EXISTS chunks (
  id            INTEGER PRIMARY KEY,
  file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  ordinal       INTEGER NOT NULL,
  language      TEXT    NOT NULL DEFAULT '',
  symbol        TEXT    NOT NULL DEFAULT '',
  start_line    INTEGER NOT NULL DEFAULT 0,
  end_line      INTEGER NOT NULL DEFAULT 0,
  content       TEXT    NOT NULL,
  content_hash  TEXT    NOT NULL,
  search_text   TEXT    NOT NULL DEFAULT '',
  UNIQUE(file_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_chunks_file_symbol ON chunks(file_id, symbol);

-- 符号图：函数/符号定义与调用引用。让检索能回答「谁定义了这个符号」
-- 以及「谁调用了这个函数」，而非只能命中文件。
CREATE TABLE IF NOT EXISTS symbols (
  id          INTEGER PRIMARY KEY,
  file_id     INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  symbol      TEXT    NOT NULL,
  kind        TEXT    NOT NULL DEFAULT 'function',
  start_line  INTEGER NOT NULL DEFAULT 0,
  end_line    INTEGER NOT NULL DEFAULT 0,
  UNIQUE(file_id, symbol, start_line)
);

CREATE INDEX IF NOT EXISTS idx_symbols_symbol ON symbols(symbol);

CREATE TABLE IF NOT EXISTS symbol_refs (
  id            INTEGER PRIMARY KEY,
  file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  caller_symbol TEXT    NOT NULL DEFAULT '',
  callee_symbol TEXT    NOT NULL,
  line          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_symbol_refs_callee ON symbol_refs(callee_symbol);
CREATE INDEX IF NOT EXISTS idx_symbol_refs_caller ON symbol_refs(caller_symbol);

-- search_text 追加在最后一列，这样已有的按列序引用（如 snippet 的列下标）不受影响。
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  symbol,
  language,
  content,
  search_text,
  content = 'chunks',
  content_rowid = 'id',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO chunks_fts(rowid, symbol, language, content, search_text)
  VALUES (new.id, new.symbol, new.language, new.content, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, symbol, language, content, search_text)
  VALUES ('delete', old.id, old.symbol, old.language, old.content, old.search_text);
END;

CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, symbol, language, content, search_text)
  VALUES ('delete', old.id, old.symbol, old.language, old.content, old.search_text);
  INSERT INTO chunks_fts(rowid, symbol, language, content, search_text)
  VALUES (new.id, new.symbol, new.language, new.content, new.search_text);
END;

CREATE TABLE IF NOT EXISTS chunk_embeddings (
  chunk_id       INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
  model          TEXT    NOT NULL,
  dimensions     INTEGER NOT NULL CHECK (dimensions > 0),
  bucket         INTEGER NOT NULL,
  vector         BLOB    NOT NULL,
  content_hash   TEXT    NOT NULL,
  updated_at_ns  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chunk_embeddings_model_bucket
  ON chunk_embeddings(model, bucket);

-- Durable intent records let startup finish an operation that was interrupted
-- between the filesystem rename and the SQLite commit.
CREATE TABLE IF NOT EXISTS operation_journal (
  id            INTEGER PRIMARY KEY,
  operation     TEXT    NOT NULL CHECK (operation IN ('rename', 'remove')),
  state         TEXT    NOT NULL,
  old_path      TEXT    NOT NULL,
  new_path      TEXT    NOT NULL DEFAULT '',
  stage_path    TEXT    NOT NULL DEFAULT '',
  last_error    TEXT    NOT NULL DEFAULT '',
  created_at_ns INTEGER NOT NULL,
  updated_at_ns INTEGER NOT NULL
);

-- External-content FTS keeps one stable rowid per files.id. Triggers make the
-- full-text index part of the same transaction as files/tags updates.
-- search_text 追加在最后一列（下标 4），content_head 仍是下标 3，所以 query.go 里
-- snippet(files_fts, 3, ...) 的列下标保持有效。
CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
  name,
  path,
  tags_text,
  content_head,
  search_text,
  content = 'files',
  content_rowid = 'id',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, name, path, tags_text, content_head, search_text)
  VALUES (new.id, new.name, new.path, new.tags_text, new.content_head, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, path, tags_text, content_head, search_text)
  VALUES ('delete', old.id, old.name, old.path, old.tags_text, old.content_head, old.search_text);
END;

CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name, path, tags_text, content_head, search_text)
  VALUES ('delete', old.id, old.name, old.path, old.tags_text, old.content_head, old.search_text);
  INSERT INTO files_fts(rowid, name, path, tags_text, content_head, search_text)
  VALUES (new.id, new.name, new.path, new.tags_text, new.content_head, new.search_text);
END;

CREATE TRIGGER IF NOT EXISTS tags_ai AFTER INSERT ON tags BEGIN
  UPDATE files
  SET tags_text = COALESCE((
    SELECT group_concat(tag, ' ')
    FROM (SELECT tag FROM tags WHERE file_id = new.file_id ORDER BY tag)
  ), '')
  WHERE id = new.file_id;
END;

CREATE TRIGGER IF NOT EXISTS tags_ad AFTER DELETE ON tags BEGIN
  UPDATE files
  SET tags_text = COALESCE((
    SELECT group_concat(tag, ' ')
    FROM (SELECT tag FROM tags WHERE file_id = old.file_id ORDER BY tag)
  ), '')
  WHERE id = old.file_id;
END;

PRAGMA user_version = 2;
