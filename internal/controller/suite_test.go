package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	//+kubebuilder:scaffold:imports
)

// fakeClusterProvider wraps an envtest client to satisfy the ClusterProvider
// interface. GetCluster always returns the same single-cluster facade.
type fakeClusterProvider struct {
	cl *fakeCluster
}

func (f *fakeClusterProvider) GetCluster(_ context.Context, _ multicluster.ClusterName) (cluster.Cluster, error) {
	return f.cl, nil
}

// fakeCluster is a minimal cluster.Cluster backed by an envtest client.
type fakeCluster struct {
	client client.Client
}

func (f *fakeCluster) GetClient() client.Client                        { return f.client }
func (f *fakeCluster) GetScheme() *runtime.Scheme                      { panic("not implemented") }
func (f *fakeCluster) GetFieldIndexer() client.FieldIndexer            { panic("not implemented") }
func (f *fakeCluster) Start(_ context.Context) error                   { panic("not implemented") }
func (f *fakeCluster) GetHTTPClient() *http.Client                     { panic("not implemented") }
func (f *fakeCluster) GetConfig() *rest.Config                         { panic("not implemented") }
func (f *fakeCluster) GetCache() cache.Cache                           { panic("not implemented") }
func (f *fakeCluster) GetRESTMapper() meta.RESTMapper                  { panic("not implemented") }
func (f *fakeCluster) GetEventRecorderFor(string) record.EventRecorder { panic("not implemented") }
func (f *fakeCluster) GetEventRecorder(string) events.EventRecorder    { panic("not implemented") }
func (f *fakeCluster) GetLogger() logr.Logger                          { panic("not implemented") }
func (f *fakeCluster) GetAPIReader() client.Reader                     { return f.client }

var (
	cfg             *rest.Config
	k8sClient       client.Client
	testEnv         *envtest.Environment
	testClusterProv *fakeClusterProvider
)

func TestControllers(t *testing.T) {
	assetsDir := envtestBinaryAssetsDirectory()
	if _, err := os.Stat(assetsDir); err != nil {
		t.Skipf("skipping controller suite: envtest binaries missing at %s", assetsDir)
	}

	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	slog.SetDefault(slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})))

	By("bootstrapping test environment")
	assetsDir := envtestBinaryAssetsDirectory()
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assetsDir,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = naisiov1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	//+kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	testClusterProv = &fakeClusterProvider{cl: &fakeCluster{client: k8sClient}}
})

var _ = AfterSuite(func() {
	if testEnv == nil {
		return
	}
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func envtestBinaryAssetsDirectory() string {
	preferred := filepath.Join("..", "..", "bin", "k8s", fmt.Sprintf("1.34.0-%s-%s", goruntime.GOOS, goruntime.GOARCH))
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}

	baseDir := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return preferred
	}

	suffix := fmt.Sprintf("-%s-%s", goruntime.GOOS, goruntime.GOARCH)
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		candidates = append(candidates, entry.Name())
	}

	if len(candidates) == 0 {
		return preferred
	}

	sort.Strings(candidates)
	return filepath.Join(baseDir, candidates[len(candidates)-1])
}
