package com.netcracker.cloud.ratelimiter.it.support;

import org.junit.jupiter.api.Tag;

/**
 * Tests of the namespace that runs the operator and the service: the baseline of a composite, or the only
 * namespace of a single-namespace environment. Their requests go through the gateways of that namespace.
 * <p>
 * They pass on any environment with rate-limiter, so they carry the tag {@code default}, as the basic tests of
 * mesh-test-app do. A run without {@code -Dgroups} selects it: the default in {@code pom.xml} is
 * {@code none() | default}.
 */
@Tag("default")
public abstract class BaselineTestBase extends RateLimiterTestBase {
}
