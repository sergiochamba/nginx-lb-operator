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
