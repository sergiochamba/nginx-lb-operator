# NGINX Load Balancer Operator

This operator watches for Kubernetes Services of type `LoadBalancer` and configures an external NGINX load balancer accordingly.

## **Features**

- Assigns IPs from a predefined IP pool to services.
- Configures NGINX with the service endpoints.
- Adds assigned IPs to the NGINX server's network interface.
- Allows sharing of IPs across services using different ports.
- Updates configurations when services or endpoints change.
- Cleans up configurations when services are deleted.

## **Prerequisites**

- Kubernetes cluster
- Operator SDK installed
- Access to an NGINX server with SSH

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
