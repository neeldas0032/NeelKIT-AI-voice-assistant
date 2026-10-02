-- Full-text search over transcripts.
--
-- FTS5 is the reason this project needs no search service: searchable voice
-- history is a headline feature and it costs one virtual table plus three
-- triggers to keep it in sync.
-- unicode61 with prefix indexes, deliberately NOT the porter stemmer.
--
-- Porter is asymmetric in a way that makes voice search feel broken: it stems
-- "deployment" to "deploy" but the query "deploy" to "deploi", so searching for
-- the word you just said finds nothing. Prefix indexes give the behaviour
-- people actually expect - typing "auth" finds "authentication" - with no
-- surprises to explain.
CREATE VIRTUAL TABLE transcripts_fts USING fts5(
  text,
  content='transcripts',
  content_rowid='rowid',
  tokenize='unicode61 remove_diacritics 2',
  prefix='2 3 4 5'
);

CREATE TRIGGER transcripts_ai AFTER INSERT ON transcripts BEGIN
  INSERT INTO transcripts_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER transcripts_ad AFTER DELETE ON transcripts BEGIN
  INSERT INTO transcripts_fts(transcripts_fts, rowid, text) VALUES('delete', old.rowid, old.text);
END;

CREATE TRIGGER transcripts_au AFTER UPDATE ON transcripts BEGIN
  INSERT INTO transcripts_fts(transcripts_fts, rowid, text) VALUES('delete', old.rowid, old.text);
  INSERT INTO transcripts_fts(rowid, text) VALUES (new.rowid, new.text);
END;
