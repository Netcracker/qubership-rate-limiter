package com.netcracker.cloud.ratelimiter.it.support;

import io.fabric8.kubernetes.api.model.GenericKubernetesResource;
import io.fabric8.kubernetes.api.model.GenericKubernetesResourceBuilder;
import io.fabric8.kubernetes.client.KubernetesClient;
import io.fabric8.kubernetes.client.dsl.Resource;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.EnumMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;

/**
 * The rate limits a test class adds for its own requests and removes afterwards.
 * <p>
 * A gateway has one rate limit domain, and a domain has one RateLimitPolicy, named after it, in the baseline. So a
 * test cannot bring a policy of its own: it adds limits to the policy of the gateway and takes only those out again.
 * <p>
 * Each limit covers a path under {@value #PATH_ROOT} that no service has, so no real request ever matches it. A
 * gateway that admits a request to such a path answers 404. Every run uses paths and limit names of its own, which
 * gives it fresh counters.
 */
public final class TestLimits {

    private static final String NAME_PREFIX = "integration-tests-";
    private static final String PATH_ROOT = "/rate-limiter-integration-tests/";
    private static final int PERIOD_SECONDS = 3600;
    // The service picks a new policy up on its own schedule; on saas-rnd-cap-03 that takes about 70 seconds.
    private static final Duration READY_TIMEOUT = Duration.ofMinutes(3);
    private static final Duration POLL_INTERVAL = Duration.ofSeconds(2);

    private final KubernetesClient client;
    private final String namespace;
    private final String run = Long.toString(System.currentTimeMillis(), 36);
    private final Map<Gateway, List<Map<String, Object>>> added = new EnumMap<>(Gateway.class);

    public TestLimits(KubernetesClient client, String baselineNamespace) {
        this.client = client;
        this.namespace = baselineNamespace;
    }

    /**
     * Registers a limit of {@code requests} per hour at a gateway and returns the path it covers. Nothing reaches
     * the cluster before {@link #apply()}.
     */
    public String add(Gateway gateway, String name, int requests) {
        String path = path(name);
        added.computeIfAbsent(gateway, key -> new ArrayList<>()).add(Map.of(
                "name", NAME_PREFIX + run + "-" + name,
                "target", Map.of("routes", List.of(Map.of("path", Map.of("type", "Prefix", "value", path)))),
                "rules", List.of(Map.of(
                        "name", "ceiling",
                        "behavior", "Enforce",
                        "counters", List.of(),
                        "rates", List.of(Map.of("requests", requests, "periodSeconds", PERIOD_SECONDS))))));
        return path;
    }

    /** A path of this run that no limit covers. */
    public String unlimitedPath() {
        return path("unlimited");
    }

    /** Writes the limits into the policies and waits until every replica of the service enforces them. */
    public void apply() {
        added.forEach((gateway, limits) -> {
            GenericKubernetesResource existing = policy(gateway).get();
            List<Map<String, Object>> all = new ArrayList<>(limitsOfOthers(existing));
            all.addAll(limits);
            write(gateway, existing, all);
        });
        added.keySet().forEach(this::awaitReady);
    }

    /**
     * Takes the limits of this run out of the policies, together with what an interrupted earlier run left behind.
     * A policy that held nothing else is deleted.
     */
    public void remove() {
        for (Gateway gateway : added.keySet()) {
            GenericKubernetesResource existing = policy(gateway).get();
            if (existing == null) {
                continue;
            }
            List<Map<String, Object>> others = limitsOfOthers(existing);
            if (others.isEmpty()) {
                policy(gateway).delete();
            } else {
                write(gateway, existing, others);
            }
        }
    }

    private String path(String name) {
        return PATH_ROOT + run + "/" + name;
    }

    private Resource<GenericKubernetesResource> policy(Gateway gateway) {
        return client.genericKubernetesResources(Resources.POLICY_API, Resources.POLICY_KIND)
                .inNamespace(namespace).withName(gateway.domain());
    }

    private void write(Gateway gateway, GenericKubernetesResource existing, List<Map<String, Object>> limits) {
        GenericKubernetesResource policy = existing != null ? existing : new GenericKubernetesResourceBuilder()
                .withApiVersion(Resources.POLICY_API).withKind(Resources.POLICY_KIND)
                .withNewMetadata().withName(gateway.domain()).withNamespace(namespace).endMetadata()
                .build();
        Map<String, Object> spec = new LinkedHashMap<>(section(policy, "spec"));
        spec.put("domain", gateway.domain());
        spec.put("limits", limits);
        policy.setAdditionalProperty("spec", spec);
        var policies = client.genericKubernetesResources(Resources.POLICY_API, Resources.POLICY_KIND).inNamespace(namespace);
        if (existing == null) {
            policies.resource(policy).create();
        } else {
            policies.resource(policy).update();
        }
    }

    // A policy is ready when the service reports that all its replicas enforce the generation just written.
    private void awaitReady(Gateway gateway) {
        Instant deadline = Instant.now().plus(READY_TIMEOUT);
        String seen = "no status yet";
        while (Instant.now().isBefore(deadline)) {
            GenericKubernetesResource policy = policy(gateway).get();
            if (policy != null) {
                Map<String, Object> status = section(policy, "status");
                Object generation = policy.getMetadata().getGeneration();
                boolean observed = generation != null && status.get("observedGeneration") instanceof Number number
                        && number.longValue() == ((Number) generation).longValue();
                List<Map<String, Object>> conditions = list(status.get("conditions"));
                boolean ready = conditions.stream().anyMatch(condition ->
                        "Ready".equals(condition.get("type")) && "True".equals(condition.get("status")));
                if (observed && ready) {
                    return;
                }
                seen = conditions.stream()
                        .map(condition -> "%s=%s (%s)".formatted(condition.get("type"), condition.get("status"), condition.get("message")))
                        .reduce((left, right) -> left + ", " + right).orElse(seen);
            }
            sleep();
        }
        throw new IllegalStateException("%s: RateLimitPolicy %s is not ready after %d seconds: %s"
                .formatted(namespace, gateway.domain(), READY_TIMEOUT.toSeconds(), seen));
    }

    private static List<Map<String, Object>> limitsOfOthers(GenericKubernetesResource policy) {
        if (policy == null) {
            return List.of();
        }
        return list(section(policy, "spec").get("limits")).stream()
                .filter(limit -> !Objects.toString(limit.get("name"), "").startsWith(NAME_PREFIX))
                .toList();
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> section(GenericKubernetesResource resource, String name) {
        return resource.getAdditionalProperties().get(name) instanceof Map<?, ?> map ? (Map<String, Object>) map : Map.of();
    }

    @SuppressWarnings("unchecked")
    private static List<Map<String, Object>> list(Object value) {
        return value instanceof List<?> list ? (List<Map<String, Object>>) list : List.of();
    }

    private static void sleep() {
        try {
            Thread.sleep(POLL_INTERVAL.toMillis());
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while waiting for a RateLimitPolicy", e);
        }
    }
}
