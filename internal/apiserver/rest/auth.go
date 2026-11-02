package rest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// TokenVerifier verifies Patrol tokens; *auth.TokenService implements it.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (*auth.Principal, error)
	RenewIfExpiring(ctx context.Context, p *auth.Principal) (auth.Issued, bool, error)
}

// AuthPolicy is the authentication configuration read from settings for each request.
type AuthPolicy struct {
	// AnonymousEnabled is users.anonymous.enabled.
	AnonymousEnabled bool
	// Scopes are the claims evaluated as RBAC groups (patrol-rbac-cm "scopes").
	Scopes []string
}

// Authenticator establishes the caller's Identity for every mounted route.
type Authenticator struct {
	// Verifier verifies tokens. Required unless DisableAuth is set.
	Verifier TokenVerifier
	// Policy returns the current authentication settings; nil means AuthPolicy{} with the
	// default scopes.
	Policy func() AuthPolicy
	// Cookie returns the attributes of the session cookies on the response to a request (path
	// and Secure attribute); nil means auth.CookieOptions{}.
	Cookie func(*http.Request) auth.CookieOptions
	// DisableAuth skips authentication (upstream --disable-auth); requests proceed with
	// the anonymous identity and RBAC is not enforced.
	DisableAuth bool
	Logger      *slog.Logger
}

func (a *Authenticator) cookie(r *http.Request) auth.CookieOptions {
	if a.Cookie == nil {
		return auth.CookieOptions{}
	}
	return a.Cookie(r)
}

func (a *Authenticator) policy() AuthPolicy {
	if a.Policy == nil {
		return AuthPolicy{Scopes: rbac.DefaultScopes()}
	}
	return a.Policy()
}

func (a *Authenticator) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// authenticate returns the caller's identity for a route with the given access mode, or
// an error to return to the client (401 or 503). renew allows replacing a login session
// close to expiry on this response (false on routes that end the session).
func (a *Authenticator) authenticate(w http.ResponseWriter, r *http.Request, mode AccessMode, renew bool) (*Identity, func(context.Context) error, error) {
	if a == nil {
		return &Identity{AuthDisabled: true}, nil, nil
	}
	ctx := r.Context()
	pol := a.policy()
	raw, src, tokErr := tokenFrom(r)

	if a.DisableAuth {
		id := &Identity{AuthDisabled: true, Source: src}
		if raw != "" && a.Verifier != nil {
			if p, err := a.Verifier.Verify(ctx, raw); err == nil {
				id.Principal, id.token = p, raw
				id.Subjects = subjectsFor(p, pol.Scopes)
			}
		}
		return id, nil, nil
	}

	if tokErr != nil && !errors.Is(tokErr, auth.ErrNoToken) {
		// A malformed cookie is an invalid token.
		tokErr = errors.Join(auth.ErrInvalidToken, tokErr)
	}
	if raw == "" {
		if errors.Is(tokErr, auth.ErrInvalidToken) && src == TokenSourceCookie {
			auth.ClearSessionCookie(w, r, a.cookie(r))
		}
		return a.unauthenticated(mode, pol, src, tokErr)
	}

	p, err := a.Verifier.Verify(ctx, raw)
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidToken) {
			// Revocation store or account lookup failed: never accept the token.
			if mode == AccessPublic {
				return &Identity{Source: src, AuthError: err, Anonymous: pol.AnonymousEnabled}, nil, nil
			}
			a.log().LogAttrs(ctx, slog.LevelError, "token verification unavailable",
				slog.String("requestId", RequestID(ctx)), slog.Any("error", err))
			return nil, nil, Unavailable("authentication is temporarily unavailable").WithCause(err)
		}
		if src == TokenSourceCookie {
			auth.ClearSessionCookie(w, r, a.cookie(r))
		}
		return a.unauthenticated(mode, pol, src, err)
	}

	id := &Identity{Principal: p, Source: src, Subjects: subjectsFor(p, pol.Scopes), token: raw}
	if renew {
		a.renew(w, r, p, src)
	}
	reverify := func(ctx context.Context) error {
		if _, err := a.Verifier.Verify(ctx, raw); err != nil {
			if errors.Is(err, auth.ErrInvalidToken) {
				return Unauthenticated("invalid session: %s", tokenProblem(err)).WithCause(err)
			}
			return Unavailable("authentication is temporarily unavailable").WithCause(err)
		}
		return nil
	}
	return id, reverify, nil
}

func (a *Authenticator) unauthenticated(mode AccessMode, pol AuthPolicy, src TokenSource, cause error) (*Identity, func(context.Context) error, error) {
	if cause != nil && errors.Is(cause, auth.ErrNoToken) {
		cause = nil
	}
	if mode == AccessPublic || pol.AnonymousEnabled {
		id := &Identity{Source: src, AuthError: cause, Anonymous: pol.AnonymousEnabled}
		var reverify func(context.Context) error
		if id.Anonymous {
			reverify = func(context.Context) error {
				if !a.policy().AnonymousEnabled {
					return Unauthenticated("no session information")
				}
				return nil
			}
		}
		return id, reverify, nil
	}
	if cause == nil {
		return nil, nil, Unauthenticated("no session information")
	}
	return nil, nil, Unauthenticated("invalid session: %s", tokenProblem(cause)).WithCause(cause)
}

// renew replaces a login session close to expiry: browsers (cookie) get a refreshed
// cookie, API clients (bearer) the new token in apiclient.RenewTokenHeader.
func (a *Authenticator) renew(w http.ResponseWriter, r *http.Request, p *auth.Principal, src TokenSource) {
	iss, renewed, err := a.Verifier.RenewIfExpiring(r.Context(), p)
	if err != nil {
		a.log().LogAttrs(r.Context(), slog.LevelWarn, "session renewal failed",
			slog.String("requestId", RequestID(r.Context())), slog.Any("error", err))
		return
	}
	if !renewed {
		return
	}
	if src == TokenSourceHeader {
		w.Header().Set(apiclient.RenewTokenHeader, iss.Token)
		return
	}
	if err := auth.SetSessionCookie(w, r, iss.Token, a.cookie(r)); err != nil {
		a.log().LogAttrs(r.Context(), slog.LevelWarn, "set renewed session cookie failed",
			slog.String("requestId", RequestID(r.Context())), slog.Any("error", err))
	}
}

// tokenProblem returns a client-safe description of a token verification failure.
func tokenProblem(err error) string {
	for _, c := range []error{
		auth.ErrTokenExpired, auth.ErrTokenRevoked, auth.ErrSessionInvalidated,
		auth.ErrAccountDisabled, auth.ErrMissingCapability, auth.ErrAccountNotFound,
		auth.ErrTokenTooLarge, auth.ErrInvalidCookie,
	} {
		if errors.Is(err, c) {
			return c.Error()
		}
	}
	return "invalid token"
}

// tokenFrom extracts the token and tells where it came from: a well-formed
// "Authorization: Bearer <jwt>" header wins, otherwise the patrol.token cookie(s).
func tokenFrom(r *http.Request) (string, TokenSource, error) {
	tok, err := auth.TokenFromRequest(r)
	if err != nil {
		src := TokenSourceNone
		if _, cerr := r.Cookie(auth.SessionCookieName); cerr == nil {
			src = TokenSourceCookie
		}
		return "", src, err
	}
	for _, h := range r.Header.Values("Authorization") {
		scheme, v, ok := strings.Cut(strings.TrimSpace(h), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(v) == tok {
			return tok, TokenSourceHeader, nil
		}
	}
	return tok, TokenSourceCookie, nil
}

// subjectsFor maps a verified principal to RBAC subjects. Project-role subjects come only
// from verified project tokens, never from claims.
func subjectsFor(p *auth.Principal, scopes []string) rbac.Subjects {
	switch p.Kind {
	case auth.KindProject:
		return rbac.Subjects{ProjectRole: &rbac.ProjectRole{Project: p.Project, Role: p.Role}}
	case auth.KindLogin, auth.KindAPIKey:
		return rbac.Subjects{User: p.Subject}
	}
	s := rbac.SubjectsFromClaims(p.Claims, scopes)
	if s.User == "" {
		s.User = p.Subject
	}
	if len(s.Groups) == 0 && slices.Contains(scopes, "groups") && len(p.Groups) > 0 {
		s.Groups = slices.Clone(p.Groups)
	}
	return s
}
