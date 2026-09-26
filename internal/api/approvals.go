package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/github"
	"github.com/cloudyhome/controlplane/internal/policy"
)

type approvalSource interface {
	RunApprovals(ctx context.Context, repository, runID string) ([]github.Approval, error)
}

// checkApproval requires that one of the action's approvers approved the caller's environment
// in the caller's workflow run. The approval happens in GitHub; this makes sure it came from
// someone the catalog names, whatever reviewers the calling repository configured.
func (server *Server) checkApproval(ctx context.Context, claims policy.Claims, action *config.Action) error {
	environment := claims["environment"]
	if environment == "" {
		return errf(http.StatusForbidden, "approval_required", "%s must be called from a job in a GitHub environment", action.Name)
	}
	approvals, err := server.runs.RunApprovals(ctx, claims["repository"], claims["run_id"])
	if err != nil {
		slog.Warn("read run approvals", "repository", claims["repository"], "run_id", claims["run_id"], "err", err)
		return errf(http.StatusBadGateway, "approval_lookup_failed", "could not read approvals for run %s", claims["run_id"])
	}
	var others []string
	for _, approval := range approvals {
		if approval.State != "approved" || !slices.Contains(approval.Environments, environment) {
			continue
		}
		if action.IsApprover(approval.Login) {
			return nil
		}
		others = append(others, approval.Login)
	}
	if len(others) > 0 {
		return errf(http.StatusForbidden, "not_approved", "environment %s was approved by %s, not by one of: %s",
			environment, strings.Join(others, ", "), strings.Join(action.Approvers, ", "))
	}
	return errf(http.StatusForbidden, "not_approved", "environment %s has no approval from one of: %s",
		environment, strings.Join(action.Approvers, ", "))
}
