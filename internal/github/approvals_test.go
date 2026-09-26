package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunApprovals(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath, gotAuth = request.URL.Path, request.Header.Get("Authorization")
		writer.Write([]byte(`[
			{"state": "approved", "comment": "", "user": {"login": "binarycodes"},
			 "environments": [{"id": 1, "name": "production"}]}
		]`))
	}))
	defer server.Close()

	approvals, err := Client{APIURL: server.URL, Token: "tok"}.RunApprovals(context.Background(), "cloudyhome/homelab", "42")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/cloudyhome/homelab/actions/runs/42/approvals" || gotAuth != "Bearer tok" {
		t.Errorf("request = %s %q", gotPath, gotAuth)
	}
	if len(approvals) != 1 || approvals[0].Login != "binarycodes" || approvals[0].State != "approved" || approvals[0].Environments[0] != "production" {
		t.Errorf("approvals = %+v", approvals)
	}
}

func TestRunApprovalsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	if _, err := (Client{APIURL: server.URL}).RunApprovals(context.Background(), "o/r", "1"); err == nil {
		t.Error("expected error for HTTP 404")
	}
}
