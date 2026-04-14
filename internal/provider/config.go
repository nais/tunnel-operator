package provider

import (
	"fmt"
	"net/http"
	"strings"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// StaticCluster represents a cluster with static credentials (bearer token).
type StaticCluster struct {
	Name  string
	Host  string
	Token string
}

// CreateClusterConfigMap builds rest.Configs for GKE-based tenant clusters
// using the Google auth plugin, and for static clusters using bearer tokens.
func CreateClusterConfigMap(tenant string, clusters []string, staticClusters []StaticCluster) (map[string]*rest.Config, error) {
	configs := make(map[string]*rest.Config)

	for _, cluster := range clusters {
		cluster = strings.TrimSpace(cluster)
		if cluster == "" {
			continue
		}
		configs[cluster] = &rest.Config{
			Host: fmt.Sprintf("https://apiserver.%s.%s.cloud.nais.io", cluster, tenant),
			AuthProvider: &clientcmdapi.AuthProviderConfig{
				Name: GoogleAuthPlugin,
			},
			WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
				return otelhttp.NewTransport(rt, otelhttp.WithServerName(cluster))
			},
		}
	}

	for _, sc := range staticClusters {
		configs[sc.Name] = &rest.Config{
			Host:        sc.Host,
			BearerToken: sc.Token,
			TLSClientConfig: rest.TLSClientConfig{
				Insecure: true,
			},
			WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
				return otelhttp.NewTransport(rt, otelhttp.WithServerName(sc.Name))
			},
		}
	}

	return configs, nil
}

// ParseStaticClusters parses a comma-separated list of static clusters in the
// format "name|host|token" into a slice of StaticCluster.
func ParseStaticClusters(raw string) ([]StaticCluster, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var clusters []StaticCluster
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "|", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid static cluster entry %q: expected name|host|token", entry)
		}
		clusters = append(clusters, StaticCluster{
			Name:  strings.TrimSpace(parts[0]),
			Host:  strings.TrimSpace(parts[1]),
			Token: strings.TrimSpace(parts[2]),
		})
	}
	return clusters, nil
}
