// Package github reads what the control plane needs from the GitHub REST API.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Approval is one review of an environment deployment in a workflow run.
type Approval struct {
	Login        string
	State        string // "approved" or "rejected"
	Environments []string
}

type Client struct {
	APIURL string
	// Token is optional for public repositories. Private ones need a token that can read
	// Actions on the repository.
	Token string
	HTTP  *http.Client
}

// RunApprovals returns the environment reviews recorded on a workflow run.
func (client Client) RunApprovals(ctx context.Context, repository, runID string) ([]Approval, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%s/approvals",
		strings.TrimSuffix(client.APIURL, "/"), repository, url.PathEscape(runID))
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if client.Token != "" {
		request.Header.Set("Authorization", "Bearer "+client.Token)
	}
	httpClient := client.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("run approvals: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("run approvals: HTTP %d", response.StatusCode)
	}
	var body []struct {
		State        string                  `json:"state"`
		User         struct{ Login string }  `json:"user"`
		Environments []struct{ Name string } `json:"environments"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("run approvals: %w", err)
	}
	approvals := make([]Approval, 0, len(body))
	for _, review := range body {
		approval := Approval{Login: review.User.Login, State: review.State}
		for _, environment := range review.Environments {
			approval.Environments = append(approval.Environments, environment.Name)
		}
		approvals = append(approvals, approval)
	}
	return approvals, nil
}
