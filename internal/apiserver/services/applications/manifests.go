package applications

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// renderInput is what the manifest requests of one Application share.
type renderInput struct {
	app  *v1alpha1.Application
	proj *v1alpha1.AppProject
	st   *settings.Settings
	// kubeVersion and apiVersions are the destination's; empty, they are left to the render tools.
	kubeVersion string
	apiVersions []string
	refs        map[string]apiclient.RefTarget
	hasMultiple bool
	noCache     bool
}

func isOCI(repoURL string) bool {
	return len(repoURL) >= 6 && strings.EqualFold(repoURL[:6], "oci://")
}

func kustomizeOptions(st *settings.Settings) apiclient.KustomizeOptions {
	k := st.Kustomize()
	out := apiclient.KustomizeOptions{BuildOptions: k.BuildOptions}
	for _, v := range k.Versions {
		if out.Versions == nil {
			out.Versions = map[string]string{}
			out.VersionBuildOptions = map[string]string{}
		}
		out.Versions[v.Name] = v.Path
		if v.BuildOptions != "" {
			out.VersionBuildOptions[v.Name] = v.BuildOptions
		}
	}
	return out
}

func enabledSourceTypes(st *settings.Settings) map[string]bool {
	out := map[string]bool{}
	for t, on := range st.EnabledSourceTypes() {
		out[string(t)] = on
	}
	return out
}

// repositoryError maps a repository resolution failure: configuration problems (missing
// Secret or key, invalid credentials) are 400 with the resolver's message, which names
// objects but never secret values; an unsynced cache is 503.
func repositoryError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, repository.ErrNotSynced) {
		return rest.Unavailable("repository information is not synchronized yet").WithCause(err)
	}
	if repository.IsConfigurationError(err) {
		return rest.InvalidArgument("%s", err.Error())
	}
	return rest.Unavailable("error getting repository").WithCause(err)
}

// repoServerError maps a repo-server failure: an unreachable or overloaded repo-server is
// 503; a request it refuses (generation errors, unknown revisions, access denied by the
// repository) is 400 with its message, which the repo-server sanitizes (no credentials,
// no cache paths).
func repoServerError(prefix string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ae *apiclient.Error
	if !errors.As(err, &ae) {
		return rest.Unavailable("%s: the repo-server is unavailable", prefix).WithCause(err)
	}
	switch ae.Code {
	case apiclient.CodeUnavailable, apiclient.CodeDeadlineExceeded, apiclient.CodeUnauthenticated:
		return rest.Unavailable("%s: the repo-server is unavailable", prefix).WithCause(err)
	}
	return rest.InvalidArgument("%s: %s", prefix, ae.Message)
}

// refSources maps "$<ref>" to the referenced sources of a multi-source Application
// (upstream GetRefSources); revisions (per source, may be shorter) override
// targetRevision.
func (s *Service) refSources(ctx context.Context, sources []v1alpha1.ApplicationSource, projectName string, revisions []string) (map[string]apiclient.RefTarget, error) {
	if len(sources) < 2 {
		return nil, nil
	}
	if err := project.ValidateRefSources(sources); err != nil {
		return nil, rest.InvalidArgument("failed to get ref sources: %v", err)
	}
	repos, err := s.repositories()
	if err != nil {
		return nil, err
	}
	out := map[string]apiclient.RefTarget{}
	for i, src := range sources {
		if src.Ref == "" {
			continue
		}
		repo, err := repos.GetRepository(ctx, src.RepoURL, projectName)
		if err != nil {
			return nil, repositoryError(err)
		}
		rev := src.TargetRevision
		if i < len(revisions) && revisions[i] != "" {
			rev = revisions[i]
		}
		out["$"+src.Ref] = apiclient.RefTarget{Repo: *repo, TargetRevision: rev, Chart: src.Chart}
	}
	return out, nil
}

// manifestRequest builds the repo-server request rendering source at revision, with the
// repository credentials and Helm dependencies the project may use.
func (s *Service) manifestRequest(ctx context.Context, in renderInput, source v1alpha1.ApplicationSource, revision string) (*apiclient.ManifestRequest, error) {
	repos, err := s.repositories()
	if err != nil {
		return nil, err
	}
	repo, err := repos.GetRepository(ctx, source.RepoURL, in.proj.Name)
	if err != nil {
		return nil, repositoryError(err)
	}
	deps, creds, err := repos.ListDependencyRepositories(ctx, in.proj, isOCI(source.RepoURL))
	if err != nil {
		return nil, repositoryError(err)
	}
	si, _ := project.EffectiveSourceIntegrity(in.proj)
	req := &apiclient.ManifestRequest{
		Repo:                  *repo,
		Revision:              revision,
		NoCache:               in.noCache,
		AppName:               tracking.InstanceName(in.app.Name, in.app.Namespace, s.controlNS()),
		AppNamespace:          in.app.Namespace,
		Namespace:             in.app.Spec.Destination.Namespace,
		Source:                source,
		Repos:                 deps,
		HelmRepoCreds:         creds,
		KustomizeOptions:      kustomizeOptions(in.st),
		KubeVersion:           in.kubeVersion,
		APIVersions:           in.apiVersions,
		HelmOptions:           apiclient.HelmOptions{ValuesFileSchemes: in.st.HelmValuesFileSchemes()},
		InstanceLabelKey:      in.st.InstanceLabelKey(),
		TrackingMethod:        string(in.st.TrackingMethod()),
		InstallationID:        in.st.InstallationID(),
		EnabledSourceTypes:    enabledSourceTypes(in.st),
		HasMultipleSources:    in.hasMultiple,
		RefSources:            in.refs,
		ProjectName:           in.proj.Name,
		ProjectSourceRepos:    in.proj.Spec.SourceRepos,
		ManifestGeneratePaths: in.app.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths],
		SourceIntegrity:       si,
	}
	return req, nil
}

// destinationVersions returns the Kubernetes version and API versions a preview renders with, so
// that it gives what the controller applies. They are the status's, which the controller keeps. A
// status without them, as on the in-cluster cluster, which has no object to hold them, or on a
// Cluster with no Applications yet, sends the question to the destination, with the credentials the
// server holds for the cluster and no service account of the project, as the controller's cache
// does (in core mode, the caller's kubeconfig). A destination the project does not permit is not
// contacted, nor is one the server has no credentials for: the render then has what the status holds.
func (s *Service) destinationVersions(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, st *settings.Settings, cluster *v1alpha1.Cluster) (string, []string, error) {
	version, apis := cluster.Status.ServerVersion, cluster.Status.Info.APIVersions
	if (version != "" && len(apis) > 0) || s.d.Resources.Clusters == nil {
		return version, apis, nil
	}
	kl, _ := s.lookups(st)
	permitted, err := project.IsDestinationPermitted(ctx, proj, project.DestinationOf(cluster, app.Spec.Destination.Namespace), kl)
	if err != nil {
		return "", nil, rest.Unavailable("error checking the destination cluster").WithCause(err)
	}
	if !permitted {
		return version, apis, nil
	}
	target, err := s.appClusterConfig(ctx, app, proj, st, false)
	var none noCredentials
	if errors.As(err, &none) {
		return version, apis, nil
	}
	if err != nil {
		return "", nil, err
	}
	version, resources, unavailable, err := cache.DiscoverAPIResources(ctx, target.config)
	if err != nil {
		return "", nil, s.discoveryError(ctx, err)
	}
	for _, u := range unavailable {
		s.log().WarnContext(ctx, "applications: the destination did not describe an API version a preview renders without",
			"server", target.cluster.Spec.Server, "group", u.GroupKind.Group, "version", u.Version, "error", u.Message)
	}
	return version, cache.APIVersionStrings(resources), nil
}

// discoveryError is the error of a preview whose destination did not give its versions. The caller
// asked about the Application, so a status the destination answers with, a 403 or a 404, does not
// become the answer to that question: the preview is unavailable.
func (s *Service) discoveryError(ctx context.Context, err error) error {
	err = s.destinationError(ctx, "error getting the destination's versions", err)
	var e *rest.Error
	if errors.As(err, &e) && e.Status != http.StatusServiceUnavailable {
		return rest.Unavailable("%s", e.Detail).WithCause(e.Cause)
	}
	return err
}

// checkSourcesPermitted refuses to render repositories the project does not permit (an
// Application applied with kubectl is not validated): rendering would hand repository
// content, fetched with the project's credentials, to anyone who may get the Application.
func checkSourcesPermitted(proj *v1alpha1.AppProject, sources []v1alpha1.ApplicationSource) error {
	for _, src := range sources {
		if !project.IsSourcePermitted(proj, src.RepoURL) {
			return rest.PermissionDenied("application repo %s is not permitted in project '%s'", redactURL(src.RepoURL), proj.Name)
		}
	}
	return nil
}

var urlUserinfo = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@]*@`)

// redactURL removes user information from a repository URL.
func redactURL(u string) string { return urlUserinfo.ReplaceAllString(u, "${1}") }

// sensitiveAnnotations returns resource.sensitive.mask.annotations as a set.
func sensitiveAnnotations(st *settings.Settings) map[string]bool {
	out := map[string]bool{}
	if st == nil {
		return out
	}
	for _, a := range st.SensitiveMaskAnnotations() {
		out[a] = true
	}
	return out
}

func isCoreSecret(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()
	return gvk.Group == "" && gvk.Kind == "Secret"
}

// redactedManifest replaces a manifest that cannot be parsed (and therefore cannot be
// masked) in responses.
const redactedManifest = "# manifest redacted: it could not be parsed to mask Secret data"

// maskManifest returns text (one or more YAML or JSON documents) with the data of core
// Secrets and the sensitive annotations masked (internal/diff.HideSecretData). Documents
// that are not Secrets are returned unchanged; a text that does not parse is replaced
// by redactedManifest (fail closed).
func maskManifest(text string, hide map[string]bool) string {
	r := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(text)))
	var docs []string
	changed := false
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return redactedManifest
		}
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := yaml.Unmarshal(trimmed, &obj); err != nil {
			return redactedManifest
		}
		u := &unstructured.Unstructured{Object: obj}
		if len(obj) == 0 || !isCoreSecret(u) {
			docs = append(docs, string(doc))
			continue
		}
		masked, _, err := diff.HideSecretData(u, nil, hide)
		if err != nil {
			return redactedManifest
		}
		b, err := json.Marshal(masked.Object)
		if err != nil {
			return redactedManifest
		}
		docs = append(docs, string(b))
		changed = true
	}
	if !changed {
		return text
	}
	return strings.Join(docs, "\n---\n")
}

// maskManifests masks every manifest of a rendering result.
func maskManifests(manifests []string, hide map[string]bool) []string {
	out := make([]string, len(manifests))
	for i, m := range manifests {
		out[i] = maskManifest(m, hide)
	}
	return out
}

// getManifests serves GET /applications/{name}/manifests: the target manifests rendered
// by the repo-server at the requested revision(s) with the Application's settings.
func (s *Service) getManifests(ctx context.Context, q *pubapi.ApplicationManifestQuery) (*pubapi.ApplicationManifestResponse, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: q.Name, namespace: q.AppNamespace, project: q.Project})
	if err != nil {
		return nil, err
	}
	rs, err := s.repoServer()
	if err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	var sources []v1alpha1.ApplicationSource
	if app.Spec.HasMultipleSources() {
		if q.Revision != "" {
			return nil, rest.InvalidArgument("revision cannot be used with a multi-source application; use revisions and sourcePositions")
		}
		if len(q.Revisions) != len(q.SourcePositions) {
			return nil, rest.InvalidArgument("revisions and sourcePositions must have the same number of values")
		}
		sources = app.Spec.DeepCopy().Sources
		for i, pos := range q.SourcePositions {
			if pos <= 0 || pos > int64(len(sources)) {
				return nil, rest.InvalidArgument("source position is out of range")
			}
			sources[pos-1].TargetRevision = q.Revisions[i]
		}
	} else {
		if len(q.Revisions) > 0 || len(q.SourcePositions) > 0 {
			return nil, rest.InvalidArgument("revisions and sourcePositions can only be used with a multi-source application")
		}
		var source v1alpha1.ApplicationSource
		switch {
		case app.Spec.SourceHydrator != nil:
			source = app.Spec.SourceHydrator.GetDrySource()
		case app.Spec.Source != nil:
			source = *app.Spec.Source.DeepCopy()
		default:
			return nil, rest.InvalidArgument("application %s has no source", app.Name)
		}
		if q.Revision != "" {
			source.TargetRevision = q.Revision
		}
		sources = []v1alpha1.ApplicationSource{source}
	}
	if err := checkSourcesPermitted(proj, sources); err != nil {
		return nil, err
	}
	kl, _ := s.lookups(st)
	cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, kl)
	if err != nil {
		return nil, rest.InvalidArgument("error getting application cluster config: %s", err.Error())
	}
	refs, err := s.refSources(ctx, sources, proj.Name, nil)
	if err != nil {
		return nil, err
	}
	kubeVersion, apiVersions, err := s.destinationVersions(ctx, app, proj, st, cluster)
	if err != nil {
		return nil, err
	}
	in := renderInput{app: app, proj: proj, st: st, kubeVersion: kubeVersion, apiVersions: apiVersions, refs: refs, hasMultiple: app.Spec.HasMultipleSources(), noCache: q.NoCache}
	hide := sensitiveAnnotations(st)
	out := &pubapi.ApplicationManifestResponse{Manifests: []string{}, Namespace: app.Spec.Destination.Namespace, Server: cluster.Spec.Server}
	for _, src := range sources {
		if in.hasMultiple && src.Ref != "" && src.Path == "" && src.Chart == "" {
			out.Revisions = append(out.Revisions, src.TargetRevision)
			out.SourceTypes = append(out.SourceTypes, "")
			continue
		}
		req, err := s.manifestRequest(ctx, in, src, src.TargetRevision)
		if err != nil {
			return nil, err
		}
		resp, err := rs.GenerateManifest(ctx, req)
		if err != nil {
			return nil, repoServerError("error generating manifests", err)
		}
		out.Manifests = append(out.Manifests, maskManifests(resp.Manifests, hide)...)
		if in.hasMultiple {
			out.Revisions = append(out.Revisions, resp.Revision)
			out.SourceTypes = append(out.SourceTypes, resp.SourceType)
		} else {
			out.Revision, out.SourceType = resp.Revision, resp.SourceType
		}
	}
	return out, nil
}

// gzipMagic starts every gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// manifestsWithFiles serves POST /applications/{name}/manifests-with-files: the body is a
// gzipped tar of a local source directory (bounded by Config.MaxLocalFilesBytes) rendered
// by the repo-server with the Application's settings. Rendering local content needs
// applications, override (like a local sync). The repo-server extracts the archive safely
// (entry and size limits, no links out of the tree).
func (s *Service) manifestsWithFiles(w http.ResponseWriter, r *http.Request) {
	out, err := s.renderLocalFiles(r.Context(), w, r)
	if err != nil {
		rest.WriteError(w, r, err)
		return
	}
	rest.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) renderLocalFiles(ctx context.Context, w http.ResponseWriter, r *http.Request) (*pubapi.ApplicationManifestResponse, error) {
	q := r.URL.Query()
	req := appRequest{name: r.PathValue("name"), namespace: q.Get("appNamespace"), project: q.Get("project")}
	app, proj, err := s.getApp(ctx, rbac.ActionGet, req)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeApp(ctx, rbac.ActionOverride, app); err != nil {
		return nil, err
	}
	auditApp(ctx, app, rbac.ActionOverride, s.rbacObject(app), "", "")
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		(mt != pubapi.ContentTypeGzip && mt != "application/x-gzip" && mt != "application/octet-stream") {
		return nil, &rest.Error{Status: http.StatusUnsupportedMediaType, Code: pubapi.CodeInvalidArgument,
			Detail: "the request body must be a gzipped tar archive (Content-Type application/gzip)"}
	}
	if app.Spec.HasMultipleSources() {
		return nil, rest.InvalidArgument("manifests with local files are only supported for single-source applications")
	}
	rs, err := s.repoServer()
	if err != nil {
		return nil, err
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.d.Config.MaxLocalFilesBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &rest.Error{Status: http.StatusRequestEntityTooLarge, Code: pubapi.CodeResourceExhausted, Detail: "request body too large"}
		}
		return nil, rest.InvalidArgument("error reading the request body")
	}
	if !bytes.HasPrefix(body, gzipMagic) {
		return nil, rest.InvalidArgument("the request body is not a gzip stream")
	}
	if want := q.Get("checksum"); want != "" {
		sum := sha256.Sum256(body)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(want)), []byte(hex.EncodeToString(sum[:]))) != 1 {
			return nil, rest.InvalidArgument("checksum mismatch: the body's SHA-256 is not %q", want)
		}
	}
	var source v1alpha1.ApplicationSource
	switch {
	case app.Spec.SourceHydrator != nil:
		source = app.Spec.SourceHydrator.GetDrySource()
	case app.Spec.Source != nil:
		source = *app.Spec.Source.DeepCopy()
	default:
		return nil, rest.InvalidArgument("application %s has no source", app.Name)
	}
	if err := checkSourcesPermitted(proj, []v1alpha1.ApplicationSource{source}); err != nil {
		return nil, err
	}
	kl, _ := s.lookups(st)
	cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, kl)
	if err != nil {
		return nil, rest.InvalidArgument("error getting application cluster config: %s", err.Error())
	}
	kubeVersion, apiVersions, err := s.destinationVersions(ctx, app, proj, st, cluster)
	if err != nil {
		return nil, err
	}
	in := renderInput{app: app, proj: proj, st: st, kubeVersion: kubeVersion, apiVersions: apiVersions}
	mreq, err := s.manifestRequest(ctx, in, source, source.TargetRevision)
	if err != nil {
		return nil, err
	}
	resp, err := rs.GenerateManifestWithFiles(ctx, mreq, bytes.NewReader(body))
	if err != nil {
		return nil, repoServerError("error generating manifests", err)
	}
	return &pubapi.ApplicationManifestResponse{
		Manifests:  maskManifests(resp.Manifests, sensitiveAnnotations(st)),
		Revision:   resp.Revision,
		SourceType: resp.SourceType,
		Namespace:  app.Spec.Destination.Namespace,
		Server:     cluster.Spec.Server,
	}, nil
}
