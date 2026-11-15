package com.netcracker.cloud.ratelimiter.it.support;

import com.netcracker.cloud.junit.cloudcore.extension.annotations.Cloud;
import com.netcracker.cloud.junit.cloudcore.extension.annotations.EnableExtension;
import com.netcracker.cloud.junit.cloudcore.extension.service.PortForwardService;
import com.netcracker.cloud.junit.cloudcore.extension.service.ServicePortForwardParams;
import io.fabric8.kubernetes.api.model.GenericKubernetesResource;
import io.fabric8.kubernetes.api.model.apps.Deployment;
import io.fabric8.kubernetes.client.KubernetesClient;
import org.junit.jupiter.api.function.Executable;

import java.net.URI;
import java.net.URISyntaxException;
import java.util.List;
import java.util.Objects;
import java.util.Optional;
import java.util.stream.Stream;

import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * What every test needs: the client, the namespaces of the environment, and a way to reach a gateway.
 * <p>
 * test-runner points the client at the namespace of the deployment: the only namespace of a single-namespace
 * environment, or the baseline of a composite. The satellite to test comes in as the
 * {@code SATELLITE_1_NAMESPACE} property of the test-runner custom parameters; a single-namespace environment
 * has none. One satellite is enough: every satellite of a composite gets the same gateway filters.
 * <p>
 * Tests do not extend this class directly. {@link BaselineTestBase} and {@link SatelliteTestBase} add the tags that
 * select them and what their part of the environment needs.
 */
@EnableExtension
public abstract class RateLimiterTestBase {

    private static final String SATELLITE_PROPERTY = "SATELLITE_1_NAMESPACE";
    private static final int GATEWAY_PORT = 8080;

    @Cloud
    protected KubernetesClient kubernetesClient;

    @Cloud
    protected PortForwardService portForwardService;

    protected String baselineNamespace() {
        return kubernetesClient.getNamespace();
    }

    protected List<String> satelliteNamespaces() {
        return parameter(SATELLITE_PROPERTY).stream().toList();
    }

    /**
     * Opens a port-forward to the gateway of a namespace and returns its address. The extension closes the
     * port-forwards after the class.
     */
    protected URI gatewayUri(String namespace, Gateway gateway) {
        try {
            return portForwardService.portForward(ServicePortForwardParams
                    .builderAsUrl(gateway.serviceName(), GATEWAY_PORT, "http").namespace(namespace).build()).toURI();
        } catch (URISyntaxException e) {
            throw new IllegalStateException("%s/%s: the port-forward returned an address that is not a URI"
                    .formatted(namespace, gateway.serviceName()), e);
        }
    }

    protected boolean deploymentIsReady(String namespace, String name) {
        Deployment deployment = kubernetesClient.apps().deployments().inNamespace(namespace).withName(name).get();
        return deployment != null && deployment.getStatus() != null
                && Objects.equals(deployment.getSpec().getReplicas(), deployment.getStatus().getReadyReplicas());
    }

    /**
     * The check that a gateway of a namespace has its rate limit filter and that the filter calls the service of
     * the baseline. A deploy into a namespace without Cloud-Core succeeds too: the EnvoyFilter attaches to the
     * Gateway by name and silently matches nothing. The check catches that by looking for the Gateway as well.
     */
    protected Executable gatewayIsWiredToBaselineService(String namespace, Gateway gateway) {
        return () -> {
            String where = "%s/%s".formatted(namespace, gateway.resourceName());
            String rlsCluster = "outbound|%d||%s.%s.svc.cluster.local"
                    .formatted(Resources.GRPC_PORT, Resources.SERVICE, baselineNamespace());
            assertNotNull(get(Resources.GATEWAY_API, Resources.GATEWAY_KIND, namespace, gateway.resourceName()),
                    "%s: Gateway is missing, is Cloud-Core deployed here?".formatted(where));
            String filterName = "%s-%s".formatted(Resources.FILTER_OWNER, gateway.resourceName());
            GenericKubernetesResource filter = get(Resources.ENVOY_FILTER_API, Resources.ENVOY_FILTER_KIND, namespace, filterName);
            assertNotNull(filter, "%s: EnvoyFilter %s is missing".formatted(where, filterName));
            String spec = kubernetesClient.getKubernetesSerialization().asJson(filter.getAdditionalProperties().get("spec"));
            assertTrue(spec.contains("\"name\":\"%s\"".formatted(gateway.resourceName())),
                    "%s: EnvoyFilter %s does not target the Gateway".formatted(where, filterName));
            assertTrue(spec.contains(rlsCluster),
                    "%s: EnvoyFilter %s does not call %s".formatted(where, filterName, rlsCluster));
        };
    }

    protected GenericKubernetesResource get(String apiVersion, String kind, String namespace, String name) {
        return kubernetesClient.genericKubernetesResources(apiVersion, kind).inNamespace(namespace).withName(name).get();
    }

    // test-runner passes custom parameters as system properties and, in the docker mode, as environment variables.
    private static Optional<String> parameter(String name) {
        return Stream.of(System.getProperty(name), System.getenv(name))
                .filter(value -> value != null && !value.isBlank())
                .findFirst();
    }
}
