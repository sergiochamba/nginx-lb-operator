package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	vridAllocationMutex sync.Mutex
)

// GetOrAllocateVRIDs retrieves or allocates VRIDs for the cluster.
// VRIDs are allocated once per cluster and are persistent.
func GetOrAllocateVRIDs(ctx context.Context, c client.Client) (int, int, error) {
	vridAllocationMutex.Lock()
	defer vridAllocationMutex.Unlock()

	clusterName := GetClusterName()

	// Attempt to retrieve existing VRIDs from ConfigMap
	vridConfigMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      "vrid-allocations",
		Namespace: "nginx-lb-operator-system",
	}, vridConfigMap)
	if err != nil && client.IgnoreNotFound(err) != nil {
		return 0, 0, fmt.Errorf("failed to get VRID allocations: %w", err)
	}

	if err == nil {
		// ConfigMap exists, check if VRIDs are allocated for this cluster
		if vridStr, exists := vridConfigMap.Data[clusterName]; exists {
			// VRIDs are already allocated
			vrids := parseVRIDPair(vridStr)
			if vrids != nil {
				return vrids[0], vrids[1], nil
			}
		}
	}

	// VRIDs not allocated yet, allocate new ones
	allocatedVRIDs := getAllocatedVRIDs(vridConfigMap)

	vrid1, vrid2 := findUnusedVRIDPair(allocatedVRIDs)
	if vrid1 == 0 || vrid2 == 0 {
		return 0, 0, fmt.Errorf("no available VRIDs")
	}

	// Update the ConfigMap
	if vridConfigMap.Data == nil {
		vridConfigMap.Data = make(map[string]string)
	}
	vridConfigMap.Data[clusterName] = fmt.Sprintf("%d,%d", vrid1, vrid2)

	if err != nil && client.IgnoreNotFound(err) == nil {
		// ConfigMap doesn't exist, create it
		vridConfigMap.ObjectMeta = metav1.ObjectMeta{
			Name:      "vrid-allocations",
			Namespace: "nginx-lb-operator-system",
		}
		err = c.Create(ctx, vridConfigMap)
	} else {
		// Update existing ConfigMap
		err = c.Update(ctx, vridConfigMap)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("failed to save VRID allocations: %w", err)
	}

	// Update VRID_allocations.conf on NGINX server
	if err := UpdateVRIDAllocationsFile(ctx, c, vridConfigMap.Data); err != nil {
		return 0, 0, err
	}

	return vrid1, vrid2, nil
}

// getAllocatedVRIDs returns a map of allocated VRIDs.
func getAllocatedVRIDs(vridConfigMap *corev1.ConfigMap) map[int]bool {
	allocated := make(map[int]bool)
	for _, vridStr := range vridConfigMap.Data {
		vrids := parseVRIDPair(vridStr)
		if vrids != nil {
			allocated[vrids[0]] = true
			allocated[vrids[1]] = true
		}
	}
	return allocated
}

// parseVRIDPair parses a string in the format "vrid1,vrid2".
func parseVRIDPair(vridStr string) []int {
	vridParts := strings.Split(vridStr, ",")
	if len(vridParts) != 2 {
		return nil
	}
	vrid1, err1 := strconv.Atoi(vridParts[0])
	vrid2, err2 := strconv.Atoi(vridParts[1])
	if err1 != nil || err2 != nil {
		return nil
	}
	return []int{vrid1, vrid2}
}

// findUnusedVRIDPair finds an unused pair of VRIDs.
func findUnusedVRIDPair(allocatedVRIDs map[int]bool) (int, int) {
	maxVRID := 255 // VRID range is typically 1-255
	for i := 1; i <= maxVRID-1; i += 2 {
		if !allocatedVRIDs[i] && !allocatedVRIDs[i+1] {
			return i, i + 1
		}
	}
	return 0, 0
}

// UpdateVRIDAllocationsFile updates the VRID_allocations.conf on the NGINX server.
func UpdateVRIDAllocationsFile(ctx context.Context, c client.Client, vridData map[string]string) error {
	content := ""
	for clusterName, vridStr := range vridData {
		content += fmt.Sprintf("%s: %s\n", clusterName, vridStr)
	}
	remotePath := "/etc/keepalived/VRID_allocations.conf"
	if err := CopyFileToNGINXServer(ctx, c, content, remotePath); err != nil {
		return fmt.Errorf("failed to update VRID_allocations.conf: %w", err)
	}
	return nil
}
