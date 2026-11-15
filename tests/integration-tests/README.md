# Cloud-Core integration tests

This Java 21 Maven project tests rate-limiter in a deployed Cloud-Core
environment. It covers baseline gateways and satellites of a composite.
These tests require cluster access and the Cloud-Core test extension available
through the Netcracker Artifactory configured in `config/settings.xml`.

Compile the tests without accessing a cluster:

```bash
mvn -s config/settings.xml test -DskipIT=true
```

Run the baseline tests against an environment in your kubeconfig:

```bash
mvn -s config/settings.xml test -DskipIT=false \
  -Dclouds.cloud.name=<kubeconfig-context> \
  -Dclouds.cloud.namespaces.namespace=<baseline-namespace>
```

To include satellite tests, add `-Dgroups=` and
`-DSATELLITE_1_NAMESPACE=<satellite-namespace>`. An empty group expression runs
both sets; the default selects baseline tests. Tests temporarily modify gateway
rate-limit policies and remove their own limits afterward.

The Dockerfile builds an ATP-compatible test image and prepares dependencies
for execution by the internal test-runner. It compiles the tests during the
image build but selects no tests to execute. The empty chart under
`helm-templates/integration-tests` supplies descriptor metadata; it deploys
no resources.

The GitLab delivery repository keeps the entity pipeline, image publication,
and descriptor integration. It builds this directory from the same pinned
GitHub commit as the operator and service. Moving these sources here does not
add them to the GitHub Actions workflows.
