package com.netcracker.cloud.ratelimiter.it.support;

import java.util.List;

/**
 * What rate-limiter installs and the API kinds the tests read through the generic client, as the cluster serves them.
 */
public final class Resources {

    public static final String POLICY_API = "ratelimit.netcracker.com/v1";
    public static final String POLICY_KIND = "RateLimitPolicy";

    public static final String GATEWAY_API = "gateway.networking.k8s.io/v1";
    public static final String GATEWAY_KIND = "Gateway";

    public static final String ENVOY_FILTER_API = "networking.istio.io/v1alpha3";
    public static final String ENVOY_FILTER_KIND = "EnvoyFilter";
    /** The chart that renders the gateway filters; a filter is named {@code <chart>-<gateway>}. */
    public static final String FILTER_OWNER = "rate-limiter";

    public static final List<String> DEPLOYMENTS = List.of("rate-limiter-operator", "rate-limiter");
    public static final String SERVICE = "rate-limiter";
    public static final int GRPC_PORT = 9000;

    private Resources() {
    }
}
