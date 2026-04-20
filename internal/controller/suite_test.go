package controller

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	//+kubebuilder:scaffold:imports
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
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
