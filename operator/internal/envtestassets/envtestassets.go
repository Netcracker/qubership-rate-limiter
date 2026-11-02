// Package envtestassets decides whether an envtest suite can start its API
// server here.
package envtestassets

import (
	"os"
	"testing"
)

// RequireEnv is the variable that turns a missing envtest binary into a
// failure rather than a skip.
const RequireEnv = "ENVTEST_REQUIRE"

// SkipUnlessFound skips t when no envtest binaries are found: KUBEBUILDER_ASSETS
// is unset and dir, the bin/k8s version directory the suite would use, is
// empty. With RequireEnv set it fails t instead. make test and the CI build set
// KUBEBUILDER_ASSETS, so the suites run there; a plain go test ./... on a bare
// checkout, which the core Go bulk release runs, skips them.
func SkipUnlessFound(t testing.TB, dir string) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") != "" || dir != "" {
		return
	}
	if os.Getenv(RequireEnv) != "" {
		t.Fatalf("no envtest binaries: KUBEBUILDER_ASSETS is unset, bin/k8s holds none, and %s is set", RequireEnv)
	}
	t.Skip("no envtest binaries: KUBEBUILDER_ASSETS is unset and bin/k8s holds none; run make test")
}
