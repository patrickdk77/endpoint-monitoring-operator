# Endpoint-Monitoring Operator

> A Kubernetes operator that checks HTTP and JSON endpoints, TCP ports, DNS, TLS certificates, SMTP, Redis, MySQL, MariaDB, PostgreSQL, Trino and OpenSearch from inside your cluster, and sends alerts to Slack, Discord, e-mail or a webhook. Each check is one EndpointMonitor resource.

![Go](https://img.shields.io/badge/Go-%3E%3D1.27.2-blue?logo=go)
![License](https://img.shields.io/github/license/LiciousTech/endpoint-monitoring-operator)

---

## Why another monitor?

Traditional uptime checkers only tell you if a port is open. **Endpoint-Monitoring Operator** runs *inside* your cluster, so it can:

* Hit real business URLs such as `/v1/status` that are not exposed publicly.  
* Assert deep JSON fields, not just `HTTP 200`.  
* Check Trino and OpenSearch cluster state, DNS answers, TCP ports and TLS certificates.
* Send alerts to Slack, Discord, e-mail (SMTP or SES) or any webhook, and record results in Valkey for a status page.

---

## Supported drivers

| Driver        | Typical use-case                                   |
|---------------|----------------------------------------------------|
| `http`        | Any 2xx after following redirects; optional body match and certificate expiry |
| `http-json`   | Validate JSON payload & status code                |
| `tcp`         | Verify a service is listening on a port            |
| `dns`         | Ensure a domain resolves to expected IP(s)         |
| `ping`        | TCP connect to port 80, or the port in the endpoint; not ICMP |
| `trino`       | Confirm Trino coordinator is *READY*               |
| `opensearch`  | Cluster health is `green`; `yellow` and `red` fail |
| `smtp`        | SMTP greeting, with optional HELO, STARTTLS or TLS, AUTH, VRFY, MAIL FROM and RCPT TO |
| `redis`       | PING answered with PONG, after AUTH if the secret holds a password |
| `tls`         | TLS handshake with certificate verification, and an optional expiry window |
| `mysql`       | Log in, check read_only, connection count, replica lag |
| `mariadb`     | The `mysql` checks against a MariaDB server        |
| `postgres`    | Log in; a primary must be writable, a replica streaming and within its lag |

`pkg/factory/factory.go` builds every driver and notifier. CONTRIBUTING.md lists the steps for adding one.

---

## CRD quick-look

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: my-check
spec:
  driver: http-json                 # see table above
  endpoint: https://api.example.com/v1/status
  checkInterval: 30                 # seconds
  httpJsonCheck:                    # driver-specific section
    expectedStatusCode: 200
    jsonAssertions:
      status: "UP"
      version: "v1.2.3"
  notify:
    slack:
      enabled: true
      webhookUrl: https://hooks.slack.com/services/XXX/YYY/ZZZ
      alertOn:                       # optional - defaults to ["failure"]
        - success
        - failure
```

Key fields in the spec (abridged):

```
driver - which probe implementation to use
endpoint - URL/host/cluster depending on driver
checkInterval - seconds between probes
ipFamily - optional list; [ipv4] or [ipv6] probes over that family only (direct, no HTTP proxy), [ipv4, ipv6] probes over both and fails if either fails
notify - one or more notifiers: slack, email, discord, webhook, valkey
Driver-specific blocks - e.g. httpJsonCheck for http-json driver
```

See the Go type definitions for the full schema.

## Installation (one-liner)

```kubectl apply -f https://raw.githubusercontent.com/LiciousTech/endpoint-monitoring-operator/main/dist/install.yaml```

### Quick-start examples

#### 1. Monitor DNS resolution
```kubectl apply -f examples/dns.yaml```

#### 2. Deep health-check on a JSON endpoint
```kubectl apply -f examples/http-json.yaml```

#### 3. Publish check results to Valkey for status dashboards
```kubectl apply -f examples/valkey-secret.yaml -f examples/valkey.yaml```

---

## Valkey status notifier

The `valkey` notifier writes per-check samples into a co-located Valkey 9.x instance using `HSET` with per-field expiration (`HEXPIRE`). Each monitor can publish to one or more named dashboards.

```yaml
notify:
  valkey:
    enabled: true
    endpoint: valkey.example.com:6379
    tls: true
    db: 0
    dashboards:
      - platform
    name: api-status          # optional; keep identical across locations
    retentionDays: 7
    secretRef:
      name: valkey-credentials  # keys: username (optional), password
```

Deploy one Valkey instance per monitoring location. Operators are location-agnostic; the dashboard app names each Valkey server as a location. Use the same `name` (or monitor name) and `dashboards` across locations for a service to aggregate correctly.

`name` and each dashboard name must not contain `:`, because the Valkey keys use it as a separator. `retentionDays` is how long each raw check sample is kept; it must be at least 1 and defaults to 7. The dashboard builds hourly and daily rollups from these samples and keeps those for its own, longer retention.

---

## Status dashboard app

The `dashboard/` module is a separate Go application that:

* Reads raw samples from every configured location Valkey (read-only)
* Rolls up hourly per-location and cross-location summaries into its primary Valkey
* Serves githubstatus-style overview and detail pages over HTTP

Configure locations in `dashboard/config.example.yaml` (exactly one `primary: true` writable instance):

```yaml
locations:
  - name: us-east
    addr: valkey-us-east.example.com:6379
    tls: true
    username: default
    password: changeme
    db: 0
    primary: true
  - name: eu-west
    addr: valkey-eu-west.example.com:6379
    tls: true
    primary: false
```

Build with:

```bash
cd dashboard && go build -o dashboard ./cmd/dashboard
```

For Kubernetes, the manifests are split so the app and its per-location config can be deployed independently:

* `dashboard/deploy/deployment.yaml` - the reusable Deployment + Service
* `dashboard/deploy/config.yaml` - the per-location ConfigMap + Secret (edit per site)

```bash
kubectl apply -f dashboard/deploy/config.yaml -f dashboard/deploy/deployment.yaml
```

---

## Supported notifiers

| Notifier | Purpose |
|----------|---------|
| `slack` | Slack webhook alerts |
| `email` | Email alerts (SES/SMTP) |
| `discord` | Discord webhook alerts |
| `webhook` | Custom HTTP webhook |
| `valkey` | Status-page time series storage |

---

## Roadmap

* Additional notifiers: PagerDuty, OpsGenie
* Persistent metrics export (Prometheus CRD)
* Synthetic transaction scripts (e.g., login + checkout)
* Secretless credentials via CSI Drivers
* **New drivers:** MongoDB, ScyllaDB, Kafka

Feel free to open an Issue or Pull Request!

## Contributing
1. Fork & clone the repo
2. Create a feature branch
3. Run make test and golangci-lint run

Submit a PR. All contributions welcome!
See CONTRIBUTING.md for details.








