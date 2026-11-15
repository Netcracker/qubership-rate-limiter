package com.netcracker.cloud.ratelimiter.it.baseline;

import com.netcracker.cloud.ratelimiter.it.support.BaselineTestBase;
import com.netcracker.cloud.ratelimiter.it.support.Gateway;
import com.netcracker.cloud.ratelimiter.it.support.Requests;
import com.netcracker.cloud.ratelimiter.it.support.TestLimits;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.net.URI;
import java.net.http.HttpResponse;
import java.util.Arrays;
import java.util.EnumMap;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertAll;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;

/**
 * A limit in the policy of a gateway is what the gateway enforces: requests within it pass, the next one gets 429,
 * and a path next to the limited one is left alone.
 * <p>
 * Each test spends a limit of {@value #LIMIT} requests on paths of its own, one per gateway, so the tests share no
 * budget and run in any order.
 */
@DisplayName("Baseline gateways enforce rate limits")
class BaselineGatewayLimitIT extends BaselineTestBase {

    private static final int LIMIT = 3;

    private final Map<Gateway, URI> gateways = new EnumMap<>(Gateway.class);
    private final Map<Gateway, String> withinPaths = new EnumMap<>(Gateway.class);
    private final Map<Gateway, String> overPaths = new EnumMap<>(Gateway.class);
    private TestLimits limits;

    @BeforeAll
    void addLimits() {
        limits = new TestLimits(kubernetesClient, baselineNamespace());
        for (Gateway gateway : Gateway.values()) {
            gateways.put(gateway, gatewayUri(baselineNamespace(), gateway));
            withinPaths.put(gateway, limits.add(gateway, "within", LIMIT));
            overPaths.put(gateway, limits.add(gateway, "over", LIMIT));
        }
        limits.apply();
    }

    @AfterAll
    void removeLimits() {
        if (limits != null) {
            limits.remove();
        }
    }

    @Test
    @DisplayName("Requests within the limit pass")
    void requestsWithinLimitPass() {
        assertAll(Arrays.stream(Gateway.values()).map(gateway -> () -> {
            List<Integer> statuses = Requests.statuses(gateways.get(gateway), withinPaths.get(gateway), LIMIT);
            assertFalse(statuses.contains(Requests.LIMITED), "%s: the first %d requests got %s, none of them is over the limit"
                    .formatted(where(gateway), LIMIT, statuses));
        }));
    }

    @Test
    @DisplayName("Requests over the limit get 429")
    void requestsOverLimitGet429() {
        assertAll(Arrays.stream(Gateway.values()).map(gateway -> () -> {
            List<Integer> spent = Requests.statuses(gateways.get(gateway), overPaths.get(gateway), LIMIT);
            HttpResponse<Void> response = Requests.get(gateways.get(gateway), overPaths.get(gateway));
            assertEquals(Requests.LIMITED, response.statusCode(), "%s: request %d of a limit of %d, the first %d got %s"
                    .formatted(where(gateway), LIMIT + 1, LIMIT, LIMIT, spent));
            assertEquals("true", response.headers().firstValue("x-envoy-ratelimited").orElse(null),
                    "%s: the 429 does not carry x-envoy-ratelimited, so it is not from the rate limit filter"
                            .formatted(where(gateway)));
        }));
    }

    @Test
    @DisplayName("Requests to other paths pass")
    void requestsToOtherPathsPass() {
        assertAll(Arrays.stream(Gateway.values()).map(gateway -> () ->
                assertNotEquals(Requests.LIMITED, Requests.get(gateways.get(gateway), limits.unlimitedPath()).statusCode(),
                        "%s: a path next to the limited one is limited too".formatted(where(gateway)))));
    }

    private String where(Gateway gateway) {
        return "%s/%s".formatted(baselineNamespace(), gateway.resourceName());
    }
}
