package cli_test

import (
	"encoding/base64"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/cmdutil"
)

func TestLoginContextsLogout(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})

	// Password login with flags.
	e.Login("admin").Contains(t, "'admin:login' logged in successfully", "Context '"+e.Addr+"' updated")
	cfg := e.ReadConfig()
	if cfg.CurrentContext != e.Addr || cfg.Users[0].AuthToken == "" || !cfg.Servers[0].PlainText {
		t.Fatalf("config: %+v", cfg)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(e.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("config mode %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(e.ConfigPath()))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("config dir mode %v, want 0700", di.Mode().Perm())
		}
	}
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Logged In: true", "Username: admin")

	// Interactive login: username and password from the prompts (stdin is not a TTY, so
	// the password is read as a line; it is never echoed).
	r := e.RunWith(clitest.RunOptions{Stdin: "alice\n" + clitest.Password + "\n"}, "login", e.Addr, "--plaintext", "--name", "alice-ctx").Succeeded(t)
	r.Contains(t, "'alice:login' logged in successfully", "Context 'alice-ctx' updated").NotContains(t, clitest.Password)
	if !strings.Contains(r.Stderr, "Username: ") || !strings.Contains(r.Stderr, "Password: ") {
		t.Fatalf("prompts not on stderr: %q", r.Stderr)
	}
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: alice")

	// Contexts.
	ctxList := e.Run("context").Succeeded(t).Contains(t, "CURRENT  NAME", e.Addr)
	if !regexp.MustCompile(`(?m)^\*\s+alice-ctx\s+` + regexp.QuoteMeta(e.Addr) + `$`).MatchString(ctxList.Stdout) {
		t.Fatalf("current context not marked:\n%s", ctxList.Stdout)
	}
	e.Run("context", e.Addr).Succeeded(t).Contains(t, "Switched to context '"+e.Addr+"'")
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	e.Run("context", e.Addr).Succeeded(t).Contains(t, "Already at context '"+e.Addr+"'")
	e.Run("context", "-").Succeeded(t).Contains(t, "Switched to context 'alice-ctx'")
	e.Run("context", "nope").Failed(t, 1, "Context 'nope' undefined")
	e.Run("--patrol-context", e.Addr, "account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")

	// Logout revokes the session on the server and removes the local token.
	aliceToken := e.ReadConfig().GetToken("alice-ctx")
	if aliceToken == "" {
		t.Fatal("no alice token")
	}
	if _, err := e.APIClient(aliceToken).UserInfo(e.Context()); err != nil {
		t.Fatalf("alice token not usable before logout: %v", err)
	}
	e.Run("logout", "alice-ctx").Succeeded(t).Contains(t, "Logged out from 'alice-ctx'")
	if e.ReadConfig().GetToken("alice-ctx") != "" {
		t.Fatal("token kept after logout")
	}
	if ui, err := e.APIClient(aliceToken).UserInfo(e.Context()); err == nil && ui.LoggedIn {
		t.Fatal("alice session still valid after logout")
	}
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Logged In: false")
	e.Run("logout", "nope").Failed(t, 1, "Context nope does not exist")

	// Optional prompts: enabled through configure, declined on stdin.
	e.Run("configure", "--prompts-enabled").Succeeded(t).Contains(t, "Successfully updated the following configuration settings:", "prompts-enabled: true")
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "logout", e.Addr).Succeeded(t).NotContains(t, "Logged out")
	if e.ReadConfig().GetToken(e.Addr) == "" {
		t.Fatal("declined logout removed the token")
	}
	// --prompts-enabled=false overrides the configuration.
	e.Run("--prompts-enabled=false", "configure", "--prompts-enabled=false").Succeeded(t).Contains(t, "prompts-enabled: false")

	// relogin needs a token in the current context.
	e.Run("relogin").Failed(t, 1, "No authentication token found for context 'alice-ctx'")
	// Relogin with the stored account.
	e.Run("context", e.Addr).Succeeded(t)
	old := e.ReadConfig().GetToken(e.Addr)
	e.Run("relogin", "--password", clitest.Password).Succeeded(t).Contains(t, "Relogging in as 'admin:login'", "Context '"+e.Addr+"' updated")
	if nt := e.ReadConfig().GetToken(e.Addr); nt == "" || nt == old {
		t.Fatal("relogin did not store a new token")
	}

	// Context deletion removes the server entry once unused.
	e.Run("context", "--delete", "alice-ctx").Succeeded(t).Contains(t, "Context 'alice-ctx' deleted")
	e.Run("context").Succeeded(t).NotContains(t, "alice-ctx")
}

func TestLoginFailures(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	e.Run("login", e.Addr, "--plaintext", "--username", "admin", "--password", "wrong-password").Failed(t, 20, "Invalid username or password").NotContains(t, "wrong-password")
	e.Run("login", e.Addr, "--plaintext", "--username", "ci", "--password", "x").Failed(t, 20)
	if _, err := os.Stat(e.ConfigPath()); err == nil {
		t.Fatal("failed logins wrote a configuration")
	}
	// Plain HTTP server without --plaintext: mandatory TLS prompt.
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "login", e.Addr, "--username", "admin", "--password", clitest.Password).
		Failed(t, 1).NotContains(t, "logged in")
	r := e.RunWith(clitest.RunOptions{Stdin: "y\n"}, "login", e.Addr, "--username", "admin", "--password", clitest.Password).Succeeded(t)
	if !strings.Contains(r.Stderr, "WARNING: server is not configured with TLS. Proceed (y/n)?") {
		t.Fatalf("no TLS warning: %q", r.Stderr)
	}
	if !e.ReadConfig().Servers[0].PlainText {
		t.Fatal("plaintext not stored")
	}
	// Bad scheme.
	e.Run("login", "ftp://"+e.Addr, "--username", "admin", "--password", "x").Failed(t, 20, `unsupported scheme "ftp"`)
	// Missing server argument prints help.
	e.Run("login").Failed(t, 1)
}

func TestLoginTLS(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{TLS: true})
	// Untrusted certificate: the mandatory prompt is declined.
	e.RunWith(clitest.RunOptions{Stdin: "n\n"}, "login", e.Addr, "--username", "admin", "--password", clitest.Password).
		Failed(t, 1)
	// Trusted through --server-crt: no prompt, CA stored in the context.
	r := e.Run("login", e.Addr, "--server-crt", e.CAFile(), "--username", "admin", "--password", clitest.Password).Succeeded(t)
	if strings.Contains(r.Stderr, "WARNING") {
		t.Fatalf("unexpected prompt: %q", r.Stderr)
	}
	cfg := e.ReadConfig()
	if cfg.Servers[0].CACertificateAuthorityData == "" || cfg.Servers[0].Insecure || cfg.Servers[0].PlainText {
		t.Fatalf("server entry: %+v", cfg.Servers[0])
	}
	// The stored CA is used by later commands.
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	// Accepting the prompt stores insecure mode for that context.
	e.RunWith(clitest.RunOptions{Stdin: "y\n"}, "login", "localhost:"+strings.Split(e.Addr, ":")[1], "--username", "admin", "--password", clitest.Password).Succeeded(t)
	if s, err := e.ReadConfig().GetServer("localhost:" + strings.Split(e.Addr, ":")[1]); err != nil || !s.Insecure {
		t.Fatalf("insecure not stored: %+v", s)
	}
	// Without the CA and without --insecure, verification fails.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_AUTH_TOKEN": e.LoginToken("admin")}}, "--server", e.Addr, "account", "get-user-info").
		Failed(t, 20, "certificate")
	// --insecure is explicit.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_AUTH_TOKEN": e.LoginToken("admin")}}, "--server", e.Addr, "--insecure", "account", "get-user-info").
		Succeeded(t).Contains(t, "Username: admin")
}

func TestServerAndTokenSources(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	admin := e.LoginToken("admin")
	alice := e.LoginToken("alice")

	// PATROL_SERVER, PATROL_AUTH_TOKEN and PATROL_OPTS, no configuration file.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_SERVER": e.Addr, "PATROL_AUTH_TOKEN": admin, "PATROL_OPTS": "--plaintext"}},
		"account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	// Command-line flags win over PATROL_OPTS.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_OPTS": "--server 127.0.0.1:1 --plaintext"}},
		"--server", e.Addr, "--auth-token", admin, "account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")

	// Argo CD migration aliases, with notes.
	r := e.RunWith(clitest.RunOptions{Env: map[string]string{"ARGOCD_SERVER": e.Addr, "ARGOCD_AUTH_TOKEN": admin, "ARGOCD_OPTS": "--plaintext"}},
		"account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	if !strings.Contains(r.Stderr, "ARGOCD_OPTS") || !strings.Contains(r.Stderr, "ARGOCD_SERVER") {
		t.Fatalf("missing migration notes: %q", r.Stderr)
	}
	// PATROL_* wins over ARGOCD_*.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_SERVER": e.Addr, "PATROL_AUTH_TOKEN": alice, "ARGOCD_AUTH_TOKEN": admin, "PATROL_OPTS": "--plaintext"}},
		"account", "get-user-info").Succeeded(t).Contains(t, "Username: alice")
	// ARGOCD_AUTH_TOKEN is not sent to a server other than ARGOCD_SERVER.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"PATROL_SERVER": e.Addr, "ARGOCD_SERVER": "elsewhere.example.com", "ARGOCD_AUTH_TOKEN": admin, "PATROL_OPTS": "--plaintext"}},
		"account", "get-user-info").Succeeded(t).Contains(t, "Logged In: false")
	// The auth-token key of ARGOCD_OPTS is dropped.
	e.RunWith(clitest.RunOptions{Env: map[string]string{"ARGOCD_OPTS": "--plaintext --server " + e.Addr + " --auth-token " + admin}},
		"account", "get-user-info").Succeeded(t).Contains(t, "Logged In: false")

	// Stored context credentials are only sent to the context's own server.
	e.LoginAs("alice")
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: alice")
	other := "localhost:" + strings.Split(e.Addr, ":")[1]
	e.Run("--server", other, "--plaintext", "account", "get-user-info").Succeeded(t).Contains(t, "Logged In: false")
	// --auth-token overrides the stored token.
	e.Run("--auth-token", admin, "account", "get-user-info").Succeeded(t).Contains(t, "Username: admin")
	// Headers are sent (the server ignores unknown headers).
	e.Run("-H", "X-Test: 1", "--header", "X-Other: 2", "account", "get-user-info").Succeeded(t)

	// A configuration file with loose permissions is refused.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(e.ConfigPath(), 0o644); err != nil {
			t.Fatal(err)
		}
		e.Run("account", "get-user-info").Failed(t, 20, "config file has incorrect permission flags")
		if err := os.Chmod(e.ConfigPath(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSSOLoginAndRefresh(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	e := clitest.New(t, clitest.Options{CM: map[string]string{"oidc.config": idp.OIDCConfig("")}})
	r := e.Run("login", e.Addr, "--plaintext", "--sso", "--sso-port", "0").Succeeded(t)
	r.Contains(t, "'jane@example.com' logged in successfully")
	for _, w := range []string{"Performing authorization_code flow login: " + idp.URL + "/auth?", "Opening system default browser for authentication", "Authentication successful"} {
		if !strings.Contains(r.Stderr, w) {
			t.Fatalf("stderr lacks %q:\n%s", w, r.Stderr)
		}
	}
	cfg := e.ReadConfig()
	u, err := cfg.GetUser(e.Addr)
	if err != nil || u.AuthToken == "" || u.RefreshToken == "" {
		t.Fatalf("SSO user entry: %+v", u)
	}
	r.NotContains(t, u.AuthToken, u.RefreshToken)
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: jane@example.com", "Issuer: ", "Groups: devs")

	// Expired session: the CLI refreshes at the IdP and exchanges the new ID token.
	later := clitest.RunOptions{Factory: func(f *cmdutil.Factory) { f.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } }}
	e.RunWith(later, "account", "get-user-info").Succeeded(t).Contains(t, "Username: jane@example.com")
	if idp.Refreshes() != 1 {
		t.Fatalf("refreshes = %d, want 1", idp.Refreshes())
	}
	nu, err := e.ReadConfig().GetUser(e.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if nu.AuthToken == u.AuthToken || nu.RefreshToken == u.RefreshToken {
		t.Fatal("renewed SSO tokens not stored")
	}

	// relogin runs the SSO flow again.
	e.Run("relogin", "--sso-port", "0").Succeeded(t).Contains(t, "Reinitiating SSO login", "Context '"+e.Addr+"' updated")
	if idp.Authorizations() != 2 {
		t.Fatalf("authorizations = %d, want 2", idp.Authorizations())
	}

	// A server without SSO refuses --sso.
	e2 := clitest.New(t, clitest.Options{})
	e2.Run("login", e2.Addr, "--plaintext", "--sso", "--sso-port", "0").Failed(t, 20, "is not configured with SSO")
}

// TestSSOKeySetRefused pins what docs/guides/installation.md says the commands show when an
// SSO session has expired and go-jose refuses the IdP's key set.
func TestSSOKeySetRefused(t *testing.T) {
	t.Parallel()
	const refused = "go-jose/go-jose: invalid RSA key, modulus too large"
	refusedKey := map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "unused", "e": "AQAB",
		"n": base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 4096).Bytes()), // 4097 bits
	}
	idp := clitest.NewFakeIdP(t)
	e := clitest.New(t, clitest.Options{CM: map[string]string{"oidc.config": idp.OIDCConfig("")}})
	e.Run("login", e.Addr, "--plaintext", "--sso", "--sso-port", "0").Succeeded(t)
	later := clitest.RunOptions{Factory: func(f *cmdutil.Factory) { f.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } }}

	idp.SetExtraJWK(refusedKey)
	e.RunWith(later, "account", "get-user-info").Failed(t, 20, refused, "run 'patrol relogin' to re-authenticate")
	// That renewal redeemed the refresh token, and this IdP rotates them.
	e.RunWith(later, "account", "get-user-info").Failed(t, 20, "invalid_grant", "run 'patrol relogin' to re-authenticate")
	e.Run("relogin", "--sso-port", "0").Failed(t, 20, refused)

	idp.SetExtraJWK(nil)
	e.RunWith(later, "account", "get-user-info").Failed(t, 20, "invalid_grant")
	e.Run("relogin", "--sso-port", "0").Succeeded(t)
	e.Run("account", "get-user-info").Succeeded(t).Contains(t, "Username: jane@example.com")

	idp.SetExtraJWK(refusedKey)
	r := e.RunWith(later, "account", "session-token").Failed(t, 1, "Refresh token is invalid or expired")
	r.NotContains(t, refused)
}

func TestNoSecretsInLogs(t *testing.T) {
	t.Parallel()
	e := clitest.New(t, clitest.Options{})
	debug := []string{"--loglevel", "debug", "--logformat", "json"}
	r := e.Run(append(debug, "login", e.Addr, "--plaintext", "--username", "admin", "--password", clitest.Password)...).Succeeded(t)
	r.NotContains(t, clitest.Password)
	tok := e.ReadConfig().GetToken(e.Addr)
	var all strings.Builder
	for _, args := range [][]string{
		{"account", "get-user-info"},
		{"proj", "list"},
		{"repo", "add", "https://git.example.com/secret.git", "--username", "u", "--password", "repo-pass-123"},
		{"account", "update-password", "--current-password", clitest.Password, "--new-password", "New-Pass-456!"},
		{"proj", "get", "missing"},
	} {
		res := e.Run(append(debug, args...)...)
		all.WriteString(res.Stdout + res.Stderr)
	}
	out := all.String() + e.ServerLog.String()
	for _, secret := range []string{tok, clitest.Password, "repo-pass-123", "New-Pass-456!", clitest.ServerKey} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked into CLI or server logs", secret[:4]+"...")
		}
	}
	if !strings.Contains(all.String(), `"level":"DEBUG"`) {
		t.Fatalf("debug logging not enabled:\n%s", all.String())
	}
}
