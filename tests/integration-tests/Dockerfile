FROM artifactorycn.netcracker.com:17114/netcracker/qubership-java-base-atp:25-alpine-2.5.9

WORKDIR /app

# Copy only pom.xml first to cache dependency downloads as a separate layer
COPY pom.xml .
COPY config/settings.xml .
ENV MAVEN_ARGS="${MAVEN_ARGS} -s /app/settings.xml"

# Maven Resolver must explicitly enable JVM proxy properties from the build proxy.
# Apply them only to build commands; do not bake the local proxy into the image.
# Download all dependencies into the local repo for offline use
RUN proxy="${HTTPS_PROXY:-${HTTP_PROXY:-}}"; \
    if [ -n "$proxy" ]; then \
      proxy="${proxy#*://}"; \
      export MAVEN_OPTS="${MAVEN_OPTS:-} -Daether.connector.http.useSystemProperties=true -Daether.transport.apache.useSystemProperties=true -Dhttp.proxyHost=${proxy%:*} -Dhttp.proxyPort=${proxy##*:} -Dhttps.proxyHost=${proxy%:*} -Dhttps.proxyPort=${proxy##*:}"; \
    fi; \
    mvn -B -U dependency:go-offline
# Copy sources
COPY src ./src

# Warm the complete runtime lifecycle online without executing integration tests.
RUN proxy="${HTTPS_PROXY:-${HTTP_PROXY:-}}"; \
    if [ -n "$proxy" ]; then \
      proxy="${proxy#*://}"; \
      export MAVEN_OPTS="${MAVEN_OPTS:-} -Daether.connector.http.useSystemProperties=true -Daether.transport.apache.useSystemProperties=true -Dhttp.proxyHost=${proxy%:*} -Dhttp.proxyPort=${proxy##*:} -Dhttps.proxyHost=${proxy%:*} -Dhttps.proxyPort=${proxy##*:}"; \
    fi; \
    mvn -B -U verify \
    -DskipIT=false \
    '-Dgroups=default & !default' \
    -DtestFailureIgnore=false
