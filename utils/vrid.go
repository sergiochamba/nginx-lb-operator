package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	vridAllocationMutex sync.Mutex
)

// GetOrAllocateVRIDs retrieves VRIDs for the cluster from the ConfigMap.
// This function no longer handles VRID allocation.
func GetOrAllocateVRIDs(ctx context.Context, c client.Client) (int, int, error) {
	vridAllocationMutex.Lock()
	defer vridAllocationMutex.Unlock()

	clusterName := GetClusterName()

	// Retrieve existing VRIDs from the ConfigMap
	vridConfigMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      "vrid-allocations",
		Namespace: "nginx-lb-operator-system",
	}, vridConfigMap)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get VRID allocations: %w", err)
	}

	// Check if VRIDs are allocated for this cluster
	if vridStr, exists := vridConfigMap.Data[clusterName]; exists {
		// VRIDs are already allocated
		vrids := parseVRIDPair(vridStr)
		if vrids != nil {
			return vrids[0], vrids[1], nil
		}
	}

	return 0, 0, fmt.Errorf("no VRIDs allocated for cluster %s", clusterName)
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

// GetOrAllocateVRIDsOnStartup handles VRID allocation at operator startup.
func GetOrAllocateVRIDsOnStartup(ctx context.Context, c client.Client) error {
	vridAllocationMutex.Lock()
	defer vridAllocationMutex.Unlock()

	clusterName := GetClusterName()

	// Step 1: Fetch VRID_allocations.conf from the NGINX server
	vridAllocationsData, err := FetchVRIDAllocationsFromNGINX(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to fetch VRID_allocations.conf from NGINX: %w", err)
	}

	// If VRID_allocations.conf does not exist (empty data), treat it as a new allocation
	if len(vridAllocationsData) == 0 {
		// Allocate new VRIDs for the cluster
		vrid1, vrid2 := 1, 2
		vridAllocationsData = map[string]string{
			clusterName: fmt.Sprintf("%d,%d", vrid1, vrid2),
		}

		// Create the VRID_allocations.conf file on NGINX
		fileContent := createVRIDFileContent(vridAllocationsData)
		if err := CopyFileToNGINXServer(ctx, c, fileContent, "/etc/keepalived/VRID_allocations.conf"); err != nil {
			return fmt.Errorf("failed to create VRID_allocations.conf on NGINX: %w", err)
		}

		// Only update the ConfigMap with the operator's VRIDs
		if err := updateConfigMapWithClusterVRID(ctx, c, clusterName, fmt.Sprintf("%d,%d", vrid1, vrid2)); err != nil {
			return fmt.Errorf("failed to update VRID allocations ConfigMap: %w", err)
		}

		return nil
	}

	// Step 2: Check if the current cluster has VRIDs in the file
	if _, exists := vridAllocationsData[clusterName]; !exists {
		// Allocate new VRIDs for the current cluster
		allocatedVRIDs := parseAllocatedVRIDs(vridAllocationsData)
		vrid1, vrid2 := findUnusedVRIDPair(allocatedVRIDs)
		if vrid1 == 0 || vrid2 == 0 {
			return fmt.Errorf("no available VRIDs")
		}

		// Add the new VRIDs to the VRID_allocations.conf
		vridAllocationsData[clusterName] = fmt.Sprintf("%d,%d", vrid1, vrid2)

		// Update the VRID_allocations.conf file on the NGINX server
		if err := UpdateVRIDAllocationsFile(ctx, c, vridAllocationsData); err != nil {
			return fmt.Errorf("failed to update VRID_allocations.conf: %w", err)
		}

		// Only update the ConfigMap with the operator's VRIDs
		if err := updateConfigMapWithClusterVRID(ctx, c, clusterName, fmt.Sprintf("%d,%d", vrid1, vrid2)); err != nil {
			return fmt.Errorf("failed to update VRID allocations ConfigMap: %w", err)
		}
	} else {
		// If the current cluster already has VRIDs, ensure the ConfigMap reflects that
		if err := updateConfigMapWithClusterVRID(ctx, c, clusterName, vridAllocationsData[clusterName]); err != nil {
			return fmt.Errorf("failed to update VRID allocations ConfigMap: %w", err)
		}
	}

	return nil
}

// updateConfigMapWithClusterVRID updates the ConfigMap with only the operator's cluster VRID allocation.
func updateConfigMapWithClusterVRID(ctx context.Context, c client.Client, clusterName, vrid string) error {
	vridConfigMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      "vrid-allocations",
		Namespace: "nginx-lb-operator-system",
	}, vridConfigMap)

	if errors.IsNotFound(err) {
		// Create the ConfigMap if it doesn't exist
		vridConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vrid-allocations",
				Namespace: "nginx-lb-operator-system",
			},
			Data: map[string]string{
				clusterName: vrid,
			},
		}
		return c.Create(ctx, vridConfigMap)
	}

	// Update only the VRID for the operator's cluster
	vridConfigMap.Data[clusterName] = vrid
	return c.Update(ctx, vridConfigMap)
}

// createOrUpdateVRIDConfigMap creates or updates the ConfigMap with the VRID allocations data.
func createOrUpdateVRIDConfigMap(ctx context.Context, c client.Client, vridAllocationsData map[string]string) error {
	vridConfigMap := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{
		Name:      "vrid-allocations",
		Namespace: "nginx-lb-operator-system",
	}, vridConfigMap)

	if errors.IsNotFound(err) {
		// Create the ConfigMap if it doesn't exist
		vridConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vrid-allocations",
				Namespace: "nginx-lb-operator-system",
			},
			Data: vridAllocationsData,
		}
		return c.Create(ctx, vridConfigMap)
	}

	// Update the existing ConfigMap
	vridConfigMap.Data = vridAllocationsData
	return c.Update(ctx, vridConfigMap)
}

// createVRIDFileContent generates the content for the VRID_allocations.conf file based on the provided data.
func createVRIDFileContent(vridData map[string]string) string {
	var content strings.Builder
	for clusterName, vridStr := range vridData {
		content.WriteString(fmt.Sprintf("%s: %s\n", clusterName, vridStr))
	}
	return content.String()
}

// FetchVRIDAllocationsFromNGINX fetches the VRID_allocations.conf from the NGINX server.
func FetchVRIDAllocationsFromNGINX(ctx context.Context, c client.Client) (map[string]string, error) {
	content, err := FetchFileFromNGINXServer(ctx, c, "/etc/keepalived/VRID_allocations.conf")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch VRID_allocations.conf: %w", err)
	}

	vridAllocations := make(map[string]string)
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) == 2 {
			clusterName := strings.TrimSpace(parts[0])
			vrids := strings.TrimSpace(parts[1])
			vridAllocations[clusterName] = vrids
		}
	}
	return vridAllocations, nil
}

// parseAllocatedVRIDs parses the VRID allocations from the VRID_allocations.conf data.
func parseAllocatedVRIDs(vridData map[string]string) map[int]bool {
	allocated := make(map[int]bool)
	for _, vridStr := range vridData {
		vrids := parseVRIDPair(vridStr)
		if vrids != nil {
			allocated[vrids[0]] = true
			allocated[vrids[1]] = true
		}
	}
	return allocated
}
