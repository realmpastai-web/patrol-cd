// Command release computes Patrol CD releases from the conventional commits on main: the next
// version, the CHANGELOG.md section, the chart version bump, the release notes and the release
// assets. It is what the release workflows (.github/workflows/release-pr.yml, release.yml) and
// the `make release-*` targets run. Guide: docs/guides/releasing.md.
//
// The version lives in deploy/helm/patrol-cd/Chart.yaml (version = appVersion) and in the git
// tag vX.Y.Z. A version is "pending" when Chart.yaml names it, CHANGELOG.md has its section and
// the tag does not exist yet: normally the release pull request was merged and the release
// workflow has not tagged it (select decides which commit, if any, is that merge). Nothing new is
// proposed while a release is pending.
//
// Usage:
//
//	go run ./hack/release next     [-version X.Y.Z]   # the next version, or nothing
//	go run ./hack/release preview  [-version X.Y.Z]   # the next CHANGELOG.md section
//	go run ./hack/release prepare  [-version X.Y.Z]   # write CHANGELOG.md and Chart.yaml
//	go run ./hack/release status   [-github-output]   # current=, tagged=, changelog=, pending=
//	go run ./hack/release select   [-github-output]   # status + sha= (release commit), pull=, ci=, release=
//	go run ./hack/release notes    -version X.Y.Z     # release notes from CHANGELOG.md
//	go run ./hack/release lint     REV-RANGE          # fail on non-conventional commit subjects
//	go run ./hack/release stage    -version X.Y.Z -registry HOST[/NS] -out DIR
//
// Commit types: a breaking change (type! or a BREAKING CHANGE trailer) bumps the major version
// (the minor before 1.0.0), feat the minor, fix/perf/revert the patch; docs, test, chore, ci,
// build, refactor and style are accepted but neither make a release nor appear in the changelog.
// Merge commits are skipped; a commit merged through "Merge pull request #N" links to #N.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	chartDir      = "deploy/helm/patrol-cd"
	manifestsDir  = "deploy/manifests"
	changelogFile = "CHANGELOG.md"
	// imageRepository is the chart's default image.repository; the release image is pushed as
	// REGISTRY/imageRepository.
	imageRepository = "patrol-cd/patrol-cd"
)

// lintTypes are the commit types `lint` accepts.
var lintTypes = map[string]bool{
	"feat": true, "fix": true, "perf": true, "revert": true, "docs": true, "test": true,
	"chore": true, "ci": true, "build": true, "refactor": true, "style": true,
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: release next|preview|prepare|status|notes|lint|stage [flags] (see hack/release/main.go)")
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	root := fs.String("root", ".", "repository root")
	ver := fs.String("version", "", "release version X.Y.Z (default: computed from the commits)")
	repoURL := fs.String("repo-url", "", "repository URL for links (default: $GITHUB_SERVER_URL/$GITHUB_REPOSITORY, else the origin remote)")
	ghOutput := fs.Bool("github-output", false, "status: also append the values to $GITHUB_OUTPUT")
	registry := fs.String("registry", "", "stage: registry (host[:port][/namespace]) the image is published to")
	out := fs.String("out", "", "stage: output directory")
	apiURL := fs.String("api-url", envOr("GITHUB_API_URL", "https://api.github.com"), "select: GitHub REST API URL")
	ghRepo := fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "select: repository owner/name")
	workflow := fs.String("workflow", "ci.yml", "select: CI workflow file whose run must pass on the release commit")
	branch := fs.String("branch", "main", "select: branch the release commit was pushed to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := &repo{root: *root, repoURL: *repoURL}

	switch cmd {
	case "next", "preview", "prepare":
		plan, err := r.plan(ctx, *ver)
		if err != nil {
			return err
		}
		if plan == nil {
			if cmd == "prepare" {
				return errors.New("nothing to release (no feat, fix, perf, revert or breaking commit since the last release, or a release is pending)")
			}
			return nil
		}
		section := renderSection(*plan)
		switch cmd {
		case "next":
			_, _ = fmt.Fprintln(stdout, plan.Version)
		case "preview":
			_, _ = fmt.Fprint(stdout, section)
		case "prepare":
			if err := r.write(plan.Version, section); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(stdout, plan.Version)
		}
		return nil
	case "status":
		st, err := r.status(ctx)
		if err != nil {
			return err
		}
		kv := fmt.Sprintf("current=%s\ntag=%s\ntagged=%t\nchangelog=%t\npending=%t\n",
			st.current, st.current.Tag(), st.tagged, st.changelog, st.pending())
		_, _ = fmt.Fprint(stdout, kv)
		if *ghOutput {
			return appendFile(os.Getenv("GITHUB_OUTPUT"), kv)
		}
		return nil
	case "select":
		if *ghRepo == "" {
			return errors.New("select: -repo (or GITHUB_REPOSITORY) is required")
		}
		c := &ciChecker{client: &http.Client{Timeout: 30 * time.Second}, api: *apiURL, repo: *ghRepo,
			token: envOr("GH_TOKEN", os.Getenv("GITHUB_TOKEN")), workflow: *workflow, branch: *branch}
		sel, err := r.selectRelease(ctx, c)
		if err != nil {
			return err
		}
		st := sel.status
		kv := fmt.Sprintf("current=%s\ntag=%s\ntagged=%t\nchangelog=%t\npending=%t\nsha=%s\npull=%d\nci=%s\nci_reason=%s\nrelease=%t\n",
			st.current, st.current.Tag(), st.tagged, st.changelog, st.pending(), sel.sha, sel.pull, sel.ci,
			strings.ReplaceAll(sel.ciReason, "\n", " "), sel.release())
		_, _ = fmt.Fprint(stdout, kv)
		if *ghOutput {
			return appendFile(os.Getenv("GITHUB_OUTPUT"), kv)
		}
		return nil
	case "notes":
		v, err := parseVersion(*ver)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(*root, changelogFile))
		if err != nil {
			return err
		}
		notes, ok := sectionNotes(string(data), v)
		if !ok {
			return fmt.Errorf("%s has no section for %s", changelogFile, v)
		}
		_, _ = fmt.Fprint(stdout, notes)
		return nil
	case "lint":
		if fs.NArg() != 1 {
			return errors.New("lint: want one revision range, e.g. origin/main..HEAD")
		}
		return lint(ctx, *root, fs.Arg(0), stdout)
	case "stage":
		v, err := parseVersion(*ver)
		if err != nil {
			return err
		}
		if *out == "" {
			return errors.New("stage: -out is required")
		}
		if !registryRe.MatchString(*registry) {
			return fmt.Errorf("stage: invalid -registry %q (want host[:port][/namespace])", *registry)
		}
		return stage(*root, *out, *registry, v)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// registryRe is the registry form image-multiarch accepts (hack/image.mk), without upper case.
var registryRe = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)*$`)

type repo struct {
	root    string
	repoURL string
}

type releaseStatus struct {
	current   version // Chart.yaml version
	tagged    bool    // vCURRENT exists
	changelog bool    // CHANGELOG.md has CURRENT's section
}

// pending: CHANGELOG.md has the section of the chart's version and it is not tagged yet.
func (s releaseStatus) pending() bool { return !s.tagged && s.changelog }

func (r *repo) status(ctx context.Context) (releaseStatus, error) {
	var st releaseStatus
	chart, err := os.ReadFile(filepath.Join(r.root, chartDir, "Chart.yaml"))
	if err != nil {
		return st, err
	}
	if st.current, err = chartVersion(string(chart)); err != nil {
		return st, err
	}
	if st.tagged, err = tagExists(ctx, r.root, st.current.Tag()); err != nil {
		return st, err
	}
	changelog, err := os.ReadFile(filepath.Join(r.root, changelogFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	_, _, st.changelog = findSection(string(changelog), st.current)
	return st, nil
}

// plan works out the next release, or nil when there is nothing to release. Without a release
// tag the first release is the chart's current version; with one, the commits since the tag
// choose the bump. An explicit version must be newer than the last release.
func (r *repo) plan(ctx context.Context, explicit string) (*releaseInfo, error) {
	st, err := r.status(ctx)
	if err != nil {
		return nil, err
	}
	// While a release is pending nothing new is proposed, except the pending version itself
	// asked for explicitly: that rebuilds its section after its release commit failed CI for a
	// real defect and the fix was merged, or when no commit is its release commit
	// (docs/guides/releasing.md).
	if st.pending() && explicit != st.current.String() {
		return nil, nil
	}
	prevTag, prev, err := latestTag(ctx, r.root)
	if err != nil {
		return nil, err
	}
	commits, err := collectCommits(ctx, r.root, prevTag)
	if err != nil {
		return nil, err
	}
	var next version
	switch {
	case explicit != "":
		if next, err = parseVersion(explicit); err != nil {
			return nil, err
		}
		if prevTag != "" && !prev.less(next) {
			return nil, fmt.Errorf("version %s is not newer than the last release %s", next, prevTag)
		}
		exists, err := tagExists(ctx, r.root, next.Tag())
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("tag %s already exists", next.Tag())
		}
	default:
		b := bumpFor(commits)
		if b == bumpNone {
			return nil, nil
		}
		next = prev.apply(b)
		if prevTag == "" {
			next = st.current
		}
	}
	date, err := git(ctx, r.root, "log", "-1", "--format=%cs", "HEAD")
	if err != nil {
		return nil, err
	}
	url, err := r.url(ctx)
	if err != nil {
		return nil, err
	}
	return &releaseInfo{Version: next, Previous: prevTag, Date: strings.TrimSpace(date), RepoURL: url, Commits: commits}, nil
}

var remoteRe = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// url is the repository's web URL for changelog links, or "" when it cannot be told.
func (r *repo) url(ctx context.Context) (string, error) {
	if r.repoURL != "" {
		return strings.TrimSuffix(r.repoURL, "/"), nil
	}
	if s, rp := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"); s != "" && rp != "" {
		return s + "/" + rp, nil
	}
	out, err := git(ctx, r.root, "remote", "get-url", "origin")
	if err != nil {
		return "", nil //nolint:nilerr // no origin remote: plain changelog without links
	}
	if m := remoteRe.FindStringSubmatch(strings.TrimSpace(out)); m != nil {
		return "https://github.com/" + m[1], nil
	}
	return "", nil
}

// write prepends the section to CHANGELOG.md and sets the chart's version. The chart's
// generated files (README) and the static manifests are regenerated by `make release-prepare`.
func (r *repo) write(v version, section string) error {
	clPath := filepath.Join(r.root, changelogFile)
	changelog, err := os.ReadFile(clPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	chartPath := filepath.Join(r.root, chartDir, "Chart.yaml")
	chart, err := os.ReadFile(chartPath)
	if err != nil {
		return err
	}
	newChart, err := setChartVersion(string(chart), v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(clPath, []byte(insertSection(string(changelog), section, v)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(chartPath, []byte(newChart), 0o644)
}

// lint checks every non-merge commit subject in rng and reports all offenders.
func lint(ctx context.Context, root, rng string, stdout io.Writer) error {
	entries, err := gitLog(ctx, root, rng)
	if err != nil {
		return err
	}
	var bad []string
	checked := 0
	for _, e := range entries {
		if len(e.Parents) > 1 {
			continue
		}
		checked++
		c := commit{SHA: e.SHA, Subject: e.Subject, Body: e.Body}
		c.parse()
		switch {
		case c.Type == "":
			bad = append(bad, fmt.Sprintf("%.7s %q: not <type>(<scope>): <description>", e.SHA, e.Subject))
		case !lintTypes[c.Type]:
			bad = append(bad, fmt.Sprintf("%.7s %q: unknown type %q", e.SHA, e.Subject, c.Type))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d of %d commit subjects are not conventional commits (docs/guides/releasing.md):\n  %s",
			len(bad), checked, strings.Join(bad, "\n  "))
	}
	_, _ = fmt.Fprintf(stdout, "release lint: %d commit subjects ok\n", checked)
	return nil
}

// stage writes the release's install manifests and a chart copy (outDir/chart) that point at
// REGISTRY/patrol-cd/patrol-cd. The chart must already carry v (released from a merged release
// pull request).
func stage(root, outDir, registry string, v version) error {
	chart, err := os.ReadFile(filepath.Join(root, chartDir, "Chart.yaml"))
	if err != nil {
		return err
	}
	cv, err := chartVersion(string(chart))
	if err != nil {
		return err
	}
	if cv != v {
		return fmt.Errorf("chart: Chart.yaml has version %s, not %s", cv, v)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "chart"), 0o755); err != nil {
		return err
	}
	ref := registry + "/" + imageRepository
	if err := stageManifests(filepath.Join(root, manifestsDir), outDir, imageRepository, ref, v); err != nil {
		return err
	}
	return stageChart(filepath.Join(root, chartDir), filepath.Join(outDir, "chart"), registry)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func appendFile(path, s string) error {
	if path == "" {
		return errors.New("-github-output: GITHUB_OUTPUT is not set")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
