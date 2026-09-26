// Package api exposes the action catalog and jobs over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cloudyhome/controlplane/internal/auth"
	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/jobs"
	"github.com/cloudyhome/controlplane/internal/logs"
	"github.com/cloudyhome/controlplane/internal/params"
	"github.com/cloudyhome/controlplane/internal/policy"
	"github.com/cloudyhome/controlplane/internal/source"
	"github.com/cloudyhome/controlplane/internal/tools"
)

const maxLogChunk = 1 << 20

type Server struct {
	cfg      *config.Config
	store    *jobs.Store
	mgr      *jobs.Manager
	src      *source.Source
	verifier auth.Verifier
}

func New(cfg *config.Config, store *jobs.Store, mgr *jobs.Manager, src *source.Source, v auth.Verifier) *Server {
	return &Server{cfg: cfg, store: store, mgr: mgr, src: src, verifier: v}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/actions", s.authed(s.listActions))
	mux.HandleFunc("POST /v1/actions/{name}/jobs", s.authed(s.submit))
	mux.HandleFunc("GET /v1/jobs/{id}", s.authed(s.withJob(s.getJob)))
	mux.HandleFunc("GET /v1/jobs/{id}/logs", s.authed(s.withJob(s.getLogs)))
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/plan.json", s.authed(s.withJob(s.getPlanJSON)))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.authed(s.withJob(s.cancel)))
	return logRequests(mux)
}

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		slog.Error("internal error", "err", err)
		ae = errf(http.StatusInternalServerError, "internal", "internal error")
	}
	writeJSON(w, ae.status, map[string]string{"error": ae.msg, "code": ae.code})
}

type handler func(w http.ResponseWriter, r *http.Request, c policy.Claims) error

func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.BearerToken(r)
		if err != nil {
			writeErr(w, errf(http.StatusUnauthorized, "unauthenticated", "%v", err))
			return
		}
		claims, err := s.verifier.Verify(r.Context(), tok)
		if err != nil {
			slog.Warn("token rejected", "err", err)
			writeErr(w, errf(http.StatusUnauthorized, "unauthenticated", "invalid token"))
			return
		}
		if err := h(w, r, claims); err != nil {
			writeErr(w, err)
		}
	}
}

type jobHandler func(w http.ResponseWriter, r *http.Request, c policy.Claims, j *jobs.Job) error

// withJob loads the job and hides it from callers outside its repository (unless admin).
func (s *Server) withJob(h jobHandler) handler {
	return func(w http.ResponseWriter, r *http.Request, c policy.Claims) error {
		j, err := s.store.Get(r.Context(), r.PathValue("id"))
		if errors.Is(err, jobs.ErrNotFound) || (err == nil && !s.canSee(c, j)) {
			return errf(http.StatusNotFound, "not_found", "job not found")
		}
		if err != nil {
			return err
		}
		return h(w, r, c, j)
	}
}

func (s *Server) canSee(c policy.Claims, j *jobs.Job) bool {
	return j.Repository == c["repository"] || policy.Allowed(s.cfg.Server.Admins, c)
}

type actionInfo struct {
	Name         string                  `json:"name"`
	Description  string                  `json:"description,omitempty"`
	Tool         string                  `json:"tool"`
	Op           string                  `json:"op"`
	AllowedRefs  []string                `json:"allowed_refs"`
	RequiresPlan string                  `json:"requires_plan,omitempty"`
	Params       map[string]*params.Spec `json:"params"`
}

func (s *Server) listActions(w http.ResponseWriter, r *http.Request, c policy.Claims) error {
	out := []actionInfo{}
	for _, name := range sortedActionNames(s.cfg) {
		a := s.cfg.Actions[name]
		if !policy.Allowed(a.Allow, c) {
			continue
		}
		out = append(out, actionInfo{
			Name: name, Description: a.Description, Tool: a.Tool, Op: a.Op,
			AllowedRefs: a.AllowedRefs, RequiresPlan: a.FromPlan, Params: a.Params,
		})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

type submitRequest struct {
	Ref       string            `json:"ref"`
	Params    map[string]string `json:"params"`
	PlanJobID string            `json:"plan_job_id"`
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, c policy.Claims) error {
	name := r.PathValue("name")
	a, ok := s.cfg.Actions[name]
	if !ok {
		return errf(http.StatusNotFound, "not_found", "unknown action %q", name)
	}
	deny := func(err *apiError) error {
		s.audit(r.Context(), c, name, "", "deny", err.msg)
		return err
	}
	if !policy.Allowed(a.Allow, c) {
		return deny(errf(http.StatusForbidden, "forbidden", "caller may not invoke %s", name))
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if j, err := s.store.ByIdempotencyKey(r.Context(), c["repository"], idemKey); err == nil {
			if j.Action != name {
				return errf(http.StatusConflict, "idempotency_conflict", "idempotency key was used for %s", j.Action)
			}
			writeJSON(w, http.StatusOK, j)
			return nil
		} else if !errors.Is(err, jobs.ErrNotFound) {
			return err
		}
	}

	var req submitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid body: %v", err)
	}

	job := &jobs.Job{
		ID: jobs.NewID(), Action: name, Status: jobs.Queued, LockKey: a.Lock,
		Repository: c["repository"], Caller: c, IdempotencyKey: idemKey, CreatedAt: time.Now(),
	}
	if a.FromPlan != "" {
		if err := s.fromPlan(r.Context(), c, a, &req, job); err != nil {
			var ae *apiError
			if errors.As(err, &ae) {
				return deny(ae)
			}
			return err
		}
	} else {
		if req.PlanJobID != "" {
			return errf(http.StatusUnprocessableEntity, "invalid", "%s does not take plan_job_id", name)
		}
		vals, err := a.Params.Validate(req.Params)
		if err != nil {
			return errf(http.StatusUnprocessableEntity, "invalid_params", "%v", err)
		}
		job.Params = vals
		job.Ref = req.Ref
		if a.Repo == "" {
			if job.Ref != "" {
				return errf(http.StatusUnprocessableEntity, "invalid", "%s does not take a ref", name)
			}
		} else if job.Ref == "" {
			job.Ref = s.src.DefaultRef(a.Repo)
		}
	}

	// Commands without a repo have no ref to check.
	if a.Repo != "" && (!source.ValidRef(job.Ref) || !a.AllowedRefs.Match(job.Ref)) {
		return deny(errf(http.StatusUnprocessableEntity, "ref_not_allowed", "ref %q is not allowed for %s", job.Ref, name))
	}
	if a.RefMatchRequired() && c["ref"] != job.Ref {
		return deny(errf(http.StatusForbidden, "ref_mismatch", "token ref %q does not match %q", c["ref"], job.Ref))
	}

	if a.Repo != "" && job.CommitSHA == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		sha, err := s.src.Resolve(ctx, a.Repo, job.Ref)
		if err != nil {
			slog.Warn("resolve ref", "action", name, "ref", job.Ref, "err", err)
			return errf(http.StatusUnprocessableEntity, "ref_unresolved", "could not resolve %s", job.Ref)
		}
		job.CommitSHA = sha
	}

	if err := s.store.Create(r.Context(), job); err != nil {
		// A concurrent request with the same idempotency key won the insert.
		if idemKey != "" {
			if j, err2 := s.store.ByIdempotencyKey(r.Context(), c["repository"], idemKey); err2 == nil {
				writeJSON(w, http.StatusOK, j)
				return nil
			}
		}
		return err
	}
	s.audit(r.Context(), c, name, job.ID, "allow", "")
	s.mgr.Enqueue(job)
	slog.Info("job queued", "job", job.ID, "action", name, "ref", job.Ref, "sha", job.CommitSHA, "repository", job.Repository)
	writeJSON(w, http.StatusAccepted, job)
	return nil
}

// fromPlan fills an apply job from the plan job it applies, after checking the plan is usable.
func (s *Server) fromPlan(ctx context.Context, c policy.Claims, a *config.Action, req *submitRequest, job *jobs.Job) error {
	if req.PlanJobID == "" {
		return errf(http.StatusUnprocessableEntity, "plan_required", "%s requires plan_job_id", a.Name)
	}
	if len(req.Params) > 0 {
		return errf(http.StatusUnprocessableEntity, "invalid_params", "%s takes its params from the plan", a.Name)
	}
	plan, err := s.store.Get(ctx, req.PlanJobID)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && !s.canSee(c, plan)) {
		return errf(http.StatusNotFound, "not_found", "plan job not found")
	}
	if err != nil {
		return err
	}
	switch {
	case plan.Action != a.FromPlan:
		return errf(http.StatusConflict, "plan_mismatch", "job %s is %s, not %s", plan.ID, plan.Action, a.FromPlan)
	case plan.Status != jobs.Succeeded:
		return errf(http.StatusConflict, "plan_not_succeeded", "plan job is %s", plan.Status)
	case plan.FinishedAt == nil || time.Since(*plan.FinishedAt) > s.cfg.Server.PlanMaxAge.Duration:
		return errf(http.StatusConflict, "plan_expired", "plan is older than %s", s.cfg.Server.PlanMaxAge.Duration)
	case req.Ref != "" && req.Ref != plan.Ref:
		return errf(http.StatusConflict, "plan_mismatch", "plan was made from %s, not %s", plan.Ref, req.Ref)
	}
	used, err := s.store.PlanConsumed(ctx, plan.ID)
	if err != nil {
		return err
	}
	if used {
		return errf(http.StatusConflict, "plan_consumed", "plan %s was already applied or is being applied", plan.ID)
	}
	job.PlanJobID, job.Ref, job.CommitSHA, job.Params = plan.ID, plan.Ref, plan.CommitSHA, plan.Params
	return nil
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, c policy.Claims, j *jobs.Job) error {
	writeJSON(w, http.StatusOK, j)
	return nil
}

// getLogs returns raw log bytes from ?offset=. X-Job-Status is read before the log,
// so a done status with an empty body means the caller has the whole log.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, c policy.Claims, j *jobs.Job) error {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if r.URL.Query().Get("offset") == "" {
		offset, err = 0, nil
	}
	if err != nil || offset < 0 {
		return errf(http.StatusBadRequest, "bad_request", "invalid offset")
	}
	b, next, err := logs.Read(s.mgr.LogFile(j.ID), offset, maxLogChunk)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))
	w.Header().Set("X-Job-Status", string(j.Status))
	w.Write(b)
	return nil
}

func (s *Server) getPlanJSON(w http.ResponseWriter, r *http.Request, c policy.Claims, j *jobs.Job) error {
	a, ok := s.cfg.Actions[j.Action]
	if !ok || a.Tool != "terraform" || a.Op != "plan" || j.Status != jobs.Succeeded {
		return errf(http.StatusNotFound, "not_found", "no plan artifact for this job")
	}
	f, err := os.Open(filepath.Join(s.mgr.JobDir(j.ID), tools.PlanJSONFile))
	if err != nil {
		return errf(http.StatusNotFound, "not_found", "no plan artifact for this job")
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json")
	http.ServeContent(w, r, "plan.json", time.Time{}, f)
	return nil
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, c policy.Claims, j *jobs.Job) error {
	if j.Status.Done() {
		return errf(http.StatusConflict, "already_done", "job is %s", j.Status)
	}
	ok := s.mgr.Cancel(j.ID)
	s.audit(r.Context(), c, j.Action, j.ID, "cancel", "")
	if !ok {
		return errf(http.StatusConflict, "not_active", "job is not active")
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": j.ID, "status": "cancelling"})
	return nil
}

func (s *Server) audit(ctx context.Context, c policy.Claims, action, jobID, decision, reason string) {
	err := s.store.Audit(context.WithoutCancel(ctx), jobs.AuditEntry{
		Subject: c["sub"], Repository: c["repository"], RunID: c["run_id"],
		Action: action, JobID: jobID, Decision: decision, Reason: reason,
	})
	if err != nil {
		slog.Error("audit", "err", err)
	}
}
