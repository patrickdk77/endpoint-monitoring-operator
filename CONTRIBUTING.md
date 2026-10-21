# Contributing to Endpoint Monitoring Operator

First off, thank you for considering contributing to the endpoint-monitoring-operator! 
This project is designed to be extensible, reliable, and easy to build upon.

## Overview

This is a Kubernetes operator that lets you declaratively monitor external services like: HTTP / HTTP-JSON APIs, TCP ports, DNS resolutions, Trino clusters, OpenSearch, Ping checks and more.

It follows the Factory Design Pattern to easily support new drivers (monitoring mechanisms) and notifiers (e.g., Slack, email, etc.).


## Project Structure

```text
api/                      # CRD schema definitions
config/                   # Install manifests, RBAC, kustomize
internal/
  controller/             # Core reconciliation logic
  driver/                 # All drivers go here (http, dns, ping, etc.)
  notifier/               # One package per notifier (slack, email, etc.)
pkg/
  factory/                # Driver/Notifier creation logic
dist/                     # install.yaml and release manifests
```


## Add a New Driver

To monitor a new type of system (e.g., Kafka, PostgreSQL, MongoDB):

Create a new Go file in `internal/driver/<yourdriver>.go`

Implement the Driver interface:

```go
type Driver interface {
    Check() (*CheckResult, error)
    GetEndpoint() string
    GetType() string
}
```

Have the constructor take the monitor's address family (`""`, `ipv4` or `ipv6`) and open every connection to the endpoint through `familyNetwork` or `httpClient` in `internal/driver/family.go`, so `spec.ipFamily` applies to the new driver.

If the endpoint is a host or host:port, call `checkHostEndpoint` from `internal/driver/endpoint.go` in the constructor and add the constructor to `TestHostDriversRejectURLAndUserEndpoints`. An endpoint with `://` or `@` then fails with a fixed error instead of carrying its credentials into the check text.

Add your driver to `createFamilyDriver` in `pkg/factory/factory.go`. When `ipFamily` lists both `ipv4` and `ipv6`, the factory builds it once per family.

Add the driver's name to the `Enum` marker on `EndpointMonitorSpec.Driver` in `api/v1alpha1/endpointmonitor_types.go` and run `make manifests`. The API server rejects any driver name missing from that list.

Add the driver to `testEndpoints` in `pkg/factory/factory_test.go`. `TestCRDDriverEnumMatchesFactory` fails for any driver in the CRD enum that is missing there.

If the driver talks to a server that runs in a container, add its images to `test/docker` and run `make test-docker`. Those tests start each image with Docker and check the driver against it. The `docker` build tag keeps them out of `make test`.

Each run labels the containers and networks it creates `emo-e2e`, and removes them when it finishes or gets Ctrl-C. A second Ctrl-C, or the run's `-timeout`, ends it before it can, so remove what it left once no other run is going:

```bash
docker rm -f -v $(docker ps -aq --filter label=emo-e2e)
docker network rm $(docker network ls -q --filter label=emo-e2e)
```

## Add a New Notifier

Want to send alerts to PagerDuty or OpsGenie?

Add a package in `internal/notifier/<notifier>/` whose `New` takes the notifier's config struct and returns `(notifier.Notifier, error)`. Check the config in `New`. An error there stops the monitor with a CannotCheck event before any check runs.

Implement the Notifier interface:

```go
type Notifier interface {
    SendAlert(status string, values *NoticeValues, client client.Client) error
}
```

`New` gets no Kubernetes client, so read any Secret in `SendAlert` with `notifier.GetSecret`.

Add the config struct and a field for it in `NotifyConfig` in `api/v1alpha1/endpointmonitor_types.go`, then run `make generate manifests`.

Build the notifier in `CreateNotifier` in `pkg/factory/factory.go`.


## Code Style

1. Use idiomatic Go formatting (go fmt)

2. Follow Kubernetes naming conventions for types and CRDs

3. Keep logic minimal in the Reconciler - offload work to factory/driver layers

4. No hardcoded secrets/tokens; use Kubernetes SecretRef


## Security & Secrets

Run Trivy to scan for secrets in the image:

```bash
trivy image --scanners secret <your-image>
```

## Pull Requests
Always create a feature branch (e.g., feat/kafka-driver)

Write clear PR titles and descriptions

Link to relevant issues if applicable

Follow conventional commit standards - https://www.conventionalcommits.org/en/v1.0.0/

