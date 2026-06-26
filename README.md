# Endpoint-Monitoring Operator

> A lightweight, extensible Kubernetes Operator that probes *any* endpoint-HTTP/JSON, TCP, DNS, ICMP, Trino, OpenSearch, and more-and routes alerts to Slack or e-mail with a simple Custom Resource.  

![Go](https://img.shields.io/badge/Go-%3E%3D1.23-blue?logo=go)
![License](https://img.shields.io/github/license/LiciousTech/endpoint-monitoring-operator)

---

## Why another monitor?

Traditional uptime checkers only tell you if a port is open. **Endpoint-Monitoring Operator** runs *inside* your cluster, so it can:

* Hit real business URLs such as `/v1/status` that are not exposed publicly.  
* Assert deep JSON fields, not just `HTTP 200`.  
* Validate distributed systems (Trino, OpenSearch) and network primitives (DNS, TCP, Ping).  
* Deliver alerts through pluggable notifiers (Slack today, e-mail and PagerDuty soon). :contentReference[oaicite:0]{index=0}

---

## Supported drivers

| Driver        | Typical use-case                                   |
|---------------|----------------------------------------------------|
| `http`        | Basic status-code check (200/302/…​)               |
| `http-json`   | Validate JSON payload & status code                |
| `tcp`         | Verify a service is listening on a port            |
| `dns`         | Ensure a domain resolves to expected IP(s)         |
| `ping`        | Simple ICMP reachability                           |
| `trino`       | Confirm Trino coordinator is *READY*               |
| `opensearch`  | Check cluster health is `green` / `yellow`         |
| `smtp`        | Verify a smtp is listening on a port               |
| `redis`       | Verify redis is listening and responds on a port   |
| `tls`         | Verify tls certificate is on a port                |

Adding a new driver or notifier is only a few lines-everything is wired through a Factory pattern. :contentReference[oaicite:1]{index=1}

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
notify - list of one or more notifiers (Slack, e-mail)
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
    retentionDays: 90
    secretRef:
      name: valkey-credentials  # keys: username (optional), password
```

Deploy one Valkey instance per monitoring location. Operators are location-agnostic; the dashboard app names each Valkey server as a location. Use the same `name` (or monitor name) and `dashboards` across locations for a service to aggregate correctly.

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

* 🔌 Additional notifiers: PagerDuty, OpsGenie, Webhook
* 🗄️ Persistent metrics export (Prometheus CRD)
* 🕵🏻‍♂️ Synthetic transaction scripts (e.g., login + checkout)
* 🔑 Secretless credentials via CSI Drivers
* ➕ **New drivers:** Redis, MySQL, Kafka

Feel free to open an Issue or Pull Request!

## Contributing
1. Fork & clone the repo
2. Create a feature branch
3. Run make test and golangci-lint run

Submit a PR-all contributions welcome!
See CONTRIBUTING.md for details.








