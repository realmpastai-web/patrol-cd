package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves the endpoints the release check reads: workflow runs by head SHA, the jobs
// of a run and the pull requests of a commit.
type fakeGitHub struct {
	mu   sync.Mutex
	runs []workflowRun
	jobs map[int64][]workflowJob
	// pulls are the pull requests of a commit; rawPulls, when set for a commit, is served instead
	// as is; pullsStatus, when set, answers every pull request listing with that status.
	pulls       map[string][]pullRequest
	rawPulls    map[string]string
	pullsStatus int
	// queries records the query strings of the runs listing.
	queries []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/repos/o/r/actions/workflows/ci.yml/runs" {
		f.queries = append(f.queries, r.URL.RawQuery)
		// Like GitHub: filter by head_sha only, so that the client's own checks of event and
		// branch are exercised.
		var out []workflowRun
		for _, run := range f.runs {
			if run.HeadSHA == r.URL.Query().Get("head_sha") {
				out = append(out, run)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(out), "workflow_runs": out})
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/commits/"); ok {
		if sha, ok := strings.CutSuffix(rest, "/pulls"); ok {
			switch {
			case f.pullsStatus != 0:
				http.Error(w, "unavailable", f.pullsStatus)
			case f.rawPulls[sha] != "":
				_, _ = w.Write([]byte(f.rawPulls[sha]))
			default:
				// Like GitHub: an empty list, not 404, for a commit without pull requests.
				_ = json.NewEncoder(w).Encode(append([]pullRequest{}, f.pulls[sha]...))
			}
			return
		}
	}
	var id int64
	if _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/actions/runs/%d/jobs", &id); err == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": f.jobs[id]})
		return
	}
	http.NotFound(w, r)
}

// mergedPull is pull request number of repository repo from branch head, titled title, merged
// into main as sha.
func mergedPull(number int, sha, repo, head, title string) pullRequest {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	p := pullRequest{Number: number, Title: title, MergedAt: &at, MergeCommitSHA: sha}
	p.Base.Ref = "main"
	p.Head.Ref = head
	p.Head.Repo.FullName = repo
	return p
}

// releasePR is the release pull request of version v that release-pr.yml opens, merged as sha.
func releasePR(number int, sha, v string) pullRequest {
	return mergedPull(number, sha, "o/r", "release/next", "chore(release): v"+v)
}

func allJobs(conclusion string) []workflowJob {
	var jobs []workflowJob
	for _, n := range requiredJobs {
		jobs = append(jobs, workflowJob{Name: n, Status: "completed", Conclusion: conclusion})
	}
	return jobs
}

// staticOnly is a documentation-only push: the full tier is skipped and the run still succeeds.
func staticOnly() []workflowJob {
	jobs := allJobs("skipped")
	for i := range jobs {
		switch jobs[i].Name {
		case "Choose tiers", "Static checks", "CI":
			jobs[i].Conclusion = "success"
		}
	}
	return jobs
}

func TestCICheck(t *testing.T) {
	t.Parallel()
	const sha = "1111111111111111111111111111111111111111"
	t0 := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	run := func(id int64, at time.Time, status, conclusion string) workflowRun {
		return workflowRun{ID: id, HeadSHA: sha, HeadBranch: "main", Event: "push", Status: status, Conclusion: conclusion, CreatedAt: at}
	}
	tests := []struct {
		name string
		runs []workflowRun
		jobs map[int64][]workflowJob
		want ciState
		why  string
	}{
		{name: "no run yet", want: ciPending, why: "no ci.yml run"},
		{name: "in progress", runs: []workflowRun{run(1, t0, "in_progress", "")}, want: ciPending, why: "in_progress"},
		{name: "failed", runs: []workflowRun{run(1, t0, "completed", "failure")}, want: ciFailed, why: `"failure"`},
		{name: "full tier passed", runs: []workflowRun{run(1, t0, "completed", "success")}, jobs: map[int64][]workflowJob{1: allJobs("success")}, want: ciPassed},
		{name: "static only (documentation push) is not enough", runs: []workflowRun{run(1, t0, "completed", "success")},
			jobs: map[int64][]workflowJob{1: staticOnly()}, want: ciFailed, why: "Integration suite: skipped"},
		{name: "a required job is missing", runs: []workflowRun{run(1, t0, "completed", "success")},
			jobs: map[int64][]workflowJob{1: allJobs("success")[1:]}, want: ciFailed, why: "Choose tiers: missing"},
		{name: "the newest run counts, not an older success", runs: []workflowRun{run(1, t0, "completed", "success"), run(2, t0.Add(time.Hour), "completed", "failure")},
			jobs: map[int64][]workflowJob{1: allJobs("success")}, want: ciFailed, why: "run 2"},
		{name: "a newer success replaces an older failure", runs: []workflowRun{run(1, t0, "completed", "failure"), run(2, t0.Add(time.Hour), "completed", "success")},
			jobs: map[int64][]workflowJob{2: allJobs("success")}, want: ciPassed, why: "run 2"},
		{name: "pull request and other-branch runs of the same commit are ignored", runs: []workflowRun{
			{ID: 3, HeadSHA: sha, HeadBranch: "release/next", Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: t0},
			{ID: 4, HeadSHA: sha, HeadBranch: "other", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: t0},
		}, jobs: map[int64][]workflowJob{3: allJobs("success"), 4: allJobs("success")}, want: ciPending, why: "no ci.yml run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := &fakeGitHub{runs: tc.runs, jobs: tc.jobs}
			srv := httptest.NewServer(gh)
			defer srv.Close()
			c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
			got, why, err := c.check(context.Background(), sha)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || !strings.Contains(why, tc.why) {
				t.Fatalf("check = %s (%s), want %s containing %q", got, why, tc.want, tc.why)
			}
			if q := gh.queries[0]; !strings.Contains(q, "head_sha="+sha) || !strings.Contains(q, "event=push") || !strings.Contains(q, "branch=main") {
				t.Fatalf("runs query %q", q)
			}
		})
	}

	srv := httptest.NewServer(&fakeGitHub{})
	defer srv.Close()
	c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "wrong", workflow: "ci.yml", branch: "main"}
	if _, _, err := c.check(context.Background(), sha); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("API error not reported: %v", err)
	}
}

// TestRequiredJobsMatchCIWorkflow fails when a CI job is renamed or added without updating
// requiredJobs: a renamed job would read as "missing" and block every release, an added one
// would not be required.
func TestRequiredJobsMatchCIWorkflow(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	// Job names: "    name: X" right after a two-space job key.
	re := regexp.MustCompile(`(?m)^  [a-z][a-z0-9_-]*:\n    name: (.+)$`)
	var names []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		names = append(names, strings.TrimSpace(m[1]))
	}
	if strings.Join(names, "|") != strings.Join(sortedLike(requiredJobs, names), "|") || len(names) != len(requiredJobs) {
		t.Fatalf("ci.yml jobs %q, requiredJobs %q", names, requiredJobs)
	}
}

// sortedLike returns want in the order of have (for a set comparison that prints both lists).
func sortedLike(want, have []string) []string {
	in := map[string]bool{}
	for _, w := range want {
		in[w] = true
	}
	var out []string
	for _, h := range have {
		if in[h] {
			out = append(out, h)
		}
	}
	return out
}

// TestReleasePull: only the merge of this repository's release/next pull request with the
// release-pr.yml title of the pending version is a release commit. A pull request from a fork
// controls its branch name and title, so it is refused however it is named.
func TestReleasePull(t *testing.T) {
	t.Parallel()
	const sha = "2222222222222222222222222222222222222222"
	v := version{Minor: 2}
	other := func(change func(*pullRequest)) pullRequest {
		p := releasePR(5, sha, "0.2.0")
		change(&p)
		return p
	}
	tests := []struct {
		name  string
		pulls []pullRequest
		raw   string
		want  int
	}{
		{name: "the release pull request", pulls: []pullRequest{releasePR(5, sha, "0.2.0")}, want: 5},
		{name: "found among other pull requests of the commit",
			pulls: []pullRequest{mergedPull(3, "3333333333333333333333333333333333333333", "o/r", "feature", "feat: x"), releasePR(5, sha, "0.2.0")}, want: 5},
		{name: "no pull request: a direct push"},
		{name: "not merged", pulls: []pullRequest{other(func(p *pullRequest) { p.MergedAt = nil })}},
		{name: "merged as another commit (sha only arrived with it)", pulls: []pullRequest{other(func(p *pullRequest) { p.MergeCommitSHA = "4444444444444444444444444444444444444444" })}},
		{name: "merged into another branch", pulls: []pullRequest{other(func(p *pullRequest) { p.Base.Ref = "release-0.1" })}},
		{name: "from another branch", pulls: []pullRequest{other(func(p *pullRequest) { p.Head.Ref = "docs/changelog" })}},
		{name: "from a fork's release/next with the release title", pulls: []pullRequest{other(func(p *pullRequest) { p.Head.Repo.FullName = "someone/r" })}},
		{name: "from a deleted fork", raw: `[{"number":5,"title":"chore(release): v0.2.0","merged_at":"2026-10-22T09:00:00Z","merge_commit_sha":"` + sha +
			`","base":{"ref":"main"},"head":{"ref":"release/next","repo":null}}]`},
		{name: "another title", pulls: []pullRequest{other(func(p *pullRequest) { p.Title = "docs: add a changelog" })}},
		{name: "the title of another version", pulls: []pullRequest{other(func(p *pullRequest) { p.Title = "chore(release): v0.2.1" })}},
		{name: "the decoded form of a GitHub answer", raw: `[{"number":7,"title":"chore(release): v0.2.0","merged_at":"2026-10-22T09:00:00Z","merge_commit_sha":"` + sha +
			`","base":{"ref":"main"},"head":{"ref":"release/next","repo":{"full_name":"o/r"}}}]`, want: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := &fakeGitHub{pulls: map[string][]pullRequest{sha: tc.pulls}, rawPulls: map[string]string{sha: tc.raw}}
			srv := httptest.NewServer(gh)
			defer srv.Close()
			c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
			got, err := c.releasePull(context.Background(), sha, v)
			if err != nil || got != tc.want {
				t.Fatalf("releasePull = %d, %v; want %d", got, err, tc.want)
			}
		})
	}

	srv := httptest.NewServer(&fakeGitHub{pullsStatus: http.StatusBadGateway})
	defer srv.Close()
	c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
	if _, err := c.releasePull(context.Background(), sha, v); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("API error not reported: %v", err)
	}
}

// TestReleaseWorkflowsMatch fails when the workflows drift from what the release check assumes:
// the branch and title release-pr.yml gives the release pull request, and a concurrency group of
// release.yml that only the events of its plan job share.
func TestReleaseWorkflowsMatch(t *testing.T) {
	t.Parallel()
	pr, err := os.ReadFile("../../.github/workflows/release-pr.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  BRANCH: " + releaseBranch + "\n", `title="` + releaseTitlePrefix + `$VERSION"`} {
		if !strings.Contains(string(pr), want) {
			t.Errorf("release-pr.yml has no %q", want)
		}
	}

	rel, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	group := regexp.MustCompile(`(?m)^  group: \$\{\{ (.+) \}\}$`).FindSubmatch(rel)
	plan := regexp.MustCompile(`(?m)^  plan:\n(?:    .*\n)*?    if: >-\n((?:      .*\n)+)`).FindSubmatch(rel)
	if group == nil || plan == nil {
		t.Fatalf("release.yml: concurrency group %q, plan if %q", group, plan)
	}
	oneLine := func(b []byte) string { return strings.Join(strings.Fields(string(b)), " ") }
	if want := "(" + oneLine(plan[1]) + ") && 'release' || format('release-ignored-{0}', github.run_id)"; oneLine(group[1]) != want {
		t.Fatalf("release.yml concurrency group\n %s\nwant\n %s", oneLine(group[1]), want)
	}
}

// TestSelectRelease covers both events of release.yml through the one selection they share: a
// CI run of a later commit and a manual run on main's tip both resolve to the release commit,
// and only a full-tier pass on that commit releases it.
func TestSelectRelease(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	r := newScratchRepo(t, dir)
	r.write(chartDir+"/Chart.yaml", "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\n")
	r.write(changelogFile, changelogHeader+"\n## [0.1.0] - 2026-10-19\n\n### Features\n\n- first\n")
	r.commit("feat: first")
	r.git("tag", "v0.1.0")
	r.commit("feat(ui): second")

	// The release pull request: prepared on a branch, merged with a merge commit.
	r.git("checkout", "-q", "-b", "release/next")
	var out bytes.Buffer
	if err := run(ctx, []string{"prepare", "-root", dir, "-repo-url", "https://github.com/o/r"}, &out); err != nil || out.String() != "0.2.0\n" {
		t.Fatalf("prepare = %q, %v", out.String(), err)
	}
	r.commit("chore(release): v0.2.0")
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "Merge pull request #5 from o/release/next", "release/next")
	releaseSHA := r.head()
	// Pushed to main before CI of the release commit finished: a docs change and a feature.
	// Neither is in the 0.2.0 section, so neither may be tagged v0.2.0.
	r.write("docs/x.md", "x\n")
	r.commit("docs: a note")
	r.commit("feat(cli): sneaked in")
	tip := r.head()

	gh, sel, addRun := selectionHarness(t, dir)

	// GitHub has not linked the merge to its pull request (yet): no commit qualifies, and the
	// reason says to re-run rather than naming a commit. Only the merge since v0.1.0 was looked at.
	if s := sel(); s.sha != "" || s.ci != ciFailed || s.release() || !strings.Contains(s.ciReason, "re-run") ||
		!strings.Contains(s.ciReason, "none of the 1 commits since v0.1.0 ") {
		t.Fatalf("without the pull request: %+v", s)
	}
	gh.addPull(releasePR(5, releaseSHA, "0.2.0"))

	// The later commits' CI passed (the docs push only ran the static tier); the release
	// commit's has not run: nothing is released, and the release commit is the one checked.
	addRun(1, tip, allJobs("success"))
	if s := sel(); s.sha != releaseSHA || s.pull != 5 || s.ci != ciPending || s.release() {
		t.Fatalf("before the release commit's CI: sha=%.7s (want %.7s) pull=%d ci=%s release=%t", s.sha, releaseSHA, s.pull, s.ci, s.release())
	}
	// A static-only pass on the release commit is not enough.
	addRun(2, releaseSHA, staticOnly())
	if s := sel(); s.ci != ciFailed || s.release() {
		t.Fatalf("static-only run: ci=%s release=%t (%s)", s.ci, s.release(), s.ciReason)
	}
	// A full-tier pass releases the release commit, not main's tip.
	addRun(3, releaseSHA, allJobs("success"))
	if s := sel(); !s.release() || s.sha != releaseSHA {
		t.Fatalf("full pass: sha=%.7s release=%t (%s)", s.sha, s.release(), s.ciReason)
	}

	// A later pull request from a fork changes CHANGELOG.md outside the pending section, with the
	// release pull request's branch name and title and a passing CI run: it is passed over, and
	// the release goes ahead from the release commit.
	data, err := os.ReadFile(filepath.Join(dir, changelogFile))
	if err != nil {
		t.Fatal(err)
	}
	r.write(changelogFile, strings.Replace(string(data), "- first", "- first, reworded", 1))
	r.commit("docs: reword an old entry")
	foreign := r.head()
	gh.addPull(mergedPull(8, foreign, "someone/r", "release/next", "chore(release): v0.2.0"))
	addRun(4, foreign, allJobs("success"))
	if s := sel(); !s.release() || s.sha != releaseSHA {
		t.Fatalf("after a foreign edit outside the section: sha=%.7s (want %.7s) release=%t (%s)", s.sha, releaseSHA, s.release(), s.ciReason)
	}

	// Another one rewrites the pending section itself: no commit carries the section main has
	// now, so nothing is released until the version is prepared again, and the reason repeats
	// nothing from the pull request.
	data, err = os.ReadFile(filepath.Join(dir, changelogFile))
	if err != nil {
		t.Fatal(err)
	}
	r.write(changelogFile, strings.Replace(string(data), "### Features", "### Features\n\n- <b>injected</b>", 1))
	r.commit("docs: edit the release notes")
	gh.addPull(mergedPull(9, r.head(), "someone/r", "docs/notes", "docs: <b>injected</b>"))
	addRun(5, r.head(), allJobs("success"))
	if s := sel(); s.release() || s.sha != "" || s.ci != ciFailed ||
		!strings.Contains(s.ciReason, "prepare 0.2.0 again") || strings.Contains(s.ciReason, "injected") || strings.Contains(s.ciReason, "someone") {
		t.Fatalf("after a foreign edit of the section: %+v", s)
	}

	// Tagged: nothing pending, no API call needed.
	r.git("tag", "v0.2.0", releaseSHA)
	if s, err := (&repo{root: dir}).selectRelease(ctx, nil); err != nil || s.status.pending() || s.release() || s.sha != "" {
		t.Fatalf("after the tag: %+v, %v", s, err)
	}
}

// selectionHarness returns a fake GitHub for the scratch repository in dir, a selectRelease call
// on it and a way to add a completed push run of CI with the given jobs.
func selectionHarness(t *testing.T, dir string) (*fakeGitHub, func() selection, func(id int64, sha string, jobs []workflowJob)) {
	t.Helper()
	gh := &fakeGitHub{jobs: map[int64][]workflowJob{}, pulls: map[string][]pullRequest{}}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	c := &ciChecker{client: srv.Client(), api: srv.URL, repo: "o/r", token: "test-token", workflow: "ci.yml", branch: "main"}
	at := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	addRun := func(id int64, sha string, jobs []workflowJob) {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.runs = append(gh.runs, workflowRun{ID: id, HeadSHA: sha, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: at.Add(time.Duration(id) * time.Minute)})
		gh.jobs[id] = jobs
	}
	sel := func() selection {
		t.Helper()
		s, err := (&repo{root: dir}).selectRelease(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return gh, sel, addRun
}

func (f *fakeGitHub) addPull(p pullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[p.MergeCommitSHA] = append(f.pulls[p.MergeCommitSHA], p)
}

// TestSelectReleaseSquashAndReprepare: a changelog a pull request from elsewhere adds is never a
// release, even with a passing CI run; a squash-merged release pull request is its own release
// commit; when it fails CI for a real defect, the fix plus an explicit re-prepare of the same
// version moves the release commit to the new merge; and reverting that merge moves it back.
func TestSelectReleaseSquashAndReprepare(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	r := newScratchRepo(t, dir)
	r.write(chartDir+"/Chart.yaml", "apiVersion: v2\nname: patrol-cd\nversion: 0.1.0\nappVersion: \"0.1.0\"\n")
	r.commit("feat: first")
	call := func(args ...string) string {
		t.Helper()
		var b bytes.Buffer
		if err := run(ctx, append(args[:1:1], append([]string{"-root", dir, "-repo-url", "https://github.com/o/r"}, args[1:]...)...), &b); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return b.String()
	}
	gh, sel, addRun := selectionHarness(t, dir)

	// No tag and Chart.yaml at 0.1.0: a pull request that adds a 0.1.0 section makes a release
	// pending. It comes from a fork with a passing CI run, and it is not released.
	r.write(changelogFile, changelogHeader+"\n## [0.1.0] - 2026-10-20\n\n- <b>my notes</b>\n")
	r.commit("docs: add a changelog (#4)")
	foreign := r.head()
	gh.addPull(mergedPull(4, foreign, "someone/r", "release/next", "chore(release): v0.1.0"))
	addRun(1, foreign, allJobs("success"))
	if s := sel(); !s.status.pending() || s.release() || s.sha != "" || s.ci != ciFailed || strings.Contains(s.ciReason, "my notes") {
		t.Fatalf("foreign changelog: %+v", s)
	}
	// Nothing new is proposed while it is pending; preparing the pending version replaces the
	// foreign section, and the squash of that release pull request is the release commit.
	if got := call("next"); got != "" {
		t.Fatalf("next while pending = %q", got)
	}
	call("prepare", "-version", "0.1.0")
	r.commit("chore(release): v0.1.0 (#6)") // squash merge
	first := r.head()
	gh.addPull(releasePR(6, first, "0.1.0"))
	addRun(2, first, allJobs("failure"))
	if s := sel(); s.sha != first || s.pull != 6 || s.ci != ciFailed || s.release() {
		t.Fatalf("squash merge: sha=%.7s (want %.7s) pull=%d ci=%s", s.sha, first, s.pull, s.ci)
	}

	// CI failed on it for a real defect; the fix lands, then the same version is prepared again.
	r.commit("fix(api): the defect")
	if got := call("next", "-version", "0.1.0"); got != "0.1.0\n" {
		t.Fatalf("explicit re-prepare of the pending version = %q", got)
	}
	call("prepare", "-version", "0.1.0")
	data, err := os.ReadFile(filepath.Join(dir, changelogFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "## [0.1.0]") != 1 || !strings.Contains(string(data), "the defect") || strings.Contains(string(data), "my notes") {
		t.Fatalf("re-prepared changelog:\n%s", data)
	}
	r.commit("chore(release): v0.1.0 (#7)")
	second := r.head()
	gh.addPull(releasePR(7, second, "0.1.0"))
	addRun(3, second, allJobs("success"))
	if s := sel(); s.sha != second || s.pull != 7 || !s.release() {
		t.Fatalf("after re-prepare: sha=%.7s (want %.7s) pull=%d release=%t (%s)", s.sha, second, s.pull, s.release(), s.ciReason)
	}

	// The re-prepare is reverted by a direct push: main describes the release as the first
	// release pull request did again, so that is the release commit again (and its CI failed).
	r.git("revert", "--no-edit", "HEAD")
	if s := sel(); s.sha != first || s.pull != 6 || s.ci != ciFailed || s.release() {
		t.Fatalf("after reverting the re-prepare: sha=%.7s (want %.7s) pull=%d ci=%s", s.sha, first, s.pull, s.ci)
	}
}

type scratchRepo struct {
	t   *testing.T
	dir string
}

func newScratchRepo(t *testing.T, dir string) *scratchRepo {
	r := &scratchRepo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *scratchRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-10-20T10:00:00Z", "GIT_COMMITTER_DATE=2026-10-20T10:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *scratchRepo) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *scratchRepo) commit(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
}

func (r *scratchRepo) head() string { return r.git("rev-parse", "HEAD") }
