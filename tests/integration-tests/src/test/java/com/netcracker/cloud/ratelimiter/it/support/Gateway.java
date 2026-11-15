package com.netcracker.cloud.ratelimiter.it.support;

/**
 * The Cloud-Core gateways that carry a rate limit filter. Every namespace of an environment has both.
 */
public enum Gateway {

    PUBLIC("public-gateway", "public-gateway-service", "gateway.public"),
    PRIVATE("private-gateway", "private-gateway-service", "gateway.private");

    private final String resourceName;
    private final String serviceName;
    private final String domain;

    Gateway(String resourceName, String serviceName, String domain) {
        this.resourceName = resourceName;
        this.serviceName = serviceName;
        this.domain = domain;
    }

    /** The name of the Gateway resource; the EnvoyFilter is named after it. */
    public String resourceName() {
        return resourceName;
    }

    /** The Service in front of the gateway pods, which the tests send requests to. */
    public String serviceName() {
        return serviceName;
    }

    /** The rate limit domain of the filter. The RateLimitPolicy of the gateway has the same name. */
    public String domain() {
        return domain;
    }
}
