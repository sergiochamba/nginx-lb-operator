package ipam

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net"
    "strings"
    "sync"

    corev1 "k8s.io/api/core/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/log"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Allocation struct {
    Namespace string
    Service   string
    IP        string
    Ports     []int32
}

var (
    allocationMutex sync.Mutex
    ipPool          []string
    allocations     map[string]*Allocation // Key: namespace/service
    ipPortUsage     map[string]map[int32]string
    k8sClient       client.Client
)

const (
    ipPoolConfigMapName        = "ip-pool-config"
    ipAllocationsConfigMapName = "ip-allocations"
    configMapNamespace         = "nginx-lb-operator-system"
)

func Init(client client.Client) error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    k8sClient = client
    allocations = make(map[string]*Allocation)
    ipPortUsage = make(map[string]map[int32]string)

    log.Log.Info("Initializing IPAM module")

    // Load IP pool from ConfigMap
    if err := loadIPPool(); err != nil {
        log.Log.Error(err, "Failed to load IP pool from ConfigMap")
        return err
    }

    // Load allocations from ConfigMap
    if err := loadAllocations(); err != nil {
        log.Log.Error(err, "Failed to load allocations from ConfigMap")
        return err
    }

    log.Log.Info("IPAM module initialized successfully")
    return nil
}

func loadIPPool() error {
    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    log.Log.Info("Loading IP pool from ConfigMap", "ConfigMapName", ipPoolConfigMapName, "Namespace", configMapNamespace)
    
    if err := k8sClient.Get(ctx, types.NamespacedName{Name: ipPoolConfigMapName, Namespace: configMapNamespace}, cm); err != nil {
        log.Log.Error(err, "Failed to get IP pool ConfigMap")
        return err
    }

    ipPoolData, exists := cm.Data["ip_pool"]
    if !exists {
        return errors.New("ip_pool not found in ConfigMap")
    }

    var err error
    if ipPool, err = parseIPPool(ipPoolData); err != nil {
        log.Log.Error(err, "Failed to parse IP pool data")
        return err
    }

    log.Log.Info("IP pool loaded successfully", "IPPool", ipPool)
    return nil
}

func parseIPPool(data string) ([]string, error) {
    ips := []string{}
    lines := strings.Split(data, "\n")
    for _, line := range lines {
        ipRange := strings.TrimSpace(line)
        if ipRange == "" || strings.HasPrefix(ipRange, "#") {
            continue
        }
        if strings.Contains(ipRange, "-") {
            rangeIps, err := expandIPRange(ipRange)
            if err != nil {
                return nil, fmt.Errorf("failed to expand IP range %s: %w", ipRange, err)
            }
            ips = append(ips, rangeIps...)
        } else {
            if net.ParseIP(ipRange) == nil {
                return nil, fmt.Errorf("invalid IP address: %s", ipRange)
            }
            ips = append(ips, ipRange)
        }
    }
    return ips, nil
}

func expandIPRange(ipRange string) ([]string, error) {
    parts := strings.Split(ipRange, "-")
    if len(parts) != 2 {
        return nil, fmt.Errorf("invalid IP range format: %s", ipRange)
    }
    startIP := net.ParseIP(strings.TrimSpace(parts[0]))
    endIP := net.ParseIP(strings.TrimSpace(parts[1]))
    if startIP == nil || endIP == nil {
        return nil, fmt.Errorf("invalid IP in range: %s", ipRange)
    }
    return generateIPRange(startIP, endIP)
}

func generateIPRange(startIP, endIP net.IP) ([]string, error) {
    ips := []string{}
    for ip := startIP; !ip.Equal(endIP); ip = nextIP(ip) {
        ips = append(ips, ip.String())
    }
    ips = append(ips, endIP.String())
    return ips, nil
}

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

func loadAllocations() error {
    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    log.Log.Info("Loading IP allocations from ConfigMap", "ConfigMapName", ipAllocationsConfigMapName, "Namespace", configMapNamespace)
    
    if err := k8sClient.Get(ctx, types.NamespacedName{Name: ipAllocationsConfigMapName, Namespace: configMapNamespace}, cm); err != nil {
        if apierrors.IsNotFound(err) {
            log.Log.Info("IP allocations ConfigMap not found, assuming no allocations exist yet")
            return nil
        }
        return fmt.Errorf("failed to get IP allocations ConfigMap: %w", err)
    }

    data, exists := cm.Data["allocations"]
    if !exists || data == "" {
        log.Log.Info("No allocations found in ConfigMap")
        return nil
    }

    var storedAllocations []*Allocation
    if err := json.Unmarshal([]byte(data), &storedAllocations); err != nil {
        return fmt.Errorf("failed to unmarshal IP allocations data: %w", err)
    }

    for _, alloc := range storedAllocations {
        key := fmt.Sprintf("%s/%s", alloc.Namespace, alloc.Service)
        allocations[key] = alloc
        if _, exists := ipPortUsage[alloc.IP]; !exists {
            ipPortUsage[alloc.IP] = make(map[int32]string)
        }
        for _, port := range alloc.Ports {
            ipPortUsage[alloc.IP][port] = key
        }
    }

    log.Log.Info("IP allocations loaded successfully", "Allocations", allocations)
    return nil
}

func saveAllocations() error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    ctx := context.Background()
    cm := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      ipAllocationsConfigMapName,
            Namespace: configMapNamespace,
        },
    }

    // Marshal allocations to JSON
    data, err := json.Marshal(allocationsToList())
    if err != nil {
        return fmt.Errorf("failed to marshal IP allocations data: %w", err)
    }

    cm.Data = map[string]string{
        "allocations": string(data),
    }

    // Try to get existing ConfigMap
    existingCM := &corev1.ConfigMap{}
    err = k8sClient.Get(ctx, types.NamespacedName{Name: ipAllocationsConfigMapName, Namespace: configMapNamespace}, existingCM)
    if err != nil && !apierrors.IsNotFound(err) {
        return fmt.Errorf("failed to get existing IP allocations ConfigMap: %w", err)
    }

    if apierrors.IsNotFound(err) {
        // ConfigMap doesn't exist, create it
        return k8sClient.Create(ctx, cm)
    }

    // Update existing ConfigMap
    existingCM.Data = cm.Data
    return k8sClient.Update(ctx, existingCM)
}

func allocationsToList() []*Allocation {
    allocs := []*Allocation{}
    for _, alloc := range allocations {
        allocs = append(allocs, alloc)
    }
    return allocs
}

func AllocateIPAndPorts(namespace, service string, ports []int32) (*Allocation, error) {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    key := fmt.Sprintf("%s/%s", namespace, service)
    if alloc, exists := allocations[key]; exists {
        return alloc, nil
    }

    // Find an IP with available ports
    for _, ip := range ipPool {
        portUsage, exists := ipPortUsage[ip]
        if !exists {
            portUsage = make(map[int32]string)
            ipPortUsage[ip] = portUsage
        }

        conflict := false
        for _, port := range ports {
            if _, inUse := portUsage[port]; inUse {
                conflict = true
                break
            }
        }

        if conflict {
            continue
        }

        // Allocate IP and ports
        for _, port := range ports {
            portUsage[port] = key
        }
        allocation := &Allocation{
            Namespace: namespace,
            Service:   service,
            IP:        ip,
            Ports:     ports,
        }
        allocations[key] = allocation

        if err := saveAllocations(); err != nil {
            return nil, err
        }

        return allocation, nil
    }

    return nil, errors.New("no available IPs with required ports")
}

func ReleaseAllocation(namespace, service string) error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    key := fmt.Sprintf("%s/%s", namespace, service)
    alloc, exists := allocations[key]
    if !exists {
        return nil
    }

    // Release ports
    portUsage := ipPortUsage[alloc.IP]
    for _, port := range alloc.Ports {
        delete(portUsage, port)
    }
    if len(portUsage) == 0 {
        delete(ipPortUsage, alloc.IP)
    }

    delete(allocations, key)

    return saveAllocations()
}

func GetAllocatedIPs() (map[string]bool, error) {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    ips := make(map[string]bool)
    for _, alloc := range allocations {
        ips[alloc.IP] = true
    }
    return ips, nil
}