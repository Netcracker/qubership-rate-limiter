// Package envtestbin finds the binaries the envtest suites of the operator start
// their control plane from, and skips a suite that has none.
package envtestbin

import (
	"os"
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
)

// SkipUnlessConfigured skips the running suite when no envtest binaries are found,
// or fails it when REQUIRE_ENVTEST is set to any value. Call it from BeforeSuite.
func SkipUnlessConfigured() {
	if configured() {
		return
	}
	if os.Getenv("REQUIRE_ENVTEST") != "" {
		ginkgo.Fail("envtest binaries not found: set KUBEBUILDER_ASSETS or run make setup-envtest", 1)
	}
	ginkgo.Skip("envtest binaries not found, skipping the envtest suite", 1)
}

// Dir returns the first directory under bin/k8s of the module, where make setup-envtest
// puts the binaries, or "" when there is none.
func Dir() string {
	root := moduleRoot()
	if root == "" {
		return ""
	}
	base := filepath.Join(root, "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}

// configured reports whether any source envtest reads its binaries from is present.
// A set variable counts even when its path is wrong, so a misconfiguration fails the suite instead of skipping it.
func configured() bool {
	for _, env := range []string{"TEST_ASSET_KUBE_APISERVER", "TEST_ASSET_ETCD", "KUBEBUILDER_ASSETS"} {
		if os.Getenv(env) != "" {
			return true
		}
	}
	if Dir() != "" {
		return true
	}
	_, err := os.Stat("/usr/local/kubebuilder/bin/kube-apiserver")
	return err == nil
}

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
