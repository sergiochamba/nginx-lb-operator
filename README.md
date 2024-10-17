# NGINX Load Balancer Operator

This operator watches for Kubernetes Services of type `LoadBalancer` and configures an external NGINX load balancer accordingly.

## **Features**

- Assigns IPs from a predefined IP pool to services after successful configuration.
- Configures NGINX to route traffic to the service endpoints.
- Adds assigned IPs to the NGINX server's network interface.
- Allows sharing of IPs across services using different ports.
- Updates configurations when services, endpoints, or nodes change.
- Cleans up configurations when services are deleted.
- Implements persistent IP allocations using ConfigMaps.

## **Prerequisites**

- Kubernetes cluster
- Operator SDK installed
- Access to an NGINX server with SSH
- SSH key-based authentication configured
- NGINX server's SSH public key added to known hosts

## **Setup**

1. **Clone the repository:**

   ```bash
   git clone https://github.com/sergiochamba/nginx-lb-operator.git
   cd nginx-lb-operator
   ```

2. **Build the operator:**

   ```bash
   make docker-build docker-push
   ```

3. **Deploy the operator:**

   ```bash
   make deploy
   ```
