//go:build e2e

package e2e

import (
	"net/http"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/netcracker/qubership-rate-limiter/api/v1"
)

var _ = Describe("excluded routes", Label("excluded-routes"), func() {
	const (
		domain  = "gateway.public"
		gateway = "public-gateway"
		prefix  = "/e2e-excluded"
	)

	It("skips the block for list requests and charges neighboring requests", func() {
		waitGatewayServes(gateway, prefix)
		DeferCleanup(func() { deletePolicies(domain) })
		Expect(apply(newPolicy(domain, []v1.LimitBlock{{
			Name: "api",
			Target: &v1.Target{
				Routes: []v1.Route{{
					Path: v1.PathMatch{Type: v1.PathMatchTemplate, Value: prefix + "/{run}/{operation}"},
				}},
				ExcludedRoutes: []v1.ExcludedRoute{{
					Path:    v1.PathMatch{Type: v1.PathMatchTemplate, Value: prefix + "/{run}/list"},
					Methods: []v1.HTTPMethod{http.MethodGet},
				}},
			},
			Rules: []v1.Rule{{
				Name:     "per-run",
				Counters: []string{"run"},
				Rates:    []v1.Rate{{Requests: 1, PeriodSeconds: 3600}},
			}},
		}}))).To(Succeed())
		waitApplied(domain)

		path := prefix + "/" + strconv.FormatInt(time.Now().UnixNano(), 10)
		codes := gatewayBurst(gateway, path+"/list?limit=20", 2, nil)
		Expect(codes).To(HaveEach(beAdmitted()), "list requests leave the shared bucket full")

		codes, headers := gatewayBurstWithHeaders(gateway, path+"/details", 2, nil)
		Expect(codes).To(HaveExactElements(beAdmitted(), Equal(http.StatusTooManyRequests)),
			"the first details request spends the bucket's only unit")
		Expect(rateLimitHeaders(headers[0])).To(HaveKeyWithValue("x-ratelimit-remaining", "0"))

		codes = gatewayBurst(gateway, path+"/list", 1, nil)
		Expect(codes).To(HaveEach(beAdmitted()), "list requests still pass after the bucket is empty")
	})
})
