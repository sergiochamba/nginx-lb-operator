package utils

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetServiceEndpoints retrieves the IPs of the pods backing the service.
func GetServiceEndpoints(ctx context.Context, c client.Client, service *corev1.Service) ([]string, error) {
	endpoints := &corev1.Endpoints{}
	err := c.Get(ctx, client.ObjectKey{
		Namespace: service.Namespace,
		Name:      service.Name,
	}, endpoints)
	if err != nil {
		return nil, err
	}

	var ips []string
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.IP != "" {
				ips = append(ips, address.IP)
			}
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no endpoints found for service %s/%s", service.Namespace, service.Name)
	}

	return ips, nil
}

// GetServiceNodeIPs retrieves the IPs of the Kubernetes nodes where the pods backing the service are running.
func GetServiceNodeIPs(ctx context.Context, c client.Client, service *corev1.Service) ([]string, error) {
	endpoints := &corev1.Endpoints{}
	err := c.Get(ctx, client.ObjectKey{
		Namespace: service.Namespace,
		Name:      service.Name,
	}, endpoints)
	if err != nil {
		return nil, err
	}

	var nodeIPs []string
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.NodeName != nil {
				node, err := getNodeIP(ctx, c, *address.NodeName)
				if err != nil {
					return nil, fmt.Errorf("failed to get node IP for node %s: %w", *address.NodeName, err)
				}
				nodeIPs = append(nodeIPs, node)
			}
		}
	}

	if len(nodeIPs) == 0 {
		return nil, fmt.Errorf("no node IPs found for service %s/%s", service.Namespace, service.Name)
	}

	return nodeIPs, nil
}

// getNodeIP fetches the IP of the node with the given name.
func getNodeIP(ctx context.Context, c client.Client, nodeName string) (string, error) {
	node := &corev1.Node{}
	err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node)
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return address.Address, nil
		}
	}

	return "", fmt.Errorf("no InternalIP found for node %s", nodeName)
}
