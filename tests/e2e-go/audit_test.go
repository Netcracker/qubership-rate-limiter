//go:build e2e

package e2e

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/netcracker/qubership-rate-limiter/engine/model"
)

var _ = Describe("security audit through the gateway", Label("audit"), func() {
	It("writes one refusal record per replica for one caller and rule in an interval", func() {
		const (
			gateway = "public-gateway"
			domain  = "gateway.public"
			path    = "/e2e"
			rule    = "audit-per-client"
			refused = 5
		)
		waitGatewayServes(gateway, path)
		policy := newPolicy(domain, hourlyGCRALimits(path, rule, []string{model.KeySub}, 1))
		DeferCleanup(deletePolicies, domain)
		Expect(apply(policy)).To(Succeed())
		waitApplied(domain)

		const interval = 10 * time.Second
		now := time.Now()
		start := now.Truncate(interval).Add(interval)
		// Leave room for clock skew and for the port-forward to open before the interval ends.
		time.Sleep(time.Until(start.Add(time.Second)))
		sub := "audit-" + strconv.FormatInt(start.UnixNano(), 10)
		token := unsignedToken(map[string]string{model.KeySub: sub})
		codes := gatewayBurst(gateway, path, 1+refused, map[string]string{
			"Authorization": "Bearer " + token,
			"X-Request-Id":  sub,
		})
		Expect(codes[0]).To(beAdmitted())
		Expect(codes[1:]).To(HaveEach(http.StatusTooManyRequests))

		message := "rate limit exceeded domain=" + domain + " rule=probe/" + rule + " window=gcra:3600 sub=" + sub +
			` axes={"sub":"` + sub + `"} path=` + path + " method=GET refusals="
		countPattern := regexp.MustCompile(regexp.QuoteMeta(message) + `([0-9]+) first=\S+ last=\S+ requestId=` + sub)
		Eventually(func(g Gomega) {
			var pods corev1.PodList
			g.Expect(k8s.List(ctx, &pods, client.InNamespace(namespace),
				client.MatchingLabels{"app.kubernetes.io/name": serviceChart})).To(Succeed())
			total := 0
			since := start.Add(-interval)
			for _, pod := range pods.Items {
				logs := podLogs(pod.Name, &since)
				g.Expect(logs).NotTo(ContainSubstring(token))
				g.Expect(strings.Count(logs, message)).To(BeNumerically("<=", 1), "records on %s", pod.Name)
				if match := countPattern.FindStringSubmatch(logs); match != nil {
					count, err := strconv.Atoi(match[1])
					g.Expect(err).NotTo(HaveOccurred())
					total += count
				}
			}
			g.Expect(total).To(Equal(refused), "refusals across replica audit logs for %s", sub)
		}).WithTimeout(30 * time.Second).Should(Succeed())
	})
})
