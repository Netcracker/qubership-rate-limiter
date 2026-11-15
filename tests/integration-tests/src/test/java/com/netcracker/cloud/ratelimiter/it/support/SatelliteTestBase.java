package com.netcracker.cloud.ratelimiter.it.support;

import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Tag;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

/**
 * Tests of the satellites of a composite: namespaces that carry only the gateway filters and count in the service
 * of the baseline. Their requests go through the gateways of the satellites.
 * <p>
 * The tags follow mesh-test-app: {@code scenario:composite} says the environment has satellites, and
 * {@code state:baseline-standalone} says its baseline is not a blue-green domain. The class extends
 * {@link RateLimiterTestBase}, not {@link BaselineTestBase}: tags are inherited, and a satellite test must not run
 * with the {@code default} ones.
 * <p>
 * A satellite cannot limit anything while the baseline is down, so these tests are skipped then, with that reason,
 * instead of all failing for it. An environment without satellites is a mistake in the run, and fails.
 */
@Tag("scenario:composite")
@Tag("state:baseline-standalone")
public abstract class SatelliteTestBase extends RateLimiterTestBase {

    @BeforeAll
    protected void requireSatellitesAndReadyBaseline() {
        assertFalse(satelliteNamespaces().isEmpty(), "The satellite tests got no satellite: pass SATELLITE_1_NAMESPACE "
                + "in the test-runner custom parameters, or run only the tag \"default\"");
        List<String> notReady = Resources.DEPLOYMENTS.stream()
                .filter(name -> !deploymentIsReady(baselineNamespace(), name))
                .toList();
        assumeTrue(notReady.isEmpty(), "skipped because the baseline %s is not ready: %s"
                .formatted(baselineNamespace(), String.join(", ", notReady)));
    }
}
