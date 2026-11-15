FROM artifactorycn.netcracker.com:17114/netcracker/qubership-java-base-atp:25-alpine-2.5.9

WORKDIR /app

# Copy only pom.xml first to cache dependency downloads as a separate layer
COPY pom.xml .
COPY config/settings.xml .
ENV MAVEN_ARGS="${MAVEN_ARGS} -s /app/settings.xml"

# Download all dependencies into the local repo for offline use
RUN mvn -B -U dependency:go-offline
# Copy sources
COPY src ./src

# Warm the complete runtime lifecycle online without executing integration tests.
RUN mvn -B -U verify \
    -DskipIT=false \
    '-Dgroups=default & !default' \
    -DtestFailureIgnore=false
