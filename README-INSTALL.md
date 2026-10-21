# Installation

You can install the endpoint-monitoring-operator into your Kubernetes cluster with a single command:

```bash
kubectl apply -f https://raw.githubusercontent.com/LiciousTech/endpoint-monitoring-operator/main/dist/install.yaml
```

This manifest sets up everything needed.

# Example usage

The examples folder in this repo has more.

## 1. DNS

To check that your domain name resolves, use the dns driver.

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: domain-monitor
  namespace: endpoint-monitoring-operator-system
spec:
  driver: dns
  endpoint: your-domain.com
  checkInterval: 30 # in seconds
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - success  # by default alerts only failures.
        - failure 
```

Apply above manifest.


Check status:

```bash
kubectl get endpointmonitor domain-monitor -n endpoint-monitoring-operator-system -o yaml
```

describe:

```bash
kubectl describe endpointmonitor domain-monitor -n endpoint-monitoring-operator-system
```

## 2. HTTP-JSON

Let's say you have a User Service with an endpoint like:

```bash
GET https://api.example.com/v1/status
```

And it responds with:

```json
{
  "status": "UP",
  "service": "user-service",
  "version": "v2.4.1",
  "env": "prod",
  "dependencies": {
    "database": "ok",
    "redis": "ok"
  }
}
```

EndpointMonitor CR for This

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: user-service-healthcheck
  namespace: endpoint-monitoring-operator-system
spec:
  driver: http-json
  endpoint: https://api.example.com/v1/status
  checkInterval: 30 # seconds
  httpJsonCheck:
    expectedStatusCode: 200 # optional
    jsonAssertions:
      status: "UP"
      service: "user-service"
      env: "prod"
      dependencies.database: "ok"
      dependencies.redis: "ok"
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```

## 3. TRINO

Want to ensure your Trino cluster is up and not in a starting state?

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: trino-coordinator-monitor
  namespace: endpoint-monitoring-operator-system
spec:
  checkInterval: 300  # 5 minutes
  driver: trino
  endpoint: http://trino.company.com
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>  # alertOn not specified, so by default only reports failures.
```

## 4. TCP

To check that a MySQL port on RDS accepts connections:

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: rds-mysql-tcp-check
  namespace: endpoint-monitoring-operator-system
spec:
  driver: tcp
  endpoint: mysql.company.com:3306
  checkInterval: 30
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```

## 5. OpenSearch
The opensearch driver reads `/_cluster/health` and passes only when the cluster status is green. Yellow and red both fail.

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: opensearch-cluster-health
  namespace: endpoint-monitoring-operator-system
spec:
  checkInterval: 30
  driver: opensearch
  endpoint: http://cluster01.os01.svc.cluster.local:9200
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```

## 6. HTTP

Use the http driver when you only care about the status code. It follows redirects and passes on any 2xx response.

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: trino-basic-http-check
  namespace: endpoint-monitoring-operator-system
spec:
  checkInterval: 60  # check every 1 minute
  driver: http
  endpoint: https://status.example.com/
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```

## 7. Ping

The ping driver opens a TCP connection to the host, on port 80 unless the endpoint names another port. It does not send ICMP.

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: ping-google
  namespace: endpoint-monitoring-operator-system
spec:
  checkInterval: 60  # every 1 minute
  driver: ping
  endpoint: 8.8.8.8:53
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```

## 8. SMTP

Use the smtp driver when you want to validate an email server is responding.

```yaml
apiVersion: monitoring.licious.app/v1alpha1
kind: EndpointMonitor
metadata:
  name: basic-smtp-check
  namespace: endpoint-monitoring-operator-system
spec:
  checkInterval: 60  # check every 1 minute
  driver: smtp
  endpoint: smtp.example.com
  notify:
    slack:
      enabled: true
      webhookUrl: <slack-webhook-url>
      alertOn:
        - failure
```
