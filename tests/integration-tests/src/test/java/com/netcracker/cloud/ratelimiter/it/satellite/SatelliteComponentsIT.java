package com.netcracker.cloud.ratelimiter.it.satellite;

import com.netcracker.cloud.ratelimiter.it.support.Gateway;
import com.netcracker.cloud.ratelimiter.it.support.Resources;
import com.netcracker.cloud.ratelimiter.it.support.SatelliteTestBase;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.function.Executable;

import java.util.Arrays;
import java.util.stream.Stream;

import static org.junit.jupiter.api.Assertions.assertAll;
import static org.junit.jupiter.api.Assertions.assertNull;

/**
 * A satellite carries the gateway filters and nothing else: the filters call the service of the baseline, and the
 * operator and the service run only there.
 */
@DisplayName("Satellites rely on the rate limiting of the baseline")
class SatelliteComponentsIT extends SatelliteTestBase {

    @Test
    @DisplayName("Gateways are wired to the baseline Service")
    void gatewaysAreWiredToBaselineService() {
        assertAll(satelliteNamespaces().stream().flatMap(namespace -> Arrays.stream(Gateway.values())
                .map(gateway -> gatewayIsWiredToBaselineService(namespace, gateway))));
    }

    @Test
    @DisplayName("Deployments run only in the baseline")
    void deploymentsRunOnlyInBaseline() {
        Stream<Executable> checks = satelliteNamespaces().stream().flatMap(namespace -> Resources.DEPLOYMENTS.stream().map(name -> () ->
                assertNull(kubernetesClient.apps().deployments().inNamespace(namespace).withName(name).get(),
                        "%s: runs %s, which belongs only in the baseline; a satellite carries just the EnvoyFilters".formatted(namespace, name))));
        assertAll(checks);
    }
}
