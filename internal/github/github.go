// Package github is a minimal client for the handful of GitHub REST calls
// publication needs: repository and branch lookup, commit existence, pull
// request creation, and pull request listing. It is small on purpose so
// every request the tool can make is visible here, and every test runs
// against the fake server in the fake subpackage.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one API base with one token.
type Client struct {
	Base  string // e.g. https://api.github.com
	Token string
	HTTP  *http.Client
}

// New returns a client. An empty token makes unauthenticated requests,
// which suffice for reading public repositories.
func New(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("github: not found")

// StatusError carries a non-2xx status.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return &StatusError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Repository is the subset of repository metadata used.
type Repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// GetRepository looks up owner/repo.
func (c *Client) GetRepository(ctx context.Context, owner, repo string) (Repository, error) {
	var r Repository
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), nil, &r)
	return r, err
}

// BranchHead returns the commit sha at the head of branch.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/branches/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch)), nil, &b)
	return b.Commit.SHA, err
}

// RefHead returns the sha a branch ref points at, or ErrNotFound.
func (c *Client) RefHead(ctx context.Context, owner, repo, branch string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch)), nil, &r)
	return r.Object.SHA, err
}

// CommitExists reports whether sha is reachable in the repository.
func (c *Client) CommitExists(ctx context.Context, owner, repo, sha string) (bool, error) {
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha)), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	// GitHub answers 422 for a malformed or unknown sha.
	var se *StatusError
	if errors.As(err, &se) && se.Status == 422 {
		return false, nil
	}
	return err == nil, err
}

// PullRequest is the subset of pull request fields used.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"` // open or closed
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergedAt *string `json:"merged_at"`
}

// CreatePullRequest opens a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo, title, body, head, base string) (PullRequest, error) {
	var pr PullRequest
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo)),
		map[string]any{"title": title, "body": body, "head": head, "base": base}, &pr)
	return pr, err
}

// ListPullRequestsByHead lists pull requests in every state whose head is
// owner:branch.
func (c *Client) ListPullRequestsByHead(ctx context.Context, owner, repo, headOwner, branch string) ([]PullRequest, error) {
	var prs []PullRequest
	q := url.Values{"state": {"all"}, "head": {headOwner + ":" + branch}, "per_page": {"100"}}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?%s", url.PathEscape(owner), url.PathEscape(repo), q.Encode()), nil, &prs)
	for i := range prs {
		if prs[i].MergedAt != nil {
			prs[i].Merged = true
		}
	}
	return prs, err
}
