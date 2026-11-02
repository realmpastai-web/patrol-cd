package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// remoteCluster is a TLS stand-in for a destination cluster that records every request
// with the credentials it carried. It answers /version and 404 for anything else.
type remoteCluster struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newRemoteCluster(t *testing.T) *remoteCluster {
	t.Helper()
	r := &remoteCluster{}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, fmt.Sprintf("%s %s auth=%s", req.Method, req.URL.Path, req.Header.Get("Authorization")))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.2"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"not found"}`))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *remoteCluster) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *remoteCluster) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = nil
}

// handlerRoundTripper serves requests with an http.Handler, in-process.
type handlerRoundTripper struct{ h http.Handler }

func (rt handlerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// With no provider, a destination other than the in-cluster one is one core mode has no credentials
// for, which a preview renders without.
func TestCoreInClusterOnly(t *testing.T) {
	t.Parallel()
	a := &coreAssembly{restCfg: &krest.Config{Host: "https://kubernetes.default.svc"}}
	cfg, err := a.inClusterOnly(t.Context(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: v1alpha1.KubernetesInternalAPIServerAddr}})
	require.NoError(t, err)
	assert.Equal(t, a.restCfg.Host, cfg.Host)

	var none interface{ NoCredentials() }
	_, err = a.inClusterOnly(t.Context(), &v1alpha1.Cluster{Spec: v1alpha1.ClusterSpec{Server: "https://edge.example.com"}})
	require.ErrorAs(t, err, &none)
	assert.ErrorContains(t, err, "https://edge.example.com")
	_, err = a.inClusterOnly(t.Context(), nil)
	require.ErrorAs(t, err, &none)
}

// TestCoreClusterConnectionsUseCallerCredentials: cluster updates and rotate-auth in core
// mode never authenticate with the credentials stored in the Cluster Secret (patrol's
// manager service account on the destination cluster). With a kubeconfig context for the
// server, the connection check and the rotation use the caller's credentials; without one,
// nothing is sent to the cluster: the update proceeds unchecked and rotate-auth is refused.
func TestCoreClusterConnectionsUseCallerCredentials(t *testing.T) {
	requireEnvtest(t)
	cfg := krest.CopyConfig(testCfg)
	ctx := t.Context()
	c, err := client.New(cfg, client.Options{Scheme: mustScheme(t)})
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ns := newNamespace(t, kube)
	remote := newRemoteCluster(t)

	stored, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":                                    "kubernetes/serviceaccount",
		"sub":                                    clusters.ServiceAccountUsername("kube-system", "patrol-manager"),
		"kubernetes.io/serviceaccount/namespace": "kube-system",
		"kubernetes.io/serviceaccount/secret.name":          "patrol-manager-token-abcde",
		"kubernetes.io/serviceaccount/service-account.name": "patrol-manager",
	}).SignedString([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-creds", Labels: map[string]string{v1alpha1.LabelKeySecretType: v1alpha1.LabelValueSecretTypeCluster}},
		Data:       map[string][]byte{v1alpha1.ClusterSecretKeyBearerToken: []byte(stored)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	// The stored configuration would connect (certificate verification off): only the
	// connector decides which credentials reach the cluster.
	require.NoError(t, c.Create(ctx, &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "remote"},
		Spec: v1alpha1.ClusterSpec{Server: remote.URL, Name: "remote", Config: v1alpha1.ClusterConfig{
			TLSClientConfig: v1alpha1.TLSClientConfig{Insecure: true},
			CredentialsRef:  &v1alpha1.LocalSecretReference{Name: "remote-creds"},
		}},
	}))

	api := func(t *testing.T, dest func(context.Context, *v1alpha1.Cluster) (*krest.Config, error)) *apiclient.Client {
		t.Helper()
		h, stop, err := NewCoreHandler(ctx, CoreConfig{
			Namespace: ns, RestConfig: krest.CopyConfig(cfg), DestinationConfig: dest,
			RepoServerURL: "http://127.0.0.1:1", TreeAPI: TreeAPIClientOptions{URL: "http://127.0.0.1:1"},
		})
		require.NoError(t, err)
		t.Cleanup(stop)
		ac, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: "core.invalid", PlainText: true, HTTPClient: &http.Client{Transport: handlerRoundTripper{h}}})
		require.NoError(t, err)
		return ac
	}
	name := func() string {
		var cl v1alpha1.Cluster
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "remote"}, &cl))
		return cl.Spec.Name
	}
	storedToken := func() string {
		sec, err := kube.CoreV1().Secrets(ns).Get(ctx, "remote-creds", metav1.GetOptions{})
		require.NoError(t, err)
		return string(sec.Data[v1alpha1.ClusterSecretKeyBearerToken])
	}
	id := apiclient.ClusterByServer(remote.URL)

	t.Run("no kubeconfig context for the server", func(t *testing.T) {
		remote.reset()
		ac := api(t, nil) // in-cluster destination only
		_, err := ac.UpdateCluster(ctx, id, &apiclient.ClusterInput{Name: "renamed-1"}, "name")
		require.NoError(t, err, "the update proceeds without a connection check")
		assert.Equal(t, "renamed-1", name())
		err = ac.RotateClusterAuth(ctx, id)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only with your own kubeconfig credentials")
		assert.Empty(t, remote.requests(), "nothing may reach the cluster")
		assert.Equal(t, stored, storedToken())
	})

	t.Run("caller's kubeconfig context for the server", func(t *testing.T) {
		remote.reset()
		const callerToken = "caller-remote-token"
		ac := api(t, func(_ context.Context, cl *v1alpha1.Cluster) (*krest.Config, error) {
			if clusters.NormalizeServer(cl.Spec.Server) != clusters.NormalizeServer(remote.URL) {
				return nil, fmt.Errorf("no context for %s", cl.Spec.Server)
			}
			return &krest.Config{Host: remote.URL, BearerToken: callerToken, TLSClientConfig: krest.TLSClientConfig{Insecure: true}}, nil
		})
		_, err := ac.UpdateCluster(ctx, id, &apiclient.ClusterInput{Name: "renamed-2"}, "name")
		require.NoError(t, err)
		assert.Equal(t, "renamed-2", name())
		assert.Equal(t, []string{"GET /version auth=Bearer " + callerToken}, remote.requests())

		// The rotation reads the token Secret named by the stored token's claims with the
		// caller's credentials (the stand-in has none, so it fails before anything changes).
		remote.reset()
		err = ac.RotateClusterAuth(ctx, id)
		require.Error(t, err)
		seen := remote.requests()
		require.NotEmpty(t, seen)
		assert.Equal(t, "GET /api/v1/namespaces/kube-system/secrets/patrol-manager-token-abcde auth=Bearer "+callerToken, seen[0])
		for _, s := range seen {
			assert.True(t, strings.HasSuffix(s, "auth=Bearer "+callerToken), s)
		}
		assert.Equal(t, stored, storedToken())
	})
}
