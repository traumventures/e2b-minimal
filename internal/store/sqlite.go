package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure Go driver, no CGO
)

// sqliteBackend keeps the same two tables as Postgres in one SQLite file.
// The connection holds an exclusive lock on the file for its lifetime, so a
// second API on the same file refuses to start.
type sqliteBackend struct {
	db      *sql.DB
	path    string
	written map[string][]byte
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS e2b_templates (
	id         TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE TABLE IF NOT EXISTS e2b_sandboxes (
	id         TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);`

func openSQLite(ctx context.Context, dir string) (*sqliteBackend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dir, "state.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	// One connection: it holds the exclusive lock and serialises writes.
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		db.Close()
		if bytes.Contains([]byte(err.Error()), []byte("locked")) || bytes.Contains([]byte(err.Error()), []byte("busy")) {
			return nil, fmt.Errorf("sqlite: %s is in use by another e2b-minimal; run one API per data dir", path)
		}

		return nil, fmt.Errorf("sqlite: create schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("chmod state.db", "err", err)
	}

	return &sqliteBackend{db: db, path: path, written: map[string][]byte{}}, nil
}

func (b *sqliteBackend) load(ctx context.Context, d *data) error {
	for _, table := range []string{"e2b_templates", "e2b_sandboxes"} {
		rows, err := b.db.QueryContext(ctx, `SELECT id, data FROM `+table)
		if err != nil {
			return fmt.Errorf("sqlite: load %s: %w", table, err)
		}
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()

				return fmt.Errorf("sqlite: load %s: %w", table, err)
			}
			if err := decodeRow(table, id, []byte(raw), d); err != nil {
				rows.Close()

				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("sqlite: load %s: %w", table, err)
		}
	}
	next, err := encodeRows(d)
	if err != nil {
		return err
	}
	b.written = next

	return b.importJSON(ctx, d)
}

// importJSON takes over the state.json of earlier versions once, when the
// database is still empty, and renames the file so it is not read again.
func (b *sqliteBackend) importJSON(ctx context.Context, d *data) error {
	old := filepath.Join(filepath.Dir(b.path), "state.json")
	raw, err := os.ReadFile(old)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", old, err)
	}
	if len(d.Templates) > 0 || len(d.Sandboxes) > 0 {
		slog.Warn("state.db already has data, leaving the old state.json alone", "path", old)

		return nil
	}
	if err := json.Unmarshal(raw, d); err != nil {
		return fmt.Errorf("parse %s: %w", old, err)
	}
	if d.Templates == nil {
		d.Templates = map[string]*Template{}
	}
	if d.Sandboxes == nil {
		d.Sandboxes = map[string]*Sandbox{}
	}
	if err := b.save(ctx, d); err != nil {
		return err
	}
	if err := os.Rename(old, old+".imported"); err != nil {
		return fmt.Errorf("rename %s: %w", old, err)
	}
	slog.Info("imported state.json into state.db", "templates", len(d.Templates), "sandboxes", len(d.Sandboxes))

	return nil
}

func (b *sqliteBackend) save(ctx context.Context, d *data) error {
	next, err := encodeRows(d)
	if err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for key, raw := range next {
		if bytes.Equal(b.written[key], raw) {
			continue
		}
		table, id := splitKey(key)
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (id, data) VALUES (?, ?)
			ON CONFLICT (id) DO UPDATE SET data = excluded.data,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`, id, string(raw)); err != nil {
			return fmt.Errorf("sqlite: save %s: %w", key, err)
		}
		changed = true
	}
	for key := range b.written {
		if _, ok := next[key]; !ok {
			table, id := splitKey(key)
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id); err != nil {
				return fmt.Errorf("sqlite: delete %s: %w", key, err)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	b.written = next

	return nil
}

func (b *sqliteBackend) close() { _ = b.db.Close() }
