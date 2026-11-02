package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// DefaultNamespace is the Patrol namespace of core mode when neither -n/--namespace nor
// the kube context names one (upstream uses the context namespace, else "default").
const DefaultNamespace = "patrol"

// resolveNamespace returns the Patrol namespace: -n/--namespace, else the namespace of the
// selected kube context, else DefaultNamespace.
func resolveNamespace(cc clientcmd.ClientConfig, o clientcmd.ConfigOverrides) string {
	if ns := strings.TrimSpace(o.Context.Namespace); ns != "" {
		return ns
	}
	raw, err := cc.RawConfig()
	if err == nil {
		name := o.CurrentContext
		if name == "" {
			name = raw.CurrentContext
		}
		if c := raw.Contexts[name]; c != nil && c.Namespace != "" {
			return c.Namespace
		}
	}
	return DefaultNamespace
}

// destinationConfigs gives the resource routes (logs, events, live resources, actions) and the
// previews of a cluster whose status lacks its versions the caller's own credentials for each
// destination cluster: the in-cluster address and the selected context's cluster use the selected
// context; another cluster uses the first kubeconfig context (by name) whose cluster has the same
// server URL. Clusters without such a context are refused: the in-process server never uses the
// credentials stored for patrol-server in Cluster Secrets.
type destinationConfigs struct {
	current       *rest.Config
	currentServer string
	raw           clientcmdapi.Config
}

func newDestinationConfigs(cc clientcmd.ClientConfig, current *rest.Config) *destinationConfigs {
	d := &destinationConfigs{current: current, currentServer: clusters.NormalizeServer(current.Host)}
	if raw, err := cc.RawConfig(); err == nil {
		d.raw = raw
	}
	return d
}

// RESTConfig returns the configuration for cluster c.
func (d *destinationConfigs) RESTConfig(_ context.Context, c *v1alpha1.Cluster) (*rest.Config, error) {
	if c == nil {
		return nil, fmt.Errorf("core mode: no destination cluster")
	}
	server := clusters.NormalizeServer(c.Spec.Server)
	if clusters.IsInClusterServer(server) || server == d.currentServer {
		return rest.CopyConfig(d.current), nil
	}
	names := make([]string, 0, len(d.raw.Contexts))
	for n := range d.raw.Contexts {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		kc := d.raw.Contexts[n]
		if kc == nil {
			continue
		}
		cl := d.raw.Clusters[kc.Cluster]
		if cl == nil || clusters.NormalizeServer(cl.Server) != server {
			continue
		}
		cfg, err := clientcmd.NewNonInteractiveClientConfig(d.raw, n, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("core mode: kube context %s for destination cluster %s: %w", n, server, err)
		}
		return cfg, nil
	}
	return nil, &noContextError{server: server}
}

// noContextError reports that the caller's kubeconfig has no context for a destination cluster.
// The in-process API server shows UserMessage to the caller (applications.userMessager) instead
// of a bare "unavailable": the kubeconfig is the caller's own, so nothing about it is secret.
type noContextError struct{ server string }

func (e *noContextError) Error() string {
	return fmt.Sprintf("core mode: no kubeconfig context for destination cluster %s; core mode reads live resources with your own kubeconfig credentials", e.server)
}

// UserMessage is the text for the caller.
func (e *noContextError) UserMessage() string { return e.Error() }

// NoCredentials marks the error for the in-process API server (applications.noCredentials): a
// preview that wanted the cluster's versions renders with what its status holds.
func (e *noContextError) NoCredentials() {}
