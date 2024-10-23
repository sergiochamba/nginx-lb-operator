package utils

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ipAllocationMutex sync.Mutex
)

// AllocateIP allocates an IP address for the given service.
func AllocateIP(ctx context.Context, c client.Client, service *corev1.Service) (string, error) {
	ipAllocationMutex.Lock()
	defer ipAllocationMutex.Unlock()

	ipPool, err := LoadIPPool(ctx, c)
	if err != nil {
		return "", err
	}

	allocatedIPs, err := LoadAllocatedIPs(ctx, c)
	if err != nil {
		return "", err
	}

	svcIdentifier := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	// Allocate an IP
	for _, ip := range ipPool {
		if _, allocated := allocatedIPs[ip]; !allocated {
			// Mark IP as allocated
			allocatedIPs[ip] = svcIdentifier
			if err := SaveAllocatedIPs(ctx, c, allocatedIPs); err != nil {
				return "", err
			}
			return ip, nil
		}
	}

	return "", fmt.Errorf("no available IPs in the pool")
}

// LoadIPPool loads the IP pool from the ConfigMap.
func LoadIPPool(ctx context.Context, c client.Client) ([]string, error) {
	configMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{Name: "ip-pool-config", Namespace: "nginx-lb-operator-system"}, configMap)
	if err != nil {
		return nil, fmt.Errorf("failed to load IP pool config: %w", err)
	}

	ipPoolData, ok := configMap.Data["ip_pool"]
	if !ok {
		return nil, fmt.Errorf("ip_pool not found in ConfigMap")
	}

	// Parse IPs and ranges
	ipPool := []string{}
	lines := strings.Split(ipPoolData, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "-") {
			// IP range
			ips, err := parseIPRange(line)
			if err != nil {
				return nil, fmt.Errorf("failed to parse IP range '%s': %w", line, err)
			}
			ipPool = append(ipPool, ips...)
		} else {
			// Single IP
			ip := net.ParseIP(line)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP address '%s'", line)
			}
			ipPool = append(ipPool, ip.String())
		}
	}
	return ipPool, nil
}

// parseIPRange parses a range like "10.1.1.60 - 10.1.1.65".
func parseIPRange(rangeStr string) ([]string, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid IP range format")
	}
	startIP := net.ParseIP(strings.TrimSpace(parts[0]))
	endIP := net.ParseIP(strings.TrimSpace(parts[1]))
	if startIP == nil || endIP == nil {
		return nil, fmt.Errorf("invalid IP in range")
	}
	ips := []string{}
	for ip := startIP; !ipAfter(ip, endIP); ip = nextIP(ip) {
		ips = append(ips, ip.String())
	}
	return ips, nil
}

// ipAfter checks if ip1 is after ip2.
func ipAfter(ip1, ip2 net.IP) bool {
	return bytesCompare(ip1.To4(), ip2.To4()) > 0
}

// bytesCompare compares two byte slices.
func bytesCompare(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return len(a) - len(b)
}

// nextIP calculates the next IP address.
func nextIP(ip net.IP) net.IP {
	ip = ip.To4()
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
	return ip
}

// LoadAllocatedIPs loads allocated IPs from the ConfigMap.
func LoadAllocatedIPs(ctx context.Context, c client.Client) (map[string]string, error) {
	configMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{Name: "ip-allocations", Namespace: "nginx-lb-operator-system"}, configMap)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			// ConfigMap not found, return empty map
			return make(map[string]string), nil
		}
		return nil, fmt.Errorf("failed to load allocated IPs: %w", err)
	}

	allocatedIPs := make(map[string]string)
	for ip, svc := range configMap.Data {
		allocatedIPs[ip] = svc
	}
	return allocatedIPs, nil
}

// SaveAllocatedIPs saves allocated IPs to the ConfigMap.
func SaveAllocatedIPs(ctx context.Context, c client.Client, allocatedIPs map[string]string) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ip-allocations",
			Namespace: "nginx-lb-operator-system",
		},
		Data: allocatedIPs,
	}

	err := c.Update(ctx, configMap)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Create(ctx, configMap)
		}
		return fmt.Errorf("failed to save allocated IPs: %w", err)
	}
	return nil
}

// ReleaseIP releases an IP associated with a service.
func ReleaseIP(ctx context.Context, c client.Client, service *corev1.Service) error {
	ipAllocationMutex.Lock()
	defer ipAllocationMutex.Unlock()

	allocatedIPs, err := LoadAllocatedIPs(ctx, c)
	if err != nil {
		return err
	}

	// Find and remove the IP associated with the service
	svcIdentifier := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	ipFound := false
	for ip, svc := range allocatedIPs {
		if svc == svcIdentifier {
			delete(allocatedIPs, ip)
			ipFound = true
			break
		}
	}

	if !ipFound {
		return fmt.Errorf("no IP allocation found for service %s", svcIdentifier)
	}

	if err := SaveAllocatedIPs(ctx, c, allocatedIPs); err != nil {
		return err
	}

	return nil
}

// IsIPAllocatedToService checks if the service already has an IP allocated.
func IsIPAllocatedToService(ctx context.Context, c client.Client, service *corev1.Service) (bool, error) {
	allocatedIPs, err := LoadAllocatedIPs(ctx, c)
	if err != nil {
		return false, err
	}

	svcIdentifier := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	for _, svc := range allocatedIPs {
		if svc == svcIdentifier {
			return true, nil
		}
	}
	return false, nil
}

// GetAllocatedIPForService retrieves the IP allocated to the service.
func GetAllocatedIPForService(ctx context.Context, c client.Client, service *corev1.Service) (string, error) {
	allocatedIPs, err := LoadAllocatedIPs(ctx, c)
	if err != nil {
		return "", err
	}

	svcIdentifier := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	for ip, svc := range allocatedIPs {
		if svc == svcIdentifier {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no IP allocated for service %s", svcIdentifier)
}
