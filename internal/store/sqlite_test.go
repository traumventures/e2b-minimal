package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLitePersistsAndDeletes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Update(func(tx Tx) error {
		tx.PutTemplate(&Template{ID: "t1", Names: []string{"base"}, CreatedAt: now, Builds: []*Build{{ID: "b1", Status: BuildReady}}})
		tx.PutSandbox(&Sandbox{ID: "s1", TemplateID: "t1", State: SandboxPaused, Metadata: map[string]string{"k": "v"}})
		tx.PutSandbox(&Sandbox{ID: "s2", TemplateID: "t1", State: SandboxRunning})

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error { tx.DeleteSandbox("s2"); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.View(func(tx Tx) {
		if tx.ResolveTemplate("base") == nil || tx.Template("t1").ReadyBuild().ID != "b1" {
			t.Error("template not restored")
		}
		if sb := tx.Sandbox("s1"); sb == nil || sb.Metadata["k"] != "v" || sb.State != SandboxPaused {
			t.Errorf("sandbox not restored: %+v", sb)
		}
		if tx.Sandbox("s2") != nil {
			t.Error("deleted sandbox came back")
		}
	})
}

func TestSQLiteRefusesSecondOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(tx Tx) error { tx.PutSandbox(&Sandbox{ID: "x"}); return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, Options{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "another e2b-minimal") {
		t.Fatalf("second open: want refusal, got %v", err)
	}
}

func TestSQLiteImportsStateJSON(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := `{"templates":{"t1":{"id":"t1","names":["base"],"builds":[{"id":"b1","status":"ready"}]}},"sandboxes":{"s1":{"id":"s1","state":"paused"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "state.json.imported")); err != nil {
		t.Fatal("state.json not renamed after import")
	}
	s, err = Open(ctx, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.View(func(tx Tx) {
		if tx.ResolveTemplate("base") == nil || tx.Sandbox("s1") == nil {
			t.Error("imported state missing after reopen")
		}
	})
}
