// Package publish pushes a frozen proposal commit and opens a pull request
// for it, as two journaled, externally observable operations, and
// reconciles them after an interruption by asking the remote what
// happened. It never regenerates anything from a proposal, never replays
// blindly, and never consults a model.
package publish

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/joeylking/repo-steward/internal/faultpoint"
	"github.com/joeylking/repo-steward/internal/github"
	"github.com/joeylking/repo-steward/internal/gitx"
	"github.com/joeylking/repo-steward/internal/proposal"
	"github.com/joeylking/repo-steward/internal/task"
	"github.com/joeylking/repo-steward/internal/workspace"
)

// Destination is where a proposal is published. It is captured at run
// start, separately from the scratch clone's origin, and verified before
// any credential is used.
type Destination struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// APIBase and PushURL default from Host; tests point them at a fake
	// server and a local bare repository.
	APIBase string `json:"api_base"`
	PushURL string `json:"push_url"`
	// BaseRef is the branch a pull request targets; BaseHeadAtStart is the
	// destination's head of that branch when the run started, which must
	// equal the local base commit.
	BaseRef         string `json:"base_ref"`
	BaseHeadAtStart string `json:"base_head_at_start"`
}

var remoteRE = regexp.MustCompile(`^(?:git@([^:]+):|ssh://git@([^/]+)/|https://([^/]+)/)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseRemote extracts host, owner, and repository from a Git remote URL.
func ParseRemote(remote string) (Destination, error) {
	m := remoteRE.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return Destination{}, fmt.Errorf("publish: cannot parse remote %q", remote)
	}
	host := m[1] + m[2] + m[3]
	d := Destination{Host: host, Owner: m[4], Repo: m[5]}
	d.APIBase, d.PushURL = defaultsFor(host, d.Owner, d.Repo)
	return d, nil
}

func defaultsFor(host, owner, repo string) (string, string) {
	if host == "github.com" {
		return "https://api.github.com", "https://github.com/" + owner + "/" + repo + ".git"
	}
	return "https://" + host + "/api/v3", "https://" + host + "/" + owner + "/" + repo + ".git"
}

// ErrUnsupportedHost means the destination host is not GitHub.
var ErrUnsupportedHost = errors.New("publish: only github.com destinations are supported")

// ErrBaseMismatch means the destination's base branch head is not the local base commit.
var ErrBaseMismatch = errors.New("publish: destination base branch head differs from the local base commit")

// Verify checks the destination before any credential is used: the host
// is supported, the repository exists, the base branch exists, and its head
// is exactly the local base commit. The observed head is recorded.
func Verify(ctx context.Context, client *github.Client, d *Destination, baseRef, localBase string) error {
	if d.Host != "github.com" && !strings.HasPrefix(d.APIBase, "http://127.0.0.1") && !strings.HasPrefix(d.APIBase, "http://localhost") {
		return ErrUnsupportedHost
	}
	if baseRef == "" {
		return errors.New("publish: the source checkout is not on a branch; a pull request needs a base branch")
	}
	if _, err := client.GetRepository(ctx, d.Owner, d.Repo); err != nil {
		return fmt.Errorf("publish: destination %s/%s: %w", d.Owner, d.Repo, err)
	}
	head, err := client.BranchHead(ctx, d.Owner, d.Repo, baseRef)
	if err != nil {
		return fmt.Errorf("publish: base branch %s on %s/%s: %w", baseRef, d.Owner, d.Repo, err)
	}
	if head != localBase {
		return fmt.Errorf("%w: local %s, remote %s on %s (push or pull first)", ErrBaseMismatch, localBase[:12], head[:12], baseRef)
	}
	d.BaseRef, d.BaseHeadAtStart = baseRef, head
	return nil
}

// Result is what a completed publication reports.
type Result struct {
	ProposalID string `json:"proposal_id"`
	Branch     string `json:"branch"`
	HeadCommit string `json:"head_commit"`
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
	BaseMoved  bool   `json:"base_moved"`
	Recovered  bool   `json:"recovered,omitempty"`
}

// Marker returns the pull request body marker for a proposal.
func Marker(proposalID string) string { return "<!-- repo-steward:proposal:" + proposalID + " -->" }

// Publisher performs and reconciles publications.
type Publisher struct {
	Store  *task.Store
	WS     *workspace.Workspace
	Client *github.Client
	Dest   Destination
	// Token is made available to Git only for the push, through a
	// credential helper that reads a process-scoped variable. It is never
	// written to disk or passed as an argument.
	Token string
}

// Publish pushes the proposal commit and opens the pull request under the
// journal. Both operations are recorded before dispatch with markers the
// remote can be asked about.
func (p *Publisher) Publish(ctx context.Context, runID, stepID string, prop *proposal.Proposal) (*Result, error) {
	ops, err := p.Store.ListPublicationOps(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if op.ProposalID == prop.ID && !terminal(op.Status) {
			return nil, fmt.Errorf("publish: proposal %s has an in-flight operation %s; reconcile first", prop.ID, op.ID)
		}
	}
	res := &Result{ProposalID: prop.ID, Branch: prop.HeadRef, HeadCommit: prop.HeadCommit}
	baseNow, err := p.Client.BranchHead(ctx, p.Dest.Owner, p.Dest.Repo, prop.BaseRef)
	if err != nil {
		return nil, err
	}
	res.BaseMoved = baseNow != prop.BaseCommit

	// Push: idempotent for the same commit and ref.
	push := task.PublicationOp{ID: newID(), RunID: runID, ProposalID: prop.ID, StepID: stepID, Kind: "push", Status: task.OpPending, Marker: prop.HeadRef + "@" + prop.HeadCommit, BaseHeadAtPublish: baseNow}
	if err := p.Store.InsertPublicationOp(ctx, push); err != nil {
		return nil, err
	}
	push.Status, push.DispatchedAt = task.OpDispatched, task.Now()
	if err := p.Store.UpdatePublicationOp(ctx, push); err != nil {
		return nil, err
	}
	faultpoint.Hit("publish.push.dispatched")
	if err := p.push(ctx, prop); err != nil {
		push.Status, push.Detail = task.OpFailed, err.Error()
		p.Store.UpdatePublicationOp(ctx, push)
		return nil, err
	}
	faultpoint.Hit("publish.push.done_unrecorded")
	push.Status, push.RemoteRef, push.CompletedAt = task.OpDone, prop.HeadCommit, task.Now()
	if err := p.Store.UpdatePublicationOp(ctx, push); err != nil {
		return nil, err
	}

	// Pull request: identified by the marker in its body.
	pr := task.PublicationOp{ID: newID(), RunID: runID, ProposalID: prop.ID, StepID: stepID, Kind: "create_pr", Status: task.OpPending, Marker: Marker(prop.ID)}
	if err := p.Store.InsertPublicationOp(ctx, pr); err != nil {
		return nil, err
	}
	pr.Status, pr.DispatchedAt = task.OpDispatched, task.Now()
	if err := p.Store.UpdatePublicationOp(ctx, pr); err != nil {
		return nil, err
	}
	faultpoint.Hit("publish.pr.dispatched")
	created, err := p.Client.CreatePullRequest(ctx, p.Dest.Owner, p.Dest.Repo, prop.Title, body(prop, res.BaseMoved), prop.HeadRef, prop.BaseRef)
	if err != nil {
		pr.Status, pr.Detail = task.OpFailed, err.Error()
		p.Store.UpdatePublicationOp(ctx, pr)
		return nil, fmt.Errorf("publish: create pull request: %w", err)
	}
	faultpoint.Hit("publish.pr.done_unrecorded")
	pr.Status, pr.PRNumber, pr.PRURL, pr.CompletedAt = task.OpDone, created.Number, created.HTMLURL, task.Now()
	if err := p.Store.UpdatePublicationOp(ctx, pr); err != nil {
		return nil, err
	}
	res.PRNumber, res.PRURL = created.Number, created.HTMLURL
	return res, nil
}

func body(prop *proposal.Proposal, baseMoved bool) string {
	var b strings.Builder
	b.WriteString(prop.Body)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Validated against base commit %s.", prop.BaseCommit)
	if baseMoved {
		b.WriteString(" The base branch has moved since; the change was not rebased.")
	}
	b.WriteString("\n\n" + Marker(prop.ID) + "\n")
	return b.String()
}

// push runs git push with the token supplied through a credential helper
// that reads an environment variable set only for this invocation.
func (p *Publisher) push(ctx context.Context, prop *proposal.Proposal) error {
	args, env := PushCommand(p.Dest.PushURL, prop.HeadCommit, prop.HeadRef, p.Token)
	_, err := p.WS.Git().RunWith(ctx, env, nil, args...)
	if err != nil {
		var ge *gitx.Error
		if errors.As(err, &ge) {
			return fmt.Errorf("publish: push: %s", redact(ge.Stderr, p.Token))
		}
		return err
	}
	return nil
}

// PushCommand builds the git arguments and the process-scoped environment
// for a push. The token appears only in the environment; the credential
// helper text in the arguments contains no secret.
func PushCommand(pushURL, commit, branch, token string) (args, env []string) {
	if token != "" {
		args = append(args, "-c", "credential.helper=", "-c", `credential.helper=!f() { echo "username=x-access-token"; echo "password=$REPO_STEWARD_PUSH_TOKEN"; }; f`)
		env = []string{"REPO_STEWARD_PUSH_TOKEN=" + token}
	}
	args = append(args, "push", "--no-verify", pushURL, commit+":refs/heads/"+branch)
	return args, env
}

func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return strings.TrimSpace(s)
}

// Reconciliation describes what recovery concluded.
type Reconciliation struct {
	Outcome string   `json:"outcome"` // completed, continue, conflict
	Detail  string   `json:"detail"`
	Result  *Result  `json:"result,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// Reconcile inspects every non-terminal publication operation of the run
// and settles it against the remote. When both operations are done the
// publication is complete. It never re-dispatches a pull request creation
// without first looking for one carrying the marker.
func (p *Publisher) Reconcile(ctx context.Context, runID string) (*Reconciliation, error) {
	ops, err := p.Store.ListPublicationOps(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return &Reconciliation{Outcome: "continue", Detail: "no publication in flight"}, nil
	}
	rec := &Reconciliation{Outcome: "continue"}
	propID := ops[0].ProposalID
	props, err := p.Store.ListProposals(ctx, runID)
	if err != nil {
		return nil, err
	}
	var prop *proposal.Proposal
	for _, row := range props {
		if row.ID == propID {
			pp, err := proposal.Verify(ctx, p.Store, p.WS, runID, propID)
			if err != nil {
				return &Reconciliation{Outcome: "conflict", Detail: "proposal no longer verifies: " + err.Error()}, nil
			}
			prop = pp
		}
	}
	if prop == nil {
		return &Reconciliation{Outcome: "conflict", Detail: "publication operations refer to an unknown proposal"}, nil
	}
	res := &Result{ProposalID: prop.ID, Branch: prop.HeadRef, HeadCommit: prop.HeadCommit, Recovered: true}
	var pushDone, prDone bool
	for i := range ops {
		op := &ops[i]
		if op.ProposalID != prop.ID {
			continue
		}
		switch op.Kind {
		case "push":
			done, err := p.reconcilePush(ctx, op, prop, rec)
			if err != nil {
				return nil, err
			}
			pushDone = done
		case "create_pr":
			done, err := p.reconcilePR(ctx, op, prop, rec, res)
			if err != nil {
				return nil, err
			}
			prDone = done
		}
		if op.Status == task.OpConflict {
			rec.Outcome, rec.Detail = "conflict", op.Detail
			return rec, nil
		}
	}
	if pushDone && !prDone && !hasOp(ops, prop.ID, "create_pr") {
		// The push landed but the pull request was never dispatched: find
		// one by marker or create it now.
		op := task.PublicationOp{ID: newID(), RunID: runID, ProposalID: prop.ID, Kind: "create_pr", Status: task.OpDispatched, Marker: Marker(prop.ID), DispatchedAt: task.Now()}
		if err := p.Store.InsertPublicationOp(ctx, op); err != nil {
			return nil, err
		}
		done, err := p.reconcilePR(ctx, &op, prop, rec, res)
		if err != nil {
			return nil, err
		}
		if op.Status == task.OpConflict {
			rec.Outcome, rec.Detail = "conflict", op.Detail
			return rec, nil
		}
		prDone = done
	}
	if pushDone && prDone {
		rec.Outcome, rec.Result, rec.Detail = "completed", res, "publication reconciled from remote state"
	}
	return rec, nil
}

func (p *Publisher) reconcilePush(ctx context.Context, op *task.PublicationOp, prop *proposal.Proposal, rec *Reconciliation) (bool, error) {
	if op.Status == task.OpDone {
		return true, nil
	}
	remote, err := p.Client.RefHead(ctx, p.Dest.Owner, p.Dest.Repo, prop.HeadRef)
	switch {
	case errors.Is(err, github.ErrNotFound):
		// Nothing landed: push again; the same commit to the same ref is idempotent.
		if perr := p.push(ctx, prop); perr != nil {
			op.Status, op.Detail = task.OpFailed, perr.Error()
			p.Store.UpdatePublicationOp(ctx, *op)
			return false, perr
		}
		op.Status, op.RemoteRef, op.CompletedAt, op.Detail = task.OpDone, prop.HeadCommit, task.Now(), "pushed during reconciliation"
		rec.Actions = append(rec.Actions, "push: ref absent, pushed "+prop.HeadCommit[:12])
	case err != nil:
		return false, err
	case remote == prop.HeadCommit:
		op.Status, op.RemoteRef, op.CompletedAt, op.Detail = task.OpDone, remote, task.Now(), "ref already at the proposal commit"
		rec.Actions = append(rec.Actions, "push: ref already at "+remote[:12])
	default:
		op.Status, op.Detail = task.OpConflict, fmt.Sprintf("remote %s is at %s, not the proposal commit %s", prop.HeadRef, remote[:12], prop.HeadCommit[:12])
		rec.Actions = append(rec.Actions, "push: conflict")
	}
	return op.Status == task.OpDone, p.Store.UpdatePublicationOp(ctx, *op)
}

func (p *Publisher) reconcilePR(ctx context.Context, op *task.PublicationOp, prop *proposal.Proposal, rec *Reconciliation, res *Result) (bool, error) {
	if op.Status == task.OpDone {
		res.PRNumber, res.PRURL = op.PRNumber, op.PRURL
		return true, nil
	}
	prs, err := p.Client.ListPullRequestsByHead(ctx, p.Dest.Owner, p.Dest.Repo, p.Dest.Owner, prop.HeadRef)
	if err != nil {
		return false, err
	}
	var foreign *github.PullRequest
	for i := range prs {
		pr := &prs[i]
		if pr.Base.Ref != prop.BaseRef {
			continue
		}
		if strings.Contains(pr.Body, Marker(prop.ID)) {
			state := pr.State
			if pr.Merged {
				state = "merged"
			}
			op.Status, op.PRNumber, op.PRURL, op.CompletedAt, op.Detail = task.OpDone, pr.Number, pr.HTMLURL, task.Now(), "found by marker, state "+state
			res.PRNumber, res.PRURL = pr.Number, pr.HTMLURL
			rec.Actions = append(rec.Actions, fmt.Sprintf("pull request: found #%d by marker (%s); not recreated", pr.Number, state))
			return true, p.Store.UpdatePublicationOp(ctx, *op)
		}
		foreign = pr
	}
	if foreign != nil {
		op.Status, op.Detail = task.OpConflict, fmt.Sprintf("pull request #%d exists for %s without the proposal marker", foreign.Number, prop.HeadRef)
		rec.Actions = append(rec.Actions, "pull request: conflict")
		return false, p.Store.UpdatePublicationOp(ctx, *op)
	}
	// None found: the push must be confirmed before creating.
	remote, err := p.Client.RefHead(ctx, p.Dest.Owner, p.Dest.Repo, prop.HeadRef)
	if err != nil || remote != prop.HeadCommit {
		op.Status, op.Detail = task.OpFailed, "cannot create the pull request: remote ref is not at the proposal commit"
		p.Store.UpdatePublicationOp(ctx, *op)
		return false, nil
	}
	baseNow, _ := p.Client.BranchHead(ctx, p.Dest.Owner, p.Dest.Repo, prop.BaseRef)
	created, err := p.Client.CreatePullRequest(ctx, p.Dest.Owner, p.Dest.Repo, prop.Title, body(prop, baseNow != prop.BaseCommit), prop.HeadRef, prop.BaseRef)
	if err != nil {
		op.Status, op.Detail = task.OpFailed, err.Error()
		p.Store.UpdatePublicationOp(ctx, *op)
		return false, err
	}
	op.Status, op.PRNumber, op.PRURL, op.CompletedAt, op.Detail = task.OpDone, created.Number, created.HTMLURL, task.Now(), "created during reconciliation"
	res.PRNumber, res.PRURL = created.Number, created.HTMLURL
	rec.Actions = append(rec.Actions, fmt.Sprintf("pull request: none found, created #%d", created.Number))
	return true, p.Store.UpdatePublicationOp(ctx, *op)
}

func hasOp(ops []task.PublicationOp, propID, kind string) bool {
	for _, op := range ops {
		if op.ProposalID == propID && op.Kind == kind {
			return true
		}
	}
	return false
}

func terminal(s task.OpStatus) bool {
	return s == task.OpDone || s == task.OpFailed || s == task.OpConflict
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
