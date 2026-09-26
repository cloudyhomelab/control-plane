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

func New(cfg *config.Config, store *jobs.Store, mgr *jobs.Manager, src *source.Source, verifier auth.Verifier) *Server {
	return &Server{cfg: cfg, store: store, mgr: mgr, src: src, verifier: verifier}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, request *http.Request) { writer.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/actions", server.authed(server.listActions))
	mux.HandleFunc("POST /v1/actions/{name}/jobs", server.authed(server.submit))
	mux.HandleFunc("GET /v1/jobs/{id}", server.authed(server.withJob(server.getJob)))
	mux.HandleFunc("GET /v1/jobs/{id}/logs", server.authed(server.withJob(server.getLogs)))
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/plan.json", server.authed(server.withJob(server.getPlanJSON)))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", server.authed(server.withJob(server.cancel)))
	return logRequests(mux)
}

type apiError struct {
	status int
	code   string
	msg    string
}

func (apiErr *apiError) Error() string { return apiErr.msg }

func errf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeErr(writer http.ResponseWriter, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		slog.Error("internal error", "err", err)
		apiErr = errf(http.StatusInternalServerError, "internal", "internal error")
	}
	writeJSON(writer, apiErr.status, map[string]string{"error": apiErr.msg, "code": apiErr.code})
}

type handler func(writer http.ResponseWriter, request *http.Request, claims policy.Claims) error

func (server *Server) authed(next handler) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		tok, err := auth.BearerToken(request)
		if err != nil {
			writeErr(writer, errf(http.StatusUnauthorized, "unauthenticated", "%v", err))
			return
		}
		claims, err := server.verifier.Verify(request.Context(), tok)
		if err != nil {
			slog.Warn("token rejected", "err", err)
			writeErr(writer, errf(http.StatusUnauthorized, "unauthenticated", "invalid token"))
			return
		}
		if err := next(writer, request, claims); err != nil {
			writeErr(writer, err)
		}
	}
}

type jobHandler func(writer http.ResponseWriter, request *http.Request, claims policy.Claims, job *jobs.Job) error

// withJob loads the job and hides it from callers outside its repository (unless admin).
func (server *Server) withJob(next jobHandler) handler {
	return func(writer http.ResponseWriter, request *http.Request, claims policy.Claims) error {
		job, err := server.store.Get(request.Context(), request.PathValue("id"))
		if errors.Is(err, jobs.ErrNotFound) || (err == nil && !server.canSee(claims, job)) {
			return errf(http.StatusNotFound, "not_found", "job not found")
		}
		if err != nil {
			return err
		}
		return next(writer, request, claims, job)
	}
}

func (server *Server) canSee(claims policy.Claims, job *jobs.Job) bool {
	return job.Repository == claims["repository"] || policy.Allowed(server.cfg.Server.Admins, claims)
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

func (server *Server) listActions(writer http.ResponseWriter, request *http.Request, claims policy.Claims) error {
	out := []actionInfo{}
	for _, name := range sortedActionNames(server.cfg) {
		action := server.cfg.Actions[name]
		if !policy.Allowed(action.Allow, claims) {
			continue
		}
		out = append(out, actionInfo{
			Name: name, Description: action.Description, Tool: action.Tool, Op: action.Op,
			AllowedRefs: action.AllowedRefs, RequiresPlan: action.FromPlan, Params: action.Params,
		})
	}
	writeJSON(writer, http.StatusOK, out)
	return nil
}

type submitRequest struct {
	Ref       string            `json:"ref"`
	Params    map[string]string `json:"params"`
	PlanJobID string            `json:"plan_job_id"`
}

func (server *Server) submit(writer http.ResponseWriter, request *http.Request, claims policy.Claims) error {
	name := request.PathValue("name")
	action, ok := server.cfg.Actions[name]
	if !ok {
		return errf(http.StatusNotFound, "not_found", "unknown action %q", name)
	}
	deny := func(err *apiError) error {
		server.audit(request.Context(), claims, name, "", "deny", err.msg)
		return err
	}
	if !policy.Allowed(action.Allow, claims) {
		return deny(errf(http.StatusForbidden, "forbidden", "caller may not invoke %s", name))
	}

	idemKey := request.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if existing, err := server.store.ByIdempotencyKey(request.Context(), claims["repository"], idemKey); err == nil {
			if existing.Action != name {
				return errf(http.StatusConflict, "idempotency_conflict", "idempotency key was used for %s", existing.Action)
			}
			writeJSON(writer, http.StatusOK, existing)
			return nil
		} else if !errors.Is(err, jobs.ErrNotFound) {
			return err
		}
	}

	var req submitRequest
	dec := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid body: %v", err)
	}

	job := &jobs.Job{
		ID: jobs.NewID(), Action: name, Status: jobs.Queued, LockKey: action.Lock,
		Repository: claims["repository"], Caller: claims, IdempotencyKey: idemKey, CreatedAt: time.Now(),
	}
	if action.FromPlan != "" {
		if err := server.fromPlan(request.Context(), claims, action, &req, job); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) {
				return deny(apiErr)
			}
			return err
		}
	} else {
		if req.PlanJobID != "" {
			return errf(http.StatusUnprocessableEntity, "invalid", "%s does not take plan_job_id", name)
		}
		vals, err := action.Params.Validate(req.Params)
		if err != nil {
			return errf(http.StatusUnprocessableEntity, "invalid_params", "%v", err)
		}
		job.Params = vals
		job.Ref = req.Ref
		if action.Repo == "" {
			if job.Ref != "" {
				return errf(http.StatusUnprocessableEntity, "invalid", "%s does not take a ref", name)
			}
		} else if job.Ref == "" {
			job.Ref = server.src.DefaultRef(action.Repo)
		}
	}

	// Commands without a repo have no ref to check.
	if action.Repo != "" && (!source.ValidRef(job.Ref) || !action.AllowedRefs.Match(job.Ref)) {
		return deny(errf(http.StatusUnprocessableEntity, "ref_not_allowed", "ref %q is not allowed for %s", job.Ref, name))
	}
	if action.RefMatchRequired() && claims["ref"] != job.Ref {
		return deny(errf(http.StatusForbidden, "ref_mismatch", "token ref %q does not match %q", claims["ref"], job.Ref))
	}

	if action.Repo != "" && job.CommitSHA == "" {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
		defer cancel()
		sha, err := server.src.Resolve(ctx, action.Repo, job.Ref)
		if err != nil {
			slog.Warn("resolve ref", "action", name, "ref", job.Ref, "err", err)
			return errf(http.StatusUnprocessableEntity, "ref_unresolved", "could not resolve %s", job.Ref)
		}
		job.CommitSHA = sha
	}

	if err := server.store.Create(request.Context(), job); err != nil {
		// A concurrent request with the same idempotency key won the insert.
		if idemKey != "" {
			if existing, err2 := server.store.ByIdempotencyKey(request.Context(), claims["repository"], idemKey); err2 == nil {
				writeJSON(writer, http.StatusOK, existing)
				return nil
			}
		}
		return err
	}
	server.audit(request.Context(), claims, name, job.ID, "allow", "")
	server.mgr.Enqueue(job)
	slog.Info("job queued", "job", job.ID, "action", name, "ref", job.Ref, "sha", job.CommitSHA, "repository", job.Repository)
	writeJSON(writer, http.StatusAccepted, job)
	return nil
}

// fromPlan fills an apply job from the plan job it applies, after checking the plan is usable.
func (server *Server) fromPlan(ctx context.Context, claims policy.Claims, action *config.Action, req *submitRequest, job *jobs.Job) error {
	if req.PlanJobID == "" {
		return errf(http.StatusUnprocessableEntity, "plan_required", "%s requires plan_job_id", action.Name)
	}
	if len(req.Params) > 0 {
		return errf(http.StatusUnprocessableEntity, "invalid_params", "%s takes its params from the plan", action.Name)
	}
	plan, err := server.store.Get(ctx, req.PlanJobID)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && !server.canSee(claims, plan)) {
		return errf(http.StatusNotFound, "not_found", "plan job not found")
	}
	if err != nil {
		return err
	}
	switch {
	case plan.Action != action.FromPlan:
		return errf(http.StatusConflict, "plan_mismatch", "job %s is %s, not %s", plan.ID, plan.Action, action.FromPlan)
	case plan.Status != jobs.Succeeded:
		return errf(http.StatusConflict, "plan_not_succeeded", "plan job is %s", plan.Status)
	case plan.FinishedAt == nil || time.Since(*plan.FinishedAt) > server.cfg.Server.PlanMaxAge.Duration:
		return errf(http.StatusConflict, "plan_expired", "plan is older than %s", server.cfg.Server.PlanMaxAge.Duration)
	case req.Ref != "" && req.Ref != plan.Ref:
		return errf(http.StatusConflict, "plan_mismatch", "plan was made from %s, not %s", plan.Ref, req.Ref)
	}
	used, err := server.store.PlanConsumed(ctx, plan.ID)
	if err != nil {
		return err
	}
	if used {
		return errf(http.StatusConflict, "plan_consumed", "plan %s was already applied or is being applied", plan.ID)
	}
	job.PlanJobID, job.Ref, job.CommitSHA, job.Params = plan.ID, plan.Ref, plan.CommitSHA, plan.Params
	return nil
}

func (server *Server) getJob(writer http.ResponseWriter, request *http.Request, claims policy.Claims, job *jobs.Job) error {
	writeJSON(writer, http.StatusOK, job)
	return nil
}

// getLogs returns raw log bytes from ?offset=. X-Job-Status is read before the log,
// so a done status with an empty body means the caller has the whole log.
func (server *Server) getLogs(writer http.ResponseWriter, request *http.Request, claims policy.Claims, job *jobs.Job) error {
	offset, err := strconv.ParseInt(request.URL.Query().Get("offset"), 10, 64)
	if request.URL.Query().Get("offset") == "" {
		offset, err = 0, nil
	}
	if err != nil || offset < 0 {
		return errf(http.StatusBadRequest, "bad_request", "invalid offset")
	}
	chunk, next, err := logs.Read(server.mgr.LogFile(job.ID), offset, maxLogChunk)
	if err != nil {
		return err
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))
	writer.Header().Set("X-Job-Status", string(job.Status))
	writer.Write(chunk)
	return nil
}

func (server *Server) getPlanJSON(writer http.ResponseWriter, request *http.Request, claims policy.Claims, job *jobs.Job) error {
	action, ok := server.cfg.Actions[job.Action]
	if !ok || action.Tool != "terraform" || action.Op != "plan" || job.Status != jobs.Succeeded {
		return errf(http.StatusNotFound, "not_found", "no plan artifact for this job")
	}
	planFile, err := os.Open(filepath.Join(server.mgr.JobDir(job.ID), tools.PlanJSONFile))
	if err != nil {
		return errf(http.StatusNotFound, "not_found", "no plan artifact for this job")
	}
	defer planFile.Close()
	writer.Header().Set("Content-Type", "application/json")
	http.ServeContent(writer, request, "plan.json", time.Time{}, planFile)
	return nil
}

func (server *Server) cancel(writer http.ResponseWriter, request *http.Request, claims policy.Claims, job *jobs.Job) error {
	if job.Status.Done() {
		return errf(http.StatusConflict, "already_done", "job is %s", job.Status)
	}
	ok := server.mgr.Cancel(job.ID)
	server.audit(request.Context(), claims, job.Action, job.ID, "cancel", "")
	if !ok {
		return errf(http.StatusConflict, "not_active", "job is not active")
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"id": job.ID, "status": "cancelling"})
	return nil
}

func (server *Server) audit(ctx context.Context, claims policy.Claims, action, jobID, decision, reason string) {
	err := server.store.Audit(context.WithoutCancel(ctx), jobs.AuditEntry{
		Subject: claims["sub"], Repository: claims["repository"], RunID: claims["run_id"],
		Action: action, JobID: jobID, Decision: decision, Reason: reason,
	})
	if err != nil {
		slog.Error("audit", "err", err)
	}
}
