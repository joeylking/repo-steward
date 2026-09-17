// Package task persists repo-steward's own state: maintenance tasks,
// manifest promotions, validation runs, and proposals. Phase is never
// stored; it is derived from the promotion journal and proposal rows. Rows
// carry the step or operation that produced them, and acceptance rules
// decide whether a row counts, so completion is never inferred from a row's
// mere existence.
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for missing rows.
var ErrNotFound = errors.New("task: not found")

// Store is the SQLite-backed store.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path
	if path != ":memory:" {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		dsn += "?" + q.Encode()
	} else {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{`
CREATE TABLE tasks (
	run_id TEXT PRIMARY KEY,
	mode TEXT NOT NULL,
	source_path TEXT NOT NULL,
	base_commit TEXT NOT NULL,
	base_tree TEXT NOT NULL,
	base_ref TEXT NOT NULL,
	module_path TEXT NOT NULL DEFAULT '',
	workspace_dir TEXT NOT NULL,
	named_dependency TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT '',
	outcome_json TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL,
	finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE promotions (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES tasks(run_id),
	step_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	target_module TEXT NOT NULL DEFAULT '',
	target_version TEXT NOT NULL DEFAULT '',
	before_mod_sha TEXT NOT NULL,
	before_sum_sha TEXT NOT NULL,
	after_mod_sha TEXT NOT NULL,
	after_sum_sha TEXT NOT NULL,
	staging_dir TEXT NOT NULL,
	status TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX promotions_run ON promotions(run_id, created_at);
CREATE TABLE validation_runs (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES tasks(run_id),
	step_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	tree_hash TEXT NOT NULL,
	config_hash TEXT NOT NULL,
	toolchain_digest TEXT NOT NULL,
	accepted INTEGER NOT NULL DEFAULT 0,
	run_json TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX validation_runs_tree ON validation_runs(run_id, tree_hash);
CREATE TABLE proposals (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES tasks(run_id),
	step_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	tree_hash TEXT NOT NULL,
	base_commit TEXT NOT NULL,
	head_commit TEXT NOT NULL DEFAULT '',
	ref TEXT NOT NULL,
	recipe_json TEXT NOT NULL,
	proposal_json TEXT NOT NULL,
	hash TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX proposals_run ON proposals(run_id, created_at);
`}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS steward_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM steward_migrations`).Scan(&current); err != nil {
		return err
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("task: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO steward_migrations (version, applied_at) VALUES (?, ?)`, i+1, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Task is one maintenance run's fixed facts and final outcome.
type Task struct {
	RunID           string          `json:"run_id"`
	Mode            string          `json:"mode"`
	SourcePath      string          `json:"source_path"`
	BaseCommit      string          `json:"base_commit"`
	BaseTree        string          `json:"base_tree"`
	BaseRef         string          `json:"base_ref"`
	ModulePath      string          `json:"module_path"`
	WorkspaceDir    string          `json:"workspace_dir"`
	NamedDependency string          `json:"named_dependency,omitempty"`
	Outcome         string          `json:"outcome"`
	OutcomeDetail   json.RawMessage `json:"outcome_detail,omitempty"`
	CreatedAt       string          `json:"created_at"`
	FinishedAt      string          `json:"finished_at,omitempty"`
}

// CreateTask inserts a task.
func (s *Store) CreateTask(ctx context.Context, t Task) error {
	if t.CreatedAt == "" {
		t.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (run_id, mode, source_path, base_commit, base_tree, base_ref, module_path, workspace_dir, named_dependency, outcome, outcome_json, created_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.RunID, t.Mode, t.SourcePath, t.BaseCommit, t.BaseTree, t.BaseRef, t.ModulePath, t.WorkspaceDir, t.NamedDependency, t.Outcome, rawOr(t.OutcomeDetail), t.CreatedAt, t.FinishedAt)
	return err
}

// FinishTask records the outcome.
func (s *Store) FinishTask(ctx context.Context, runID, outcome string, detail any) error {
	d, _ := json.Marshal(detail)
	if detail == nil {
		d = []byte("{}")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET outcome=?, outcome_json=?, finished_at=? WHERE run_id=?`, outcome, string(d), now(), runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// UpdateTaskModule records the module path once profiled.
func (s *Store) UpdateTaskModule(ctx context.Context, runID, modulePath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET module_path=? WHERE run_id=?`, modulePath, runID)
	return err
}

// GetTask loads a task.
func (s *Store) GetTask(ctx context.Context, runID string) (Task, error) {
	var t Task
	var detail string
	err := s.db.QueryRowContext(ctx, `SELECT run_id, mode, source_path, base_commit, base_tree, base_ref, module_path, workspace_dir, named_dependency, outcome, outcome_json, created_at, finished_at FROM tasks WHERE run_id=?`, runID).
		Scan(&t.RunID, &t.Mode, &t.SourcePath, &t.BaseCommit, &t.BaseTree, &t.BaseRef, &t.ModulePath, &t.WorkspaceDir, &t.NamedDependency, &t.Outcome, &detail, &t.CreatedAt, &t.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.OutcomeDetail = json.RawMessage(detail)
	return t, err
}

// PromotionStatus is the journal state of a manifest promotion.
type PromotionStatus string

const (
	PromotionStaged    PromotionStatus = "staged"
	PromotionPromoting PromotionStatus = "promoting"
	PromotionPromoted  PromotionStatus = "promoted"
	PromotionAborted   PromotionStatus = "aborted"
	PromotionConflict  PromotionStatus = "conflict"
)

// Terminal reports whether the promotion needs no recovery.
func (p PromotionStatus) Terminal() bool {
	return p == PromotionPromoted || p == PromotionAborted || p == PromotionConflict
}

// Promotion is one journaled manifest change.
type Promotion struct {
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	StepID        string          `json:"step_id,omitempty"`
	Kind          string          `json:"kind"` // upgrade normalize
	TargetModule  string          `json:"target_module,omitempty"`
	TargetVersion string          `json:"target_version,omitempty"`
	BeforeModSHA  string          `json:"before_mod_sha"`
	BeforeSumSHA  string          `json:"before_sum_sha"`
	AfterModSHA   string          `json:"after_mod_sha"`
	AfterSumSHA   string          `json:"after_sum_sha"`
	StagingDir    string          `json:"staging_dir"`
	Status        PromotionStatus `json:"status"`
	Detail        string          `json:"detail,omitempty"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
}

// InsertPromotion journals a staged promotion.
func (s *Store) InsertPromotion(ctx context.Context, p Promotion) error {
	ts := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO promotions (id, run_id, step_id, kind, target_module, target_version, before_mod_sha, before_sum_sha, after_mod_sha, after_sum_sha, staging_dir, status, detail, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.RunID, p.StepID, p.Kind, p.TargetModule, p.TargetVersion, p.BeforeModSHA, p.BeforeSumSHA, p.AfterModSHA, p.AfterSumSHA, p.StagingDir, p.Status, p.Detail, ts, ts)
	return err
}

// SetPromotionStatus advances the journal.
func (s *Store) SetPromotionStatus(ctx context.Context, id string, status PromotionStatus, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE promotions SET status=?, detail=?, updated_at=? WHERE id=?`, status, detail, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// ListPromotions returns a run's promotions in creation order.
func (s *Store) ListPromotions(ctx context.Context, runID string) ([]Promotion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, kind, target_module, target_version, before_mod_sha, before_sum_sha, after_mod_sha, after_sum_sha, staging_dir, status, detail, created_at, updated_at FROM promotions WHERE run_id=? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Promotion
	for rows.Next() {
		var p Promotion
		if err := rows.Scan(&p.ID, &p.RunID, &p.StepID, &p.Kind, &p.TargetModule, &p.TargetVersion, &p.BeforeModSHA, &p.BeforeSumSHA, &p.AfterModSHA, &p.AfterSumSHA, &p.StagingDir, &p.Status, &p.Detail, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ValidationRecord binds a validation run to its evidence identity.
type ValidationRecord struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	StepID          string `json:"step_id,omitempty"`
	Kind            string `json:"kind"`
	TreeHash        string `json:"tree_hash"`
	ConfigHash      string `json:"config_hash"`
	ToolchainDigest string `json:"toolchain_digest"`
	// Accepted is set by the pipeline once the producing step completed.
	Accepted  bool            `json:"accepted"`
	Run       json.RawMessage `json:"run"`
	CreatedAt string          `json:"created_at"`
}

// InsertValidation stores a validation run.
func (s *Store) InsertValidation(ctx context.Context, v ValidationRecord) error {
	if v.CreatedAt == "" {
		v.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO validation_runs (id, run_id, step_id, kind, tree_hash, config_hash, toolchain_digest, accepted, run_json, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.RunID, v.StepID, v.Kind, v.TreeHash, v.ConfigHash, v.ToolchainDigest, boolInt(v.Accepted), rawOr(v.Run), v.CreatedAt)
	return err
}

// AcceptValidation marks a validation run admissible.
func (s *Store) AcceptValidation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE validation_runs SET accepted=1 WHERE id=?`, id)
	return err
}

// ListValidations returns a run's validation records, newest first.
func (s *Store) ListValidations(ctx context.Context, runID string) ([]ValidationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, kind, tree_hash, config_hash, toolchain_digest, accepted, run_json, created_at FROM validation_runs WHERE run_id=? ORDER BY created_at DESC, id DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ValidationRecord
	for rows.Next() {
		var v ValidationRecord
		var accepted int
		var run string
		if err := rows.Scan(&v.ID, &v.RunID, &v.StepID, &v.Kind, &v.TreeHash, &v.ConfigHash, &v.ToolchainDigest, &accepted, &run, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Accepted = accepted == 1
		v.Run = json.RawMessage(run)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ProposalStatus is the lifecycle of a frozen proposal.
type ProposalStatus string

const (
	ProposalPreparing   ProposalStatus = "preparing"
	ProposalFrozen      ProposalStatus = "frozen"
	ProposalSuperseded  ProposalStatus = "superseded"
	ProposalPublished   ProposalStatus = "published"
	ProposalInvalidated ProposalStatus = "invalidated"
)

// ProposalRecord is the persisted proposal with its commit recipe.
type ProposalRecord struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	StepID     string          `json:"step_id,omitempty"`
	Status     ProposalStatus  `json:"status"`
	TreeHash   string          `json:"tree_hash"`
	BaseCommit string          `json:"base_commit"`
	HeadCommit string          `json:"head_commit,omitempty"`
	Ref        string          `json:"ref"`
	Recipe     json.RawMessage `json:"recipe"`
	Proposal   json.RawMessage `json:"proposal"`
	Hash       string          `json:"hash,omitempty"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

// InsertProposal journals a proposal in the preparing state.
func (s *Store) InsertProposal(ctx context.Context, p ProposalRecord) error {
	ts := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO proposals (id, run_id, step_id, status, tree_hash, base_commit, head_commit, ref, recipe_json, proposal_json, hash, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.RunID, p.StepID, p.Status, p.TreeHash, p.BaseCommit, p.HeadCommit, p.Ref, rawOr(p.Recipe), rawOr(p.Proposal), p.Hash, ts, ts)
	return err
}

// FreezeProposal records the head commit, the frozen body, and its hash.
func (s *Store) FreezeProposal(ctx context.Context, id, headCommit string, proposal json.RawMessage, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE proposals SET status=?, head_commit=?, proposal_json=?, hash=?, updated_at=? WHERE id=?`, ProposalFrozen, headCommit, string(proposal), hash, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// SetProposalStatus changes a proposal's status.
func (s *Store) SetProposalStatus(ctx context.Context, id string, status ProposalStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE proposals SET status=?, updated_at=? WHERE id=?`, status, now(), id)
	return err
}

// ListProposals returns a run's proposals in creation order.
func (s *Store) ListProposals(ctx context.Context, runID string) ([]ProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, status, tree_hash, base_commit, head_commit, ref, recipe_json, proposal_json, hash, created_at, updated_at FROM proposals WHERE run_id=? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProposalRecord
	for rows.Next() {
		var p ProposalRecord
		var recipe, prop string
		if err := rows.Scan(&p.ID, &p.RunID, &p.StepID, &p.Status, &p.TreeHash, &p.BaseCommit, &p.HeadCommit, &p.Ref, &recipe, &prop, &p.Hash, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Recipe, p.Proposal = json.RawMessage(recipe), json.RawMessage(prop)
		out = append(out, p)
	}
	return out, rows.Err()
}

func rawOr(r json.RawMessage) string {
	if len(r) == 0 {
		return "{}"
	}
	return string(r)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
