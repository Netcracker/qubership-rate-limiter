package com.netcracker.cloud.ratelimiter.it.support;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.List;
import java.util.stream.IntStream;

/**
 * Requests through a gateway. A gateway answers 429 to a request it limits; any other status means the rate limit
 * filter let the request through, whatever the gateway did with it next.
 */
public final class Requests {

    public static final int LIMITED = 429;

    private static final HttpClient CLIENT = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build();

    private Requests() {
    }

    public static HttpResponse<Void> get(URI gateway, String path) {
        HttpRequest request = HttpRequest.newBuilder(gateway.resolve(path)).timeout(Duration.ofSeconds(15)).GET().build();
        try {
            return CLIENT.send(request, HttpResponse.BodyHandlers.discarding());
        } catch (IOException e) {
            throw new IllegalStateException("GET %s failed: %s".formatted(request.uri(), e.getMessage()), e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted during GET " + request.uri(), e);
        }
    }

    /** Sends {@code count} requests one after another and returns their statuses in order. */
    public static List<Integer> statuses(URI gateway, String path, int count) {
        return IntStream.range(0, count).map(i -> get(gateway, path).statusCode()).boxed().toList();
    }
}
