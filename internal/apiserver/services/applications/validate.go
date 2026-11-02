package applications

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/appset"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// specInvalid is the upstream InvalidArgument "application spec for <app> is invalid:
// <Type>: <message>;<Type>: <message>".
func specInvalid(app string, issues []project.Issue) error {
	parts := make([]string, 0, len(issues))
	for _, i := range issues {
		parts = append(parts, fmt.Sprintf("%s: %s", i.Type, i.Message))
	}
	return rest.InvalidArgument("application spec for %s is invalid: %s", app, strings.Join(parts, ";"))
}

func invalidSpecIssue(format string, args ...any) project.Issue {
	return project.Issue{Type: v1alpha1.ApplicationConditionInvalidSpecError, Reason: v1alpha1.ReasonInvalidSpec, Message: fmt.Sprintf(format, args...)}
}

// validateAndNormalize checks app before it is stored (upstream validateAndNormalizeApp)
// and normalizes its spec in place. existing is the stored Application (nil on create).
// It returns the project app is validated against (the new one on a project change).
//
//   - name present, multi-source names unique;
//   - a project change needs create on the new and update on the old RBAC object, and the
//     new project must exist and admit the Application's namespace;
//   - the destination must resolve;
//   - with validate: every source is rendered by the repo-server (repository access and
//     path), the hydrator's dry source for hydrator Applications;
//   - project permissions (sources, destination), multi-source references and sync
//     options (internal/project.ValidateApplication);
//   - the managed-by-url annotation is an absolute http(s) URL.
func (s *Service) validateAndNormalize(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, validate bool, existing *v1alpha1.Application) (*v1alpha1.AppProject, error) {
	if app.Name == "" {
		return nil, rest.InvalidArgument("resource name may not be empty")
	}
	if err := project.ValidateSourceNames(app); err != nil {
		return nil, rest.InvalidArgument("%s", err.Error())
	}
	if existing != nil && existing.Spec.GetProject() != app.Spec.GetProject() {
		if err := s.authorizeProjectChange(ctx, app, existing); err != nil {
			return nil, err
		}
		newProj, err := s.appProject(ctx, app)
		if err != nil {
			return nil, err
		}
		proj = newProj
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	kl, _ := s.lookups(st)
	cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, kl)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, rest.InvalidArgument("application destination spec for %s is invalid: %s", app.Name, err.Error())
	}
	if validate {
		issues, err := s.validateSources(ctx, app, proj, cluster, st)
		if err != nil {
			return nil, err
		}
		if len(issues) > 0 {
			return nil, specInvalid(app.Name, issues)
		}
	}
	issues := project.ValidateApplication(ctx, app, proj, kl, project.ValidateOptions{ControlPlaneNamespace: s.controlNS()})
	var invalid []project.Issue
	for _, i := range issues {
		if i.Type == v1alpha1.ApplicationConditionUnknownError {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			s.log().WarnContext(ctx, "applications: error validating project permissions", "application", app.Name, "error", i.Message)
			return nil, rest.Unavailable("error validating project permissions")
		}
		invalid = append(invalid, i)
	}
	if u, ok := app.Annotations[v1alpha1.AnnotationKeyManagedByURL]; ok && u != "" {
		if err := settings.ValidateExternalURL(u); err != nil {
			invalid = append(invalid, invalidSpecIssue("invalid managed-by URL: %v", err))
		}
	}
	if len(invalid) > 0 {
		return nil, specInvalid(app.Name, invalid)
	}
	app.Spec = *appset.NormalizeApplicationSpec(&app.Spec)
	return proj, nil
}

// authorizeProjectChange enforces the rules of moving existing to app's project (TEN-043,
// APP-055): create on the new RBAC object and update on the old one. It does nothing when
// the project is unchanged.
func (s *Service) authorizeProjectChange(ctx context.Context, app, existing *v1alpha1.Application) error {
	if existing == nil || existing.Spec.GetProject() == app.Spec.GetProject() {
		return nil
	}
	if err := s.authorizeApp(ctx, rbac.ActionCreate, app); err != nil {
		return err
	}
	return s.authorizeApp(ctx, rbac.ActionUpdate, existing)
}

// validateSources renders every source with the repo-server (upstream ValidateRepo). Only
// access to the path is checked, not that it holds manifests.
func (s *Service) validateSources(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, cluster *v1alpha1.Cluster, st *settings.Settings) ([]project.Issue, error) {
	rs, err := s.repoServer()
	if err != nil {
		return nil, err
	}
	sources := app.Spec.GetSources()
	in := renderInput{app: app, proj: proj, st: st, hasMultiple: app.Spec.HasMultipleSources()}
	switch {
	case app.Spec.SourceHydrator != nil:
		// The sync branch may not exist yet: check the dry source, without cluster data.
		sources = v1alpha1.ApplicationSources{app.Spec.SourceHydrator.GetDrySource()}
	case cluster != nil:
		in.kubeVersion, in.apiVersions = cluster.Status.ServerVersion, cluster.Status.Info.APIVersions
	}
	// Only sources the project permits are rendered; the others are reported by the
	// permission check (repository credentials must not be used for them).
	for _, src := range sources {
		if !project.IsSourcePermitted(proj, src.RepoURL) {
			return nil, nil
		}
	}
	if in.refs, err = s.refSources(ctx, sources, proj.Name, nil); err != nil {
		return nil, err
	}
	var issues []project.Issue
	for _, src := range sources {
		if src.Ref != "" && src.Path == "" && src.Chart == "" {
			continue // ref-only source: nothing to render
		}
		req, err := s.manifestRequest(ctx, in, src, src.TargetRevision)
		if err != nil {
			var re *rest.Error
			if errors.As(err, &re) && re.Status == 400 {
				return append(issues, invalidSpecIssue("Unable to get repository: %s", re.Detail)), nil
			}
			return nil, err
		}
		req.NoRevisionCache = true
		if _, err := rs.GenerateManifest(ctx, req); err != nil {
			var ae *apiclient.Error
			if !errors.As(err, &ae) {
				return nil, repoServerError("error generating manifests", err)
			}
			switch ae.Code {
			case apiclient.CodeUnavailable, apiclient.CodeDeadlineExceeded, apiclient.CodeUnauthenticated:
				return nil, repoServerError("error generating manifests", err)
			case apiclient.CodePermissionDenied:
				return append(issues, invalidSpecIssue("repository not accessible: %s", ae.Message)), nil
			case apiclient.CodeNotFound:
				if strings.Contains(ae.Message, "matched no files") {
					continue // a runtime condition reported later as ComparisonError
				}
			}
			issues = append(issues, invalidSpecIssue("Unable to generate manifests in %s: %s", src.Path, ae.Message))
		}
	}
	return issues, nil
}
