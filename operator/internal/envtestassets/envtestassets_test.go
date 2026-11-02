package envtestassets

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// recorder is a testing.TB that records whether it was skipped or failed.
// Skip and Fatalf end the calling goroutine, as the real ones do, so the
// recorder is driven from a goroutine of its own.
type recorder struct {
	testing.TB
	skipped, failed bool
}

func (r *recorder) Helper() {}

func (r *recorder) Skip(...any) {
	r.skipped = true
	runtime.Goexit()
}

func (r *recorder) Fatalf(string, ...any) {
	r.failed = true
	runtime.Goexit()
}

func run(dir string) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		SkipUnlessFound(r, dir)
	}()
	<-done
	return r
}

// A suite runs where KUBEBUILDER_ASSETS or bin/k8s names the binaries; with
// neither it is skipped, and with RequireEnv set it fails instead.
func TestSkipUnlessFound_skipsOnlyWithoutBinaries(t *testing.T) {
	for _, c := range []struct {
		name            string
		assets, require string
		dir             string
		skipped, failed bool
	}{
		{name: "KUBEBUILDER_ASSETS set", assets: "/opt/envtest"},
		{name: "a bin/k8s version directory", dir: "../../../bin/k8s/1.37.0-linux-amd64"},
		{name: "neither", skipped: true},
		{name: "neither, with ENVTEST_REQUIRE set", require: "1", failed: true},
		{name: "KUBEBUILDER_ASSETS set, with ENVTEST_REQUIRE set", assets: "/opt/envtest", require: "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KUBEBUILDER_ASSETS", c.assets)
			t.Setenv(RequireEnv, c.require)

			r := run(c.dir)

			assert.Equal(t, c.skipped, r.skipped, "skipped")
			assert.Equal(t, c.failed, r.failed, "failed")
		})
	}
}
