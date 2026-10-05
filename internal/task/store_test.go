package task

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

var ctx = context.Background()

func TestScanRecordRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "steward.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{RunID: "r1", Mode: "baseline", SourcePath: "/src", BaseCommit: "c", BaseTree: "t", BaseRef: "main", WorkspaceDir: "/ws"}); err != nil {
		t.Fatal(err)
	}
	output := bytes.Repeat([]byte(`{"progress":{"message":"x"}}`+"\n"), 1000)
	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := ScanRecord{ID: "s1", RunID: "r1", Kind: "base", TreeHash: "tree", ConfigHash: "cfg", ToolchainDigest: "sha256:img", ScannerVersion: "v1.1.4", ScannerSHA256: "abc",
		DBSnapshotID: "snap", DBModified: mod, GoVersion: "go1.22.12", OutputSHA256: "ignored", Conclusive: true, Scan: json.RawMessage(`{"conclusive":true,"findings":[]}`), Output: output, Stderr: "note"}
	if err := s.InsertScan(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec2 := rec
	rec2.ID, rec2.Kind, rec2.TreeHash, rec2.Conclusive, rec2.Reason, rec2.Output = "s2", "post", "tree2", false, "scanner timed out", nil
	if err := s.InsertScan(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListScans(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s2" || got[1].ID != "s1" {
		t.Fatalf("scans = %+v", got)
	}
	b := got[1]
	if !bytes.Equal(b.Output, output) || b.OutputSHA256 == "ignored" || len(b.OutputSHA256) != 64 || !b.DBModified.Equal(mod) || !b.Conclusive || b.Kind != "base" || b.ScannerSHA256 != "abc" || b.Stderr != "note" || string(b.Scan) != string(rec.Scan) {
		t.Fatalf("base scan = %+v", b)
	}
	if got[0].Conclusive || got[0].Reason != "scanner timed out" || len(got[0].Output) != 0 {
		t.Fatalf("post scan = %+v", got[0])
	}
	// The raw output is stored compressed.
	var stored int
	s.db.QueryRowContext(ctx, `SELECT length(output_gz) FROM scan_runs WHERE id='s1'`).Scan(&stored)
	if stored == 0 || stored >= len(output)/10 {
		t.Fatalf("stored %d bytes for %d bytes of output", stored, len(output))
	}
	// A tampered output no longer matches its digest.
	s.db.ExecContext(ctx, `UPDATE scan_runs SET output_sha256='00' WHERE id='s1'`)
	if _, err := s.ListScans(ctx, "r1"); err == nil {
		t.Fatal("a scan whose output does not match its digest was returned")
	}
}

// A data directory created before scans were recorded opens, keeps its
// rows, and gains the scan table.
func TestOpen_MigratesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steward.db")
	if err := createOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steward_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:3] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO steward_migrations (version, applied_at) VALUES (?, ?)`, i+1, now())
	}
	if _, err := db.Exec(`INSERT INTO tasks (run_id, mode, source_path, base_commit, base_tree, base_ref, workspace_dir, created_at) VALUES ('old','baseline','/src','c','t','main','/ws','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if tk, err := s.GetTask(ctx, "old"); err != nil || tk.Mode != "baseline" {
		t.Fatalf("old task = %+v, %v", tk, err)
	}
	if err := s.InsertScan(ctx, ScanRecord{ID: "s", RunID: "old", Kind: "base", Scan: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var version int
	s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM steward_migrations`).Scan(&version)
	if version != len(migrations) {
		t.Fatalf("migrated to %d, want %d", version, len(migrations))
	}
}
