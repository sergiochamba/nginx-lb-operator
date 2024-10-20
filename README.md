# NGINX Load Balancer Operator

This operator manages LoadBalancer services in a Kubernetes cluster by configuring an external NGINX server to handle traffic.

## Features

- Allocates IPs from a configurable IP pool.
- Generates NGINX and Keepalived configurations, including the cluster name to avoid conflicts.
- Balances provisioned IPs among active and standby groups.
- Handles service creation, update, and deletion.
- Remains stateless and reconciles state on restarts.

## Requirements

- Kubernetes cluster.
- External NGINX server(s) with SSH access.
- Keepalived installed on NGINX server(s).
- SSH keys and known hosts configured.

## Configuration

### IP Pool

Define the IP pool in `config/ip-pool-config.yaml`.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ip-pool-config
  namespace: nginx-lb-operator-system
data:
  ip_pool: |
    # Single IPs
    10.1.1.55
    10.1.1.56
    # IP Range
    10.1.1.60 - 10.1.1.65
