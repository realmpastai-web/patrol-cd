package clusters

import (
	"errors"
	"net/http"
)

// ErrRedirect is returned when a cluster answers a request with a redirect: credentials
// are never replayed to another location.
var ErrRedirect = errors.New("the cluster answered with a redirect, which is not followed")

// RefuseRedirects turns redirect responses into errors so that the HTTP client never replays
// the cluster credentials to the redirect target.
func RefuseRedirects(rt http.RoundTripper) http.RoundTripper { return noRedirect{rt} }

type noRedirect struct{ rt http.RoundTripper }

func (n noRedirect) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := n.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
		_ = resp.Body.Close()
		return nil, ErrRedirect
	}
	return resp, nil
}
