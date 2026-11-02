package envtestbin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name                   string
		env                    map[string]string
		binDir                 string
		defaultAPIServerExists bool
		want                   suiteAction
	}{
		{name: "no configuration skips", want: skipSuite},
		{
			name: "required binaries are missing",
			env:  map[string]string{"REQUIRE_ENVTEST": "1"},
			want: failSuite,
		},
		{
			name: "any nonempty requirement fails without binaries",
			env:  map[string]string{"REQUIRE_ENVTEST": "0"},
			want: failSuite,
		},
		{
			name: "ENVTEST_REQUIRE does not require binaries",
			env:  map[string]string{"ENVTEST_REQUIRE": "1"},
			want: skipSuite,
		},
		{
			name: "an existing cluster needs no binaries",
			env:  map[string]string{"USE_EXISTING_CLUSTER": "true"},
			want: runSuite,
		},
		{
			name: "an existing cluster setting is case-insensitive",
			env:  map[string]string{"USE_EXISTING_CLUSTER": "TRUE"},
			want: runSuite,
		},
		{
			name: "an existing cluster satisfies the requirement",
			env:  map[string]string{"USE_EXISTING_CLUSTER": "true", "REQUIRE_ENVTEST": "1"},
			want: runSuite,
		},
		{
			name: "an existing cluster must be enabled",
			env:  map[string]string{"USE_EXISTING_CLUSTER": "false"},
			want: skipSuite,
		},
		{
			name: "an explicit API server path is enough to run",
			env:  map[string]string{"TEST_ASSET_KUBE_APISERVER": "/missing/kube-apiserver"},
			want: runSuite,
		},
		{
			name: "an explicit etcd path is enough to run",
			env:  map[string]string{"TEST_ASSET_ETCD": "/missing/etcd"},
			want: runSuite,
		},
		{
			name: "an explicit assets path is enough to run",
			env:  map[string]string{"KUBEBUILDER_ASSETS": "/missing/assets"},
			want: runSuite,
		},
		{
			name: "explicit assets satisfy the requirement",
			env:  map[string]string{"KUBEBUILDER_ASSETS": "/missing/assets", "REQUIRE_ENVTEST": "1"},
			want: runSuite,
		},
		{name: "a discovered directory is enough to run", binDir: "/envtest", want: runSuite},
		{
			name:   "a discovered directory satisfies the requirement",
			env:    map[string]string{"REQUIRE_ENVTEST": "1"},
			binDir: "/envtest",
			want:   runSuite,
		},
		{name: "a default API server is enough to run", defaultAPIServerExists: true, want: runSuite},
		{
			name:                   "a default API server satisfies the requirement",
			env:                    map[string]string{"REQUIRE_ENVTEST": "1"},
			defaultAPIServerExists: true,
			want:                   runSuite,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			assert.Equal(t, tc.want, decide(tc.binDir, getenv, tc.defaultAPIServerExists))
		})
	}
}
