package main

import (
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

// The release workflow publishes exactly one commit: the release commit, the merge (or the
// squashed or rebased commit) of this repository's release pull request, the one release-pr.yml
// opens from release/next. It is the newest commit on main's first-parent history since the last
// release tag that changed CHANGELOG.md, carries the pending version in Chart.yaml and the same
// CHANGELOG.md section as main's tip, and is the merge of that pull request (GitHub's pull requests
// of the commit). Any other commit that changed CHANGELOG.md, a pull request from a fork or from
// another branch or a direct push, is passed over: it neither publishes nor blocks the release.
// Whichever event started the workflow (a CI run of some later commit, or a manual run on main's
// tip), the commit is chosen the same way, and it is released only after a CI run of a push of
// that very commit passed every job of the full tier. Releasing the commit that triggered the
// event instead would tag a commit the changelog does not describe (commits after the release
// commit would land in the tag but in no section, and the next release starts from the tag) and
// could rely on a CI run that skipped the full tier (a documentation-only push runs the static
// checks only).

// releaseBranch and releaseTitlePrefix are the head branch and the title release-pr.yml gives the
// release pull request ("chore(release): vX.Y.Z"); releaseEnvironment is the environment the
// image and publish jobs of release.yml run in, whose required reviewer approves each release.
const (
	releaseBranch      = "release/next"
	releaseTitlePrefix = "chore(release): v"
	releaseEnvironment = "release"
)

// requiredJobs are the CI jobs (names as GitHub reports them, .github/workflows/ci.yml) that
// must have succeeded on the release commit; a skipped job (a reduced tier) does not count.
var requiredJobs = []string{
	"Choose tiers",
	"Static checks",
	"Unit tests",
	"Web UI",
	"Image (linux/amd64)",
	"Integration suite",
	"Deploy suite",
	"Kind (install, verify, lifecycle)",
	"CI",
}

// changelogCommits returns the commits on the first-parent history of HEAD after the tag since
// ("": all of it) that changed CHANGELOG.md, newest first: the candidates for the release commit.
func changelogCommits(ctx context.Context, dir, since string) ([]string, error) {
	rev := "HEAD"
	if since != "" {
		rev = since + "..HEAD"
	}
	out, err := git(ctx, dir, "log", "--first-parent", "--format=%H", rev, "--", changelogFile)
	if err != nil {
		return nil, err
	}
	shas := strings.Fields(out)
	if len(shas) == 0 {
		return nil, fmt.Errorf("no commit on the first-parent history of HEAD changes %s", changelogFile)
	}
	return shas, nil
}

// ciState is the outcome of the CI check of the release commit.
type ciState string

const (
	ciPassed  ciState = "passed"  // a completed run passed every required job
	ciPending ciState = "pending" // no run yet, or the newest run is not finished
	// The newest run failed or skipped a required job, or no commit is the release commit: either
	// way the release is blocked (docs/guides/releasing.md).
	ciFailed ciState = "failed"
)

// ciChecker reads workflow runs, their jobs and the pull requests of a commit from the GitHub
// REST API.
type ciChecker struct {
	client   *http.Client
	api      string // https://api.github.com
	repo     string // owner/name
	token    string
	workflow string // workflow file name, ci.yml
	branch   string // main
}

type workflowRun struct {
	ID         int64     `json:"id"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
	HTMLURL    string    `json:"html_url"`
}

type workflowJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// pullRequest is a pull request as GET /repos/{repo}/commits/{sha}/pulls returns it. head.repo is
// null for a pull request from a deleted fork and decodes to an empty name.
type pullRequest struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// releasePull returns the number of this repository's release pull request of v whose merge is
// sha, or 0 when sha is not one: the pull request must be merged into the branch, its merge
// (merge, squash or rebased commit) must be sha, and it must come from releaseBranch of this
// repository with the title release-pr.yml gives it. The repository is what tells it apart: a
// fork controls its branch name and its title, not its repository. GitHub's owner and repository
// names are ASCII and case-insensitive, and it answers with their canonical case whatever case
// -repo has.
func (c *ciChecker) releasePull(ctx context.Context, sha string, v version) (int, error) {
	var pulls []pullRequest
	if err := c.get(ctx, "/repos/"+c.repo+"/commits/"+url.PathEscape(sha)+"/pulls?per_page=100", &pulls); err != nil {
		return 0, err
	}
	for _, p := range pulls {
		if p.MergedAt != nil && p.MergeCommitSHA == sha && p.Base.Ref == c.branch &&
			p.Head.Ref == releaseBranch && strings.EqualFold(p.Head.Repo.FullName, c.repo) && p.Title == releaseTitlePrefix+v.String() {
			return p.Number, nil
		}
	}
	return 0, nil
}

// environmentGuarded reports whether releaseEnvironment exists and has a required reviewer. GitHub
// creates an environment a job names on first use, without protection, so without this check a
// release could publish before the owner set the environment up (docs/guides/releasing.md).
func (c *ciChecker) environmentGuarded(ctx context.Context) (bool, error) {
	var env struct {
		ProtectionRules []struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		} `json:"protection_rules"`
	}
	err := c.get(ctx, "/repos/"+c.repo+"/environments/"+releaseEnvironment, &env)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, r := range env.ProtectionRules {
		if r.Type == "required_reviewers" && len(r.Reviewers) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// check reports whether CI passed on a push of sha to the branch. Only the newest run counts: a
// re-run (a new attempt of the same run) replaces the result of the attempt before it.
func (c *ciChecker) check(ctx context.Context, sha string) (ciState, string, error) {
	q := url.Values{"head_sha": {sha}, "event": {"push"}, "branch": {c.branch}, "per_page": {"100"}}
	var runs struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := c.get(ctx, "/repos/"+c.repo+"/actions/workflows/"+url.PathEscape(c.workflow)+"/runs?"+q.Encode(), &runs); err != nil {
		return "", "", err
	}
	var newest *workflowRun
	for i := range runs.WorkflowRuns {
		r := &runs.WorkflowRuns[i]
		// The query filters already; checked again so that a misbehaving filter cannot pick
		// another commit's run.
		if r.HeadSHA != sha || r.Event != "push" || r.HeadBranch != c.branch {
			continue
		}
		if newest == nil || r.CreatedAt.After(newest.CreatedAt) || (r.CreatedAt.Equal(newest.CreatedAt) && r.ID > newest.ID) {
			newest = r
		}
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	if newest == nil {
		return ciPending, fmt.Sprintf("no %s run for the push of %s to %s yet", c.workflow, short, c.branch), nil
	}
	if newest.Status != "completed" {
		return ciPending, fmt.Sprintf("%s run %d for %s is %s", c.workflow, newest.ID, short, newest.Status), nil
	}
	if newest.Conclusion != "success" {
		return ciFailed, fmt.Sprintf("%s run %d for %s concluded %q (%s)", c.workflow, newest.ID, short, newest.Conclusion, newest.HTMLURL), nil
	}
	var jobs struct {
		Jobs []workflowJob `json:"jobs"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.repo, newest.ID), &jobs); err != nil {
		return "", "", err
	}
	got := map[string]workflowJob{}
	for _, j := range jobs.Jobs {
		got[j.Name] = j
	}
	var bad []string
	for _, name := range requiredJobs {
		j, ok := got[name]
		switch {
		case !ok:
			bad = append(bad, name+": missing")
		case j.Conclusion != "success":
			bad = append(bad, fmt.Sprintf("%s: %s", name, orDefault(j.Conclusion, j.Status)))
		}
	}
	if len(bad) > 0 {
		return ciFailed, fmt.Sprintf("%s run %d for %s did not pass the full tier (%s; %s)", c.workflow, newest.ID, short, strings.Join(bad, ", "), newest.HTMLURL), nil
	}
	return ciPassed, fmt.Sprintf("%s run %d for %s passed every required job (%s)", c.workflow, newest.ID, short, newest.HTMLURL), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// statusError is a GET that GitHub answered with a status other than 200.
type statusError struct {
	path   string
	status int
	body   []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d: %.200s", e.path, e.status, e.body)
}

func (c *ciChecker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.api, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{path: path, status: resp.StatusCode, body: body}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// selection is what the release workflow's plan job acts on.
type selection struct {
	status   releaseStatus
	sha      string // the release commit ("" when nothing is pending or no commit qualifies)
	pull     int    // its release pull request
	ci       ciState
	ciReason string
}

func (s selection) release() bool { return s.status.pending() && s.ci == ciPassed }

// selectRelease works out whether to publish and from which commit. HEAD is main's tip.
func (r *repo) selectRelease(ctx context.Context, c *ciChecker) (selection, error) {
	var sel selection
	st, err := r.status(ctx)
	if err != nil {
		return sel, err
	}
	sel.status = st
	if !st.pending() {
		return sel, nil
	}
	head, err := git(ctx, r.root, "show", "HEAD:"+changelogFile)
	if err != nil {
		return sel, err
	}
	want, _ := section(head, st.current)
	prevTag, _, err := latestTag(ctx, r.root)
	if err != nil {
		return sel, err
	}
	candidates, err := changelogCommits(ctx, r.root, prevTag)
	if err != nil {
		return sel, err
	}
	if c == nil {
		return sel, errors.New("no GitHub API access configured")
	}
	guarded, err := c.environmentGuarded(ctx)
	if err != nil {
		return sel, err
	}
	if !guarded {
		sel.ci = ciFailed
		sel.ciReason = fmt.Sprintf("the environment %q that the image and publish jobs run in does not exist or has no required reviewer; "+
			"set it up as docs/guides/releasing.md describes, then re-run this run", releaseEnvironment)
		return sel, nil
	}
	for _, sha := range candidates {
		// The release commit carries the release as main describes it: the chart version and the
		// same section. A later edit of the section leaves no commit that does until the version
		// is prepared again; reverting a re-prepare brings back the release commit before it. A
		// commit without either file (one that deleted or moved it) is not the release commit.
		chart, ok, err := showFile(ctx, r.root, sha, chartDir+"/Chart.yaml")
		if err != nil {
			return sel, err
		}
		if !ok {
			continue
		}
		if v, err := chartVersion(chart); err != nil || v != st.current {
			continue
		}
		changelog, ok, err := showFile(ctx, r.root, sha, changelogFile)
		if err != nil {
			return sel, err
		}
		if !ok {
			continue
		}
		if got, ok := section(changelog, st.current); !ok || got != want {
			continue
		}
		pull, err := c.releasePull(ctx, sha, st.current)
		if err != nil {
			return sel, err
		}
		if pull == 0 {
			continue
		}
		sel.sha, sel.pull = sha, pull
		sel.ci, sel.ciReason, err = c.check(ctx, sha)
		return sel, err
	}
	// Built from fixed text only: pull request titles and branches are untrusted.
	since := "the first commit"
	if prevTag != "" {
		since = prevTag
	}
	sel.ci = ciFailed
	sel.ciReason = fmt.Sprintf("none of the %d commits since %s that changed %s is the merge of this repository's %s pull request %q "+
		"with the %s section main has; prepare %s again with the Release PR workflow, or re-run this run if GitHub has not linked the pull request yet",
		len(candidates), since, changelogFile, releaseBranch, releaseTitlePrefix+st.current.String(), st.current, st.current)
	return sel, nil
}

// showFile returns the content of path in commit sha, and false when the commit has no such file.
func showFile(ctx context.Context, dir, sha, path string) (string, bool, error) {
	out, err := git(ctx, dir, "ls-tree", "--name-only", sha, "--", path)
	if err != nil || out == "" {
		return "", false, err
	}
	content, err := git(ctx, dir, "show", sha+":"+path)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// section returns the CHANGELOG.md section of v, heading included.
func section(changelog string, v version) (string, bool) {
	start, end, ok := findSection(changelog, v)
	if !ok {
		return "", false
	}
	return changelog[start:end], true
}
