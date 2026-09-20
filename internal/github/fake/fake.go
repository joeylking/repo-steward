// Package fake is an in-memory GitHub API server for tests. It models
// repositories, branch heads, commits, and pull requests, and offers
// failure injection so recovery paths can be exercised: a request can be
// made to fail, or to succeed while its response is dropped.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
)

// PR is a stored pull request.
type PR struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
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

// Repo is a stored repository.
type Repo struct {
	Owner, Name   string
	DefaultBranch string
	Branches      map[string]string // branch -> head sha
	Commits       map[string]bool
	PRs           []*PR
	nextPR        int
	// GitDir, when set, is a repository whose refs are consulted for
	// branches absent from Branches, so a git push to it becomes visible
	// through the API the way it is on the real service.
	GitDir string
}

// branchHead resolves a branch from the table or the git directory.
func (r *Repo) branchHead(branch string) (string, bool) {
	if sha, ok := r.Branches[branch]; ok {
		return sha, true
	}
	if r.GitDir != "" {
		out, err := exec.Command("git", "--git-dir="+r.GitDir, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
		if err == nil {
			return strings.TrimSpace(string(out)), true
		}
	}
	return "", false
}

// Server is the fake.
type Server struct {
	mu    sync.Mutex
	repos map[string]*Repo
	srv   *httptest.Server
	// Requests records every request path in order.
	Requests []string
	// FailNextCreatePR makes the next PR creation return 500 without
	// creating anything. DropNextCreatePRResponse creates the PR and then
	// closes the connection without a response.
	FailNextCreatePR         bool
	DropNextCreatePRResponse bool
	FailAllWrites            bool
	RequireToken             string
}

// New starts a fake server.
func New() *Server {
	s := &Server{repos: map[string]*Repo{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the API base.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// AddRepo registers a repository with a branch at sha.
func (s *Server) AddRepo(owner, name, defaultBranch, sha string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Repo{Owner: owner, Name: name, DefaultBranch: defaultBranch, Branches: map[string]string{defaultBranch: sha}, Commits: map[string]bool{sha: true}, nextPR: 1}
	s.repos[owner+"/"+name] = r
	return r
}

// Repo returns a registered repository.
func (s *Server) Repo(owner, name string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[owner+"/"+name]
}

// SetBranch moves a branch head and records the commit.
func (s *Server) SetBranch(owner, name, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[owner+"/"+name]
	r.Branches[branch] = sha
	r.Commits[sha] = true
}

// AddPR inserts a pull request directly, for reconciliation tests.
func (s *Server) AddPR(owner, name string, pr PR) *PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[owner+"/"+name]
	pr.Number = r.nextPR
	r.nextPR++
	pr.HTMLURL = fmt.Sprintf("https://github.example/%s/%s/pull/%d", owner, name, pr.Number)
	p := pr
	r.PRs = append(r.PRs, &p)
	return &p
}

func (s *Server) handle(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	s.Requests = append(s.Requests, req.Method+" "+req.URL.Path)
	if s.RequireToken != "" && req.Header.Get("Authorization") != "Bearer "+s.RequireToken {
		s.mu.Unlock()
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	s.mu.Unlock()
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" {
		http.Error(w, `{"message":"Not Found"}`, 404)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[parts[1]+"/"+parts[2]]
	if r == nil {
		http.Error(w, `{"message":"Not Found"}`, 404)
		return
	}
	rest := parts[3:]
	switch {
	case len(rest) == 0 && req.Method == http.MethodGet:
		writeJSON(w, map[string]any{"full_name": r.Owner + "/" + r.Name, "default_branch": r.DefaultBranch, "permissions": map[string]bool{"push": true}})
	case len(rest) >= 2 && rest[0] == "branches":
		name := strings.Join(rest[1:], "/")
		sha, ok := r.branchHead(name)
		if !ok {
			http.Error(w, `{"message":"Branch not found"}`, 404)
			return
		}
		writeJSON(w, map[string]any{"name": name, "commit": map[string]string{"sha": sha}})
	case len(rest) >= 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads":
		name := strings.Join(rest[3:], "/")
		sha, ok := r.branchHead(name)
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		writeJSON(w, map[string]any{"ref": "refs/heads/" + name, "object": map[string]string{"sha": sha, "type": "commit"}})
	case len(rest) == 2 && rest[0] == "commits":
		if !r.Commits[rest[1]] {
			http.Error(w, `{"message":"No commit found"}`, 422)
			return
		}
		writeJSON(w, map[string]any{"sha": rest[1]})
	case len(rest) == 1 && rest[0] == "pulls" && req.Method == http.MethodGet:
		head := req.URL.Query().Get("head")
		var out []*PR
		for _, pr := range r.PRs {
			if head == "" || head == r.Owner+":"+pr.Head.Ref {
				out = append(out, pr)
			}
		}
		if out == nil {
			out = []*PR{}
		}
		writeJSON(w, out)
	case len(rest) == 1 && rest[0] == "pulls" && req.Method == http.MethodPost:
		if s.FailAllWrites || s.FailNextCreatePR {
			s.FailNextCreatePR = false
			http.Error(w, `{"message":"Server Error"}`, 500)
			return
		}
		var in struct{ Title, Body, Head, Base string }
		json.NewDecoder(req.Body).Decode(&in)
		headSHA, ok := r.branchHead(in.Head)
		if !ok {
			http.Error(w, `{"message":"Validation Failed: head branch does not exist"}`, 422)
			return
		}
		pr := &PR{Number: r.nextPR, State: "open", Title: in.Title, Body: in.Body}
		pr.Head.Ref, pr.Head.SHA, pr.Base.Ref = in.Head, headSHA, in.Base
		pr.HTMLURL = fmt.Sprintf("https://github.example/%s/%s/pull/%d", r.Owner, r.Name, pr.Number)
		r.nextPR++
		r.PRs = append(r.PRs, pr)
		if s.DropNextCreatePRResponse {
			s.DropNextCreatePRResponse = false
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
			panic("cannot hijack")
		}
		w.WriteHeader(201)
		writeJSON(w, pr)
	default:
		http.Error(w, `{"message":"Not Found"}`, 404)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
