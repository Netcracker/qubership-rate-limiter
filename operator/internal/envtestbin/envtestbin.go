// Package envtestbin finds the binaries the envtest suites of the operator start
// their control plane from, and skips a suite that has none.
package envtestbin

import (
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SkipUnlessConfigured skips the running suite when no envtest binaries are found,
// or fails it when REQUIRE_ENVTEST is set to any value. binDir is the result of Dir.
// Call it from BeforeSuite.
func SkipUnlessConfigured(binDir string) {
	if configured(binDir) {
		return
	}
	if os.Getenv("REQUIRE_ENVTEST") != "" {
		ginkgo.Fail("envtest binaries not found: set KUBEBUILDER_ASSETS or run make setup-envtest", 1)
	}
	ginkgo.Skip("envtest binaries not found, skipping the envtest suite", 1)
}

// Dir returns the first directory under bin/k8s, where make setup-envtest puts the binaries,
// or "" when there is none. The path is relative to a suite in operator/internal.
func Dir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// configured reports whether any source envtest reads its binaries from is present.
// A set variable counts even when its path is wrong, so a misconfiguration fails the suite instead of skipping it.
func configured(binDir string) bool {
	for _, env := range []string{"TEST_ASSET_KUBE_APISERVER", "TEST_ASSET_ETCD", "KUBEBUILDER_ASSETS"} {
		if os.Getenv(env) != "" {
			return true
		}
	}
	if binDir != "" {
		return true
	}
	_, err := os.Stat("/usr/local/kubebuilder/bin/kube-apiserver")
	return err == nil
}
