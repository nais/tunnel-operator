package provider

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

var (
	_ multicluster.Provider         = &Static{}
	_ multicluster.ProviderRunnable = &Static{}
)

type indexEntry struct {
	obj          client.Object
	field        string
	extractValue client.IndexerFunc
}

// Static is a multicluster provider that manages a fixed set of clusters
// from pre-configured rest.Configs.
type Static struct {
	lock     sync.RWMutex
	configs  map[multicluster.ClusterName]*rest.Config
	clusters map[multicluster.ClusterName]cluster.Cluster
	opts     []cluster.Option
	indexers []indexEntry
}

// NewStatic creates a new static provider from a map of cluster name to rest.Config.
func NewStatic(configs map[string]*rest.Config, opts ...cluster.Option) *Static {
	named := make(map[multicluster.ClusterName]*rest.Config, len(configs))
	for name, cfg := range configs {
		named[multicluster.ClusterName(name)] = cfg
	}
	return &Static{
		configs:  named,
		clusters: make(map[multicluster.ClusterName]cluster.Cluster, len(configs)),
		opts:     opts,
	}
}

// Get returns a cluster by name, or ErrClusterNotFound if unknown.
func (p *Static) Get(_ context.Context, name multicluster.ClusterName) (cluster.Cluster, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if cl, ok := p.clusters[name]; ok {
		return cl, nil
	}
	return nil, multicluster.ErrClusterNotFound
}

// Start creates and starts all configured clusters, engages them with the
// multicluster-aware manager, and blocks until the context is cancelled.
func (p *Static) Start(ctx context.Context, aware multicluster.Aware) error {
	logger := log.FromContext(ctx).WithName("static-cluster-provider")

	for name, cfg := range p.configs {
		logger.Info("Starting cluster", "cluster", name)

		cl, err := cluster.New(cfg, p.opts...)
		if err != nil {
			return fmt.Errorf("creating cluster %s: %w", name, err)
		}

		for _, idx := range p.indexers {
			if err := cl.GetCache().IndexField(ctx, idx.obj, idx.field, idx.extractValue); err != nil {
				return fmt.Errorf("indexing field %q on cluster %s: %w", idx.field, name, err)
			}
		}

		clusterCtx, cancel := context.WithCancel(ctx)
		go func() {
			if err := cl.Start(clusterCtx); err != nil {
				logger.Error(err, "Cluster stopped with error", "cluster", name)
				cancel()
			}
		}()

		if !cl.GetCache().WaitForCacheSync(ctx) {
			cancel()
			return fmt.Errorf("cache sync failed for cluster %s", name)
		}

		p.lock.Lock()
		p.clusters[name] = cl
		p.lock.Unlock()

		if err := aware.Engage(clusterCtx, name, cl); err != nil {
			cancel()
			return fmt.Errorf("engaging cluster %s: %w", name, err)
		}

		logger.Info("Cluster ready", "cluster", name)
	}

	<-ctx.Done()
	return nil
}

// IndexField indexes the given object by the given field on all clusters, current and future.
func (p *Static) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.indexers = append(p.indexers, indexEntry{obj, field, extractValue})

	for name, cl := range p.clusters {
		if err := cl.GetCache().IndexField(ctx, obj, field, extractValue); err != nil {
			return fmt.Errorf("indexing field %q on cluster %s: %w", field, name, err)
		}
	}

	return nil
}
