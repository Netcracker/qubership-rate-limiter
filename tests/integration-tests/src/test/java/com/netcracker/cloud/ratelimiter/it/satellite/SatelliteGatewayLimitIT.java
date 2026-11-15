package com.netcracker.cloud.ratelimiter.it.satellite;

import com.netcracker.cloud.ratelimiter.it.support.Gateway;
import com.netcracker.cloud.ratelimiter.it.support.Requests;
import com.netcracker.cloud.ratelimiter.it.support.SatelliteTestBase;
import com.netcracker.cloud.ratelimiter.it.support.TestLimits;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.net.URI;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertAll;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;

/**
 * The public gateway of a satellite enforces the policy that lives in the baseline, and counts in the same budget
 * as the gateway of the baseline.
 * <p>
 * Every satellite gets a limit on a path of its own, because one path is one budget for all gateways. The tests
 * assert the first and the last request of a series, not each one: a request for a path the satellite does not
 * know goes on through the gateway of the baseline and is counted there again.
 */
@DisplayName("Satellite gateways enforce the rate limits of the baseline")
class SatelliteGatewayLimitIT extends SatelliteTestBase {

    private static final int LIMIT = 3;

    private final Map<String, URI> satelliteGateways = new LinkedHashMap<>();
    private final Map<String, String> ownPaths = new LinkedHashMap<>();
    private String sharedPath;
    private TestLimits limits;

    @BeforeAll
    void addLimits() {
        limits = new TestLimits(kubernetesClient, baselineNamespace());
        for (String namespace : satelliteNamespaces()) {
            satelliteGateways.put(namespace, gatewayUri(namespace, Gateway.PUBLIC));
            ownPaths.put(namespace, limits.add(Gateway.PUBLIC, "own-" + namespace, LIMIT));
        }
        sharedPath = limits.add(Gateway.PUBLIC, "shared", LIMIT);
        limits.apply();
    }

    @AfterAll
    void removeLimits() {
        if (limits != null) {
            limits.remove();
        }
    }

    @Test
    @DisplayName("Requests over the limit get 429")
    void requestsOverLimitGet429() {
        assertAll(satelliteGateways.entrySet().stream().map(satellite -> () -> {
            List<Integer> statuses = Requests.statuses(satellite.getValue(), ownPaths.get(satellite.getKey()), LIMIT + 1);
            String where = "%s/%s".formatted(satellite.getKey(), Gateway.PUBLIC.resourceName());
            assertNotEquals(Requests.LIMITED, statuses.getFirst(), "%s: the first request of a limit of %d, statuses %s"
                    .formatted(where, LIMIT, statuses));
            assertEquals(Requests.LIMITED, statuses.getLast(), "%s: request %d of a limit of %d, statuses %s"
                    .formatted(where, LIMIT + 1, LIMIT, statuses));
        }));
    }

    @Test
    @DisplayName("Baseline and satellites share one budget")
    void baselineAndSatellitesShareOneBudget() {
        URI baselineGateway = gatewayUri(baselineNamespace(), Gateway.PUBLIC);
        List<Integer> spent = Requests.statuses(baselineGateway, sharedPath, LIMIT + 1);
        assertEquals(Requests.LIMITED, spent.getLast(), "%s/%s: the budget of %d is not spent after %d requests, statuses %s"
                .formatted(baselineNamespace(), Gateway.PUBLIC.resourceName(), LIMIT, LIMIT + 1, spent));
        assertAll(satelliteGateways.entrySet().stream().map(satellite -> () ->
                assertEquals(Requests.LIMITED, Requests.get(satellite.getValue(), sharedPath).statusCode(),
                        "%s/%s: admits a request after the baseline gateway spent the budget"
                                .formatted(satellite.getKey(), Gateway.PUBLIC.resourceName()))));
    }
}
