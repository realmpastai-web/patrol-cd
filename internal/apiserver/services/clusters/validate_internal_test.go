package clusters

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestNormalizeServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, server string
		allowLL      bool
		want         string
		wantErr      string
	}{
		{"https", "https://kube.example.com:6443/", false, "https://kube.example.com:6443", ""},
		{"http and path", "http://10.0.0.1/k8s/clusters/c-1//", false, "http://10.0.0.1/k8s/clusters/c-1", ""},
		{"loopback allowed", "https://127.0.0.1:6443", false, "https://127.0.0.1:6443", ""},
		{"ipv6", "https://[fd00::1]:6443", false, "https://[fd00::1]:6443", ""},
		{"empty", "", false, "", "server is required"},
		{"scheme", "ftp://kube", false, "", `server must be an http(s) URL, not "ftp"`},
		{"relative", "kube.example.com", false, "", "is not a valid URL"},
		{"no host", "https://", false, "", "has no host"},
		{"userinfo", "https://user:pw@kube.example.com", false, "", "must not contain credentials"},
		{"query", "https://kube.example.com?x=1", false, "", "query or fragment"},
		{"fragment", "https://kube.example.com#x", false, "", "is not a valid URL"},
		{"whitespace", "https://kube example.com", false, "", "whitespace"},
		{"metadata ip", "http://169.254.169.254", false, "", "link-local, multicast or metadata"},
		{"metadata ip allowed", "http://169.254.169.254", true, "http://169.254.169.254", ""},
		{"metadata host", "http://metadata.google.internal.", false, "", "link-local, multicast or metadata"},
		{"ipv6 link-local", "https://[fe80::1]:6443", false, "", "link-local, multicast or metadata"},
		{"aws imds v6", "http://[fd00:ec2::254]", false, "", "link-local, multicast or metadata"},
		{"unspecified", "https://0.0.0.0:6443", false, "", "link-local, multicast or metadata"},
		{"too long", "https://a.example/" + strings.Repeat("x", 2048), false, "", "at most 2048"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeServer(tc.server, tc.allowLL)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Equal(t, http.StatusBadRequest, rest.ProblemFor(err).Status)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidateInput(t *testing.T) {
	t.Parallel()
	const server = "https://kube.example.com"
	tests := []struct {
		name    string
		in      apiclient.ClusterInput
		wantErr string
		check   func(t *testing.T, d *desired)
	}{
		{name: "defaults", in: apiclient.ClusterInput{Namespaces: []string{" a ", "", "b"}, Shard: new(int64(-1)), Config: apiclient.ClusterConfigInput{BearerToken: "tok", TLSClientConfig: apiclient.ClusterTLSClientConfigInput{Insecure: true, CAData: []byte("ca")}}},
			check: func(t *testing.T, d *desired) {
				assert.Equal(t, server, d.spec.Name, "name defaults to the server")
				assert.Equal(t, []string{"a", "b"}, d.spec.Namespaces)
				assert.Nil(t, d.spec.Shard, "-1 means unset")
				assert.True(t, d.spec.Config.TLSClientConfig.Insecure)
				assert.Nil(t, d.spec.Config.CredentialsRef)
				assert.Equal(t, map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("tok"), v1alpha1.ClusterSecretKeyCAData: []byte("ca")}, d.creds)
			}},
		{name: "shard kept", in: apiclient.ClusterInput{Shard: new(int64(2))}, check: func(t *testing.T, d *desired) { assert.Equal(t, int64(2), *d.spec.Shard) }},
		{name: "proxy ok", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{ProxyURL: "socks5://proxy:1080"}}, check: func(t *testing.T, d *desired) {
			assert.Equal(t, "socks5://proxy:1080", d.spec.Config.ProxyURL)
		}},
		{name: "proxy scheme", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{ProxyURL: "socks5h://proxy:1080"}}, wantErr: `unsupported scheme "socks5h", must be http, https, or socks5`},
		{name: "proxy credentials", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{ProxyURL: "http://u:p@proxy:3128"}}, wantErr: "proxyUrl must not contain credentials"},
		{name: "proxy metadata", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{ProxyURL: "http://169.254.169.254:80"}}, wantErr: "link-local"},
		{name: "cert without key", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{TLSClientConfig: apiclient.ClusterTLSClientConfigInput{CertData: []byte("c")}}}, wantErr: "must be set together"},
		{name: "exec without command", in: apiclient.ClusterInput{Config: apiclient.ClusterConfigInput{ExecProviderConfig: &v1alpha1.ExecProviderConfig{Args: []string{"x"}}}}, wantErr: "execProviderConfig.command is required"},
		{name: "bad namespace", in: apiclient.ClusterInput{Namespaces: []string{"Bad_NS"}}, wantErr: `namespace "Bad_NS" is invalid`},
		{name: "duplicate namespace", in: apiclient.ClusterInput{Namespaces: []string{"a", "a"}}, wantErr: "listed twice"},
		{name: "bad project", in: apiclient.ClusterInput{Project: "Team A"}, wantErr: `project "Team A" is invalid`},
		{name: "last-applied", in: apiclient.ClusterInput{Annotations: map[string]string{lastAppliedAnnotation: "{}"}}, wantErr: "annotation kubectl.kubernetes.io/last-applied-configuration cannot be set"},
		{name: "bad label", in: apiclient.ClusterInput{Labels: map[string]string{"bad key!": "v"}}, wantErr: "labels"},
		{name: "control char name", in: apiclient.ClusterInput{Name: "a\nb"}, wantErr: "control characters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := validateInput(&tc.in, server, false)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, d)
		})
	}
}

func TestParseUpdatedFields(t *testing.T) {
	t.Parallel()
	got := parseUpdatedFields([]string{"name,namespaces", " config ", "unknown", "project"})
	assert.Equal(t, map[string]bool{"name": true, "namespaces": true, "config": true, "project": true}, got)
}

func TestBlockedAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"169.254.169.254": true, "169.254.1.1": true, "fe80::1": true, "0.0.0.0": true, "::": true,
		"224.0.0.1": true, "ff02::1": true, "fd00:ec2::254": true, "100.100.100.200": true,
		"::ffff:169.254.169.254": true,
		"127.0.0.1":              false, "10.0.0.1": false, "192.168.1.10": false, "8.8.8.8": false, "::1": false, "fd00::1": false,
	} {
		assert.Equal(t, want, blockedAddr(netip.MustParseAddr(addr)), addr)
	}
	require.Error(t, guardControl("tcp", "169.254.169.254:80", nil))
	require.NoError(t, guardControl("tcp", "127.0.0.1:6443", nil))
	require.Error(t, guardControl("tcp", "not-an-address", nil))
}

// TestProbeNoRedirectAndGuard: probes never follow redirects (the bearer token must not
// reach the redirect target) and the dialer refuses metadata addresses after resolution.
func TestProbeNoRedirectAndGuard(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/version", http.StatusFound)
	}))
	defer redirector.Close()
	conn := NewConnector(ConnectorOptions{})
	c := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: redirector.URL, Name: "r"}}
	_, err := conn.Probe(context.Background(), c, map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("secret-token")})
	require.Error(t, err)
	assert.True(t, errors.Is(err, clusterlib.ErrRedirect), "%v", err)
	assert.Zero(t, leaked.Load(), "credentials replayed to the redirect target")

	meta := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "http://169.254.169.254:80", Name: "m"}}
	_, err = conn.Probe(context.Background(), meta, nil)
	require.ErrorContains(t, err, "not allowed")

	exec := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://kube.example.com", Name: "e", Config: v1alpha1.ClusterConfig{
		ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "/bin/sh", Args: []string{"-c", "exit 1"}, APIVersion: "client.authentication.k8s.io/v1beta1"},
	}}}
	_, err = conn.Probe(context.Background(), exec, nil)
	require.ErrorIs(t, err, ErrExecNotAllowed)
	_, err = conn.Kubernetes(context.Background(), exec, nil)
	require.ErrorIs(t, err, ErrExecNotAllowed)
}

func TestClusterDiff(t *testing.T) {
	t.Parallel()
	cur := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://a", Name: "a", Namespaces: []string{"x"}, Config: v1alpha1.ClusterConfig{CredentialsRef: &v1alpha1.LocalSecretReference{Name: "s"}}}}
	cur.Annotations = map[string]string{v1alpha1.AnnotationKeyRefresh: "2026-01-01T00:00:00Z", "team": "a"}
	creds := map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("t1")}
	same := &desired{spec: v1alpha1.ClusterSpec{Server: "https://a", Name: "a", Namespaces: []string{"x"}}, annotations: map[string]string{"team": "a"}, creds: map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("t1")}}
	assert.Empty(t, clusterDiff(cur, creds, same))
	other := &desired{spec: v1alpha1.ClusterSpec{Server: "https://a", Name: "b", Project: "p", Shard: new(int64(1))}, labels: map[string]string{"l": "v"}, creds: map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte("t2")}}
	got := clusterDiff(cur, creds, other)
	assert.Equal(t, []string{"Name", "Namespaces", "Shard", "Project", "Annotations", "Labels", "Config"}, got)
	for _, k := range got {
		assert.NotContains(t, k, "Token", "credential keys are never named")
	}
}

func TestPaginate(t *testing.T) {
	t.Parallel()
	items := []string{"a", "b", "c", "d", "e"}
	id := func(s string) string { return s }
	page, next, err := paginate(items, id, 2, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, page)
	page, next, err = paginate(items, id, 2, next)
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "d"}, page)
	page, next, err = paginate(items, id, 2, next)
	require.NoError(t, err)
	assert.Equal(t, []string{"e"}, page)
	assert.Empty(t, next)
	all, next, err := paginate(items, id, 0, "")
	require.NoError(t, err)
	assert.Len(t, all, 5)
	assert.Empty(t, next)
	_, _, err = paginate(items, id, -1, "")
	require.Error(t, err)
	_, _, err = paginate(items, id, 1, "!!!")
	require.ErrorContains(t, err, "invalid continue token")
}

func TestSanitize(t *testing.T) {
	t.Parallel()
	c := &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://a", Name: "a", Config: v1alpha1.ClusterConfig{
		ExecProviderConfig: &v1alpha1.ExecProviderConfig{Command: "x", Env: map[string]string{"PASSWORD": "hunter2"}},
		AWSAuthConfig:      &v1alpha1.AWSAuthConfig{ClusterName: "eks"},
	}}}
	c.Annotations = map[string]string{lastAppliedAnnotation: `{"secret":"x"}`, "keep": "1"}
	out := Sanitize(c, map[string][]byte{
		v1alpha1.ClusterSecretKeyBearerToken: []byte("t"), v1alpha1.ClusterSecretKeyPassword: []byte("p"),
		v1alpha1.ClusterSecretKeyCertData: []byte("c"), v1alpha1.ClusterSecretKeyKeyData: []byte("k"),
	}, false)
	assert.Nil(t, out.Spec.Config.ExecProviderConfig)
	assert.Equal(t, "eks", out.Spec.Config.AWSAuthConfig.ClusterName, "awsAuthConfig is not secret")
	assert.Equal(t, apiclient.ClusterCredentialsInfo{HasBearerToken: true, HasBasicAuth: true, HasClientCertificate: true, HasExecProviderConfig: true}, out.Credentials)
	assert.Equal(t, map[string]string{"keep": "1"}, out.Metadata.Annotations)
	assert.NotNil(t, c.Spec.Config.ExecProviderConfig, "input not modified")
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	var got []string
	for _, r := range tbl.Routes() {
		got = append(got, r.Method+" "+r.Path+" "+r.Access.Resource+","+r.Access.Action)
		assert.True(t, r.Access.InHandler, r.OperationID)
	}
	assert.Equal(t, []string{
		"GET /api/v1/clusters clusters,get",
		"POST /api/v1/clusters clusters,create",
		"DELETE /api/v1/clusters/{server} clusters,delete",
		"GET /api/v1/clusters/{server} clusters,get",
		"PUT /api/v1/clusters/{server} clusters,update",
		"POST /api/v1/clusters/{server}/invalidate-cache clusters,update",
		"POST /api/v1/clusters/{server}/rotate-auth clusters,update",
	}, got)
}

func TestUnconfigured(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	_, err := s.list(context.Background(), &apiclient.ListClustersRequest{})
	assert.Equal(t, http.StatusServiceUnavailable, rest.ProblemFor(err).Status)
}
