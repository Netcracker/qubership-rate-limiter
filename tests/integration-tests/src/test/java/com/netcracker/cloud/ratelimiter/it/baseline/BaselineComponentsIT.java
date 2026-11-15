package com.netcracker.cloud.ratelimiter.it.baseline;

import com.netcracker.cloud.ratelimiter.it.support.BaselineTestBase;
import com.netcracker.cloud.ratelimiter.it.support.Gateway;
import com.netcracker.cloud.ratelimiter.it.support.Resources;
import io.fabric8.kubernetes.api.model.discovery.v1.Endpoint;
import io.fabric8.kubernetes.api.model.discovery.v1.EndpointPort;
import io.fabric8.kubernetes.api.model.discovery.v1.EndpointSlice;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.Arrays;
import java.util.List;
import java.util.Objects;

import static org.junit.jupiter.api.Assertions.assertAll;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Everything rate-limiter needs is in place in the baseline, so the traffic tests have something to test.
 * <p>
 * Test names stay the same on every environment. The namespace and the gateway of a failure are in its message,
 * and {@code assertAll} reports every failing one at once. Each test checks one part on its own.
 */
@DisplayName("Rate limiting components are in place in the baseline")
class BaselineComponentsIT extends BaselineTestBase {

    @Test
    @DisplayName("Deployments are ready")
    void deploymentsAreReady() {
        assertAll(Resources.DEPLOYMENTS.stream().map(name -> () ->
                assertTrue(deploymentIsReady(baselineNamespace(), name),
                        "%s: Deployment %s is missing or has replicas that are not ready".formatted(baselineNamespace(), name))));
    }

    @Test
    @DisplayName("Service has a ready gRPC endpoint")
    void serviceHasReadyGrpcEndpoint() {
        List<EndpointSlice> slices = kubernetesClient.discovery().v1().endpointSlices()
                .inNamespace(baselineNamespace()).withLabel("kubernetes.io/service-name", Resources.SERVICE)
                .list().getItems();
        boolean servesGrpc = slices.stream()
                .flatMap(slice -> slice.getPorts().stream())
                .map(EndpointPort::getPort)
                .anyMatch(port -> Objects.equals(port, Resources.GRPC_PORT));
        boolean hasReadyEndpoint = slices.stream()
                .flatMap(slice -> slice.getEndpoints().stream())
                .map(Endpoint::getConditions)
                .anyMatch(conditions -> conditions != null && Boolean.TRUE.equals(conditions.getReady()));
        assertAll(
                () -> assertTrue(servesGrpc, "%s: Service %s exposes no port %d"
                        .formatted(baselineNamespace(), Resources.SERVICE, Resources.GRPC_PORT)),
                () -> assertTrue(hasReadyEndpoint, "%s: Service %s has no ready endpoint"
                        .formatted(baselineNamespace(), Resources.SERVICE)));
    }

    // Listing the policies proves both that the CRD is installed and that the token may use it; the deploy token
    // cannot read cluster-scoped CRDs directly.
    @Test
    @DisplayName("RateLimitPolicy resources are available")
    void policiesAreAvailable() {
        assertDoesNotThrow(() -> kubernetesClient.genericKubernetesResources(Resources.POLICY_API, Resources.POLICY_KIND)
                .inNamespace(baselineNamespace()).list());
    }

    @Test
    @DisplayName("Gateways are wired to the Service")
    void gatewaysAreWiredToService() {
        assertAll(Arrays.stream(Gateway.values())
                .map(gateway -> gatewayIsWiredToBaselineService(baselineNamespace(), gateway)));
    }
}
