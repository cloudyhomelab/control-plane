package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cloudyhome/controlplane/internal/policy"
)

type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
	TimedOut  Status = "timed_out"
)

func (s Status) Done() bool {
	return s != Queued && s != Running
}

type Job struct {
	ID             string            `json:"id"`
	Action         string            `json:"action"`
	Status         Status            `json:"status"`
	Params         map[string]string `json:"params"`
	Ref            string            `json:"ref"`
	CommitSHA      string            `json:"commit_sha"`
	PlanJobID      string            `json:"plan_job_id,omitempty"`
	LockKey        string            `json:"lock_key,omitempty"`
	Repository     string            `json:"repository"`
	Caller         policy.Claims     `json:"-"`
	IdempotencyKey string            `json:"-"`
	CreatedAt      time.Time         `json:"created_at"`
	StartedAt      *time.Time        `json:"started_at,omitempty"`
	FinishedAt     *time.Time        `json:"finished_at,omitempty"`
	ExitCode       *int              `json:"exit_code,omitempty"`
	Error          string            `json:"error,omitempty"`
}

type AuditEntry struct {
	Subject, Repository, RunID, Action, JobID, Decision, Reason string
}

var ErrNotFound = errors.New("job not found")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	action TEXT NOT NULL,
	status TEXT NOT NULL,
	params_json TEXT NOT NULL,
	ref TEXT NOT NULL,
	commit_sha TEXT NOT NULL,
	plan_job_id TEXT NOT NULL DEFAULT '',
	lock_key TEXT NOT NULL DEFAULT '',
	repository TEXT NOT NULL,
	caller_json TEXT NOT NULL,
	idempotency_key TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	exit_code INTEGER,
	error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotency ON jobs(repository, idempotency_key) WHERE idempotency_key != '';
CREATE INDEX IF NOT EXISTS jobs_plan ON jobs(plan_job_id) WHERE plan_job_id != '';
CREATE TABLE IF NOT EXISTS audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	subject TEXT NOT NULL,
	repository TEXT NOT NULL,
	run_id TEXT NOT NULL,
	action TEXT NOT NULL,
	job_id TEXT NOT NULL,
	decision TEXT NOT NULL,
	reason TEXT NOT NULL
);`

func OpenStore(file string) (*Store, error) {
	db, err := sql.Open("sqlite", file+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One writer avoids SQLITE_BUSY; the load here is tiny.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) Create(ctx context.Context, j *Job) error {
	p, _ := json.Marshal(j.Params)
	c, _ := json.Marshal(j.Caller)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs
		(id, action, status, params_json, ref, commit_sha, plan_job_id, lock_key, repository, caller_json, idempotency_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Action, j.Status, string(p), j.Ref, j.CommitSHA, j.PlanJobID, j.LockKey, j.Repository, string(c),
		j.IdempotencyKey, ts(j.CreatedAt))
	return err
}

const cols = `id, action, status, params_json, ref, commit_sha, plan_job_id, lock_key, repository, caller_json,
	idempotency_key, created_at, started_at, finished_at, exit_code, error`

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	return s.one(ctx, `SELECT `+cols+` FROM jobs WHERE id = ?`, id)
}

func (s *Store) ByIdempotencyKey(ctx context.Context, repository, key string) (*Job, error) {
	return s.one(ctx, `SELECT `+cols+` FROM jobs WHERE repository = ? AND idempotency_key = ?`, repository, key)
}

// PlanConsumed reports whether an apply using this plan is queued, running or done successfully.
func (s *Store) PlanConsumed(ctx context.Context, planID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE plan_job_id = ? AND status IN (?, ?, ?)`,
		planID, Queued, Running, Succeeded).Scan(&n)
	return n > 0, err
}

func (s *Store) SetRunning(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, started_at = ? WHERE id = ?`, Running, ts(at), id)
	return err
}

func (s *Store) Finish(ctx context.Context, id string, st Status, exitCode *int, errMsg string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, exit_code = ?, error = ?, finished_at = ? WHERE id = ?`,
		st, exitCode, errMsg, ts(at), id)
	return err
}

// FailInterrupted marks jobs left queued or running by a previous process as failed.
func (s *Store) FailInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, error = 'interrupted by server restart', finished_at = ?
		WHERE status IN (?, ?)`, Failed, ts(time.Now()), Queued, Running)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (ts, subject, repository, run_id, action, job_id, decision, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, ts(time.Now()), e.Subject, e.Repository, e.RunID, e.Action, e.JobID, e.Decision, e.Reason)
	return err
}

func (s *Store) one(ctx context.Context, q string, args ...any) (*Job, error) {
	var (
		j                      Job
		params, caller, create string
		started, finished      sql.NullString
		exit                   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&j.ID, &j.Action, &j.Status, &params, &j.Ref, &j.CommitSHA,
		&j.PlanJobID, &j.LockKey, &j.Repository, &caller, &j.IdempotencyKey, &create, &started, &finished, &exit, &j.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(params), &j.Params)
	json.Unmarshal([]byte(caller), &j.Caller)
	j.CreatedAt = parseTS(create)
	if started.Valid {
		t := parseTS(started.String)
		j.StartedAt = &t
	}
	if finished.Valid {
		t := parseTS(finished.String)
		j.FinishedAt = &t
	}
	if exit.Valid {
		n := int(exit.Int64)
		j.ExitCode = &n
	}
	return &j, nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
