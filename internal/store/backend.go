package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backend persists the in-memory state. save is called with the store lock
// held after every change; a failed save is retried by the next one, because
// backends compare against what they last wrote, not against the change.
type backend interface {
	load(ctx context.Context, d *data) error
	save(ctx context.Context, d *data) error
	close()
}

type fileBackend struct{ path string }

func openFile(dir string) (*fileBackend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return &fileBackend{path: filepath.Join(dir, "state.json")}, nil
}

func (f *fileBackend) load(_ context.Context, d *data) error {
	raw, err := os.ReadFile(f.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(raw, d); err != nil {
		return fmt.Errorf("parse %s: %w", f.path, err)
	}
	if d.Templates == nil {
		d.Templates = map[string]*Template{}
	}
	if d.Sandboxes == nil {
		d.Sandboxes = map[string]*Sandbox{}
	}

	return nil
}

func (f *fileBackend) save(_ context.Context, d *data) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}

	return nil
}

func (f *fileBackend) close() {}

// pgBackend keeps one row per template and per sandbox, the record as JSONB.
// It holds a session advisory lock for its lifetime, so a second API pointed
// at the same database refuses to start instead of overwriting state.
type pgBackend struct {
	pool *pgxpool.Pool
	lock *pgxpool.Conn
	// written is what each row held after the last successful save.
	written map[string][]byte
}

const pgLockKey = 0x6532626d696e // "e2bmin"

const pgSchema = `
CREATE TABLE IF NOT EXISTS e2b_templates (
	id         text PRIMARY KEY,
	data       jsonb NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS e2b_sandboxes (
	id         text PRIMARY KEY,
	data       jsonb NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now()
);`

func openPostgres(ctx context.Context, url string) (*pgBackend, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	lock, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()

		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	var got bool
	if err := lock.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(pgLockKey)).Scan(&got); err != nil {
		lock.Release()
		pool.Close()

		return nil, fmt.Errorf("postgres: advisory lock: %w", err)
	}
	if !got {
		lock.Release()
		pool.Close()

		return nil, errors.New("postgres: another e2b-minimal holds this database; run one API per database")
	}
	if _, err := pool.Exec(ctx, pgSchema); err != nil {
		lock.Release()
		pool.Close()

		return nil, fmt.Errorf("postgres: create schema: %w", err)
	}

	return &pgBackend{pool: pool, lock: lock, written: map[string][]byte{}}, nil
}

func (p *pgBackend) load(ctx context.Context, d *data) error {
	for _, table := range []string{"e2b_templates", "e2b_sandboxes"} {
		rows, err := p.pool.Query(ctx, `SELECT id, data FROM `+table)
		if err != nil {
			return fmt.Errorf("postgres: load %s: %w", table, err)
		}
		for rows.Next() {
			var (
				id  string
				raw []byte
			)
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()

				return fmt.Errorf("postgres: load %s: %w", table, err)
			}
			if table == "e2b_templates" {
				var t Template
				if err := json.Unmarshal(raw, &t); err != nil {
					rows.Close()

					return fmt.Errorf("postgres: template %s: %w", id, err)
				}
				d.Templates[id] = &t
			} else {
				var s Sandbox
				if err := json.Unmarshal(raw, &s); err != nil {
					rows.Close()

					return fmt.Errorf("postgres: sandbox %s: %w", id, err)
				}
				d.Sandboxes[id] = &s
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("postgres: load %s: %w", table, err)
		}
	}
	// Remember the loaded rows in their re-encoded form, so the first save
	// only writes what really changed.
	next, err := encodeRows(d)
	if err != nil {
		return err
	}
	p.written = next

	return nil
}

// encodeRows renders every record keyed by "<table>/<id>".
func encodeRows(d *data) (map[string][]byte, error) {
	out := make(map[string][]byte, len(d.Templates)+len(d.Sandboxes))
	for id, t := range d.Templates {
		raw, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("encode template %s: %w", id, err)
		}
		out["e2b_templates/"+id] = raw
	}
	for id, s := range d.Sandboxes {
		raw, err := json.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("encode sandbox %s: %w", id, err)
		}
		out["e2b_sandboxes/"+id] = raw
	}

	return out, nil
}

func (p *pgBackend) save(ctx context.Context, d *data) error {
	next, err := encodeRows(d)
	if err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for key, raw := range next {
		if bytes.Equal(p.written[key], raw) {
			continue
		}
		table, id := splitKey(key)
		batch.Queue(`INSERT INTO `+table+` (id, data, updated_at) VALUES ($1, $2, now())
			ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, id, raw)
	}
	for key := range p.written {
		if _, ok := next[key]; !ok {
			table, id := splitKey(key)
			batch.Queue(`DELETE FROM `+table+` WHERE id = $1`, id)
		}
	}
	if batch.Len() == 0 {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("postgres: save: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	p.written = next

	return nil
}

func splitKey(key string) (table, id string) {
	for i := range len(key) {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}

	return key, ""
}

func (p *pgBackend) close() {
	p.lock.Release()
	p.pool.Close()
}
