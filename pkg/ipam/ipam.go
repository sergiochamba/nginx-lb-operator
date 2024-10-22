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
    err := loadIPPool()
    if err != nil {
        log.Log.Error(err, "Failed to load IP pool from ConfigMap")
        return err
    }

    // Load allocations from ConfigMap
    err = loadAllocations()
    if err != nil {
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
    err := k8sClient.Get(ctx, types.NamespacedName{Name: ipPoolConfigMapName, Namespace: configMapNamespace}, cm)
    if err != nil {
        log.Log.Error(err, "Failed to get IP pool ConfigMap")
        return err
    }

    ipPoolData, exists := cm.Data["ip_pool"]
    if !exists {
        log.Log.Error(nil, "ip_pool not found in ConfigMap")
        return errors.New("ip_pool not found in ConfigMap")
    }

    ipPool, err = parseIPPool(ipPoolData)
    if err != nil {
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
            // It's a range
            rangeIps, err := expandIPRange(ipRange)
            if err != nil {
                log.Log.Error(err, "Failed to expand IP range", "IPRange", ipRange)
                return nil, err
            }
            ips = append(ips, rangeIps...)
        } else {
            // It's a single IP
            if net.ParseIP(ipRange) == nil {
                err := fmt.Errorf("invalid IP address: %s", ipRange)
                log.Log.Error(err, "Invalid IP address found in IP pool data")
                return nil, err
            }
            ips = append(ips, ipRange)
        }
    }
    return ips, nil
}

func expandIPRange(ipRange string) ([]string, error) {
    parts := strings.Split(ipRange, "-")
    if len(parts) != 2 {
        err := fmt.Errorf("invalid IP range format: %s", ipRange)
        log.Log.Error(err, "Failed to parse IP range")
        return nil, err
    }
    startIP := net.ParseIP(strings.TrimSpace(parts[0]))
    endIP := net.ParseIP(strings.TrimSpace(parts[1]))
    if startIP == nil || endIP == nil {
        err := fmt.Errorf("invalid IP in range: %s", ipRange)
        log.Log.Error(err, "Invalid IP in range")
        return nil, err
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
    err := k8sClient.Get(ctx, types.NamespacedName{Name: ipAllocationsConfigMapName, Namespace: configMapNamespace}, cm)
    if err != nil {
        if apierrors.IsNotFound(err) {
            log.Log.Info("IP allocations ConfigMap not found, assuming no allocations exist yet")
            return nil
        }
        log.Log.Error(err, "Failed to get IP allocations ConfigMap")
        return err
    }

    data, exists := cm.Data["allocations"]
    if !exists || data == "" {
        log.Log.Info("No allocations found in ConfigMap")
        return nil
    }

    var storedAllocations []*Allocation
    err = json.Unmarshal([]byte(data), &storedAllocations)
    if err != nil {
        log.Log.Error(err, "Failed to unmarshal IP allocations data")
        return err
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
    // allocationMutex.Lock() - Removed to avoid double locking
    // defer allocationMutex.Unlock() - Removed to avoid double unlocking

    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    cm.Name = ipAllocationsConfigMapName
    cm.Namespace = configMapNamespace

    log.Log.Info("Marshaling IP allocations to JSON")
    data, err := json.Marshal(allocationsToList())
    if err != nil {
        log.Log.Error(err, "Failed to marshal IP allocations data")
        return err
    }

    cm.Data = map[string]string{
        "allocations": string(data),
    }

    log.Log.Info("Attempting to save IP allocations to ConfigMap", "ConfigMapName", ipAllocationsConfigMapName, "Namespace", configMapNamespace)

    // First, try to get the ConfigMap to determine whether we need to create or update
    existingCM := &corev1.ConfigMap{}
    err = k8sClient.Get(ctx, types.NamespacedName{Name: ipAllocationsConfigMapName, Namespace: configMapNamespace}, existingCM)
    if err != nil {
        if apierrors.IsNotFound(err) {
            // ConfigMap does not exist, create it
            log.Log.Info("IP allocations ConfigMap not found, creating new ConfigMap")
            err = k8sClient.Create(ctx, cm)
            if err != nil {
                log.Log.Error(err, "Failed to create IP allocations ConfigMap")
                return err
            }
            log.Log.Info("IP allocations ConfigMap created successfully")
        } else {
            // Failed to get the ConfigMap for an unknown reason
            log.Log.Error(err, "Failed to get existing IP allocations ConfigMap")
            return err
        }
    } else {
        // ConfigMap exists, update it
        existingCM.Data = cm.Data
        log.Log.Info("Updating existing IP allocations ConfigMap", "ConfigMapName", ipAllocationsConfigMapName, "Namespace", configMapNamespace)
        err = k8sClient.Update(ctx, existingCM)
        if err != nil {
            log.Log.Error(err, "Failed to update IP allocations ConfigMap")
            return err
        }
        log.Log.Info("IP allocations ConfigMap updated successfully")
    }

    log.Log.Info("IP allocations saved successfully")
    return nil
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
    log.Log.Info("Allocating IP and ports", "Namespace", namespace, "Service", service, "Ports", ports)

    // Check if allocation already exists
    if alloc, exists := allocations[key]; exists {
        log.Log.Info("Allocation already exists", "Namespace", namespace, "Service", service, "IP", alloc.IP)
        return alloc, nil
    }

    // Find an IP with available ports
    for _, ip := range ipPool {
        log.Log.Info("Checking IP for availability", "IP", ip)

        portUsage, exists := ipPortUsage[ip]
        if !exists {
            portUsage = make(map[int32]string)
            ipPortUsage[ip] = portUsage
        }

        conflict := false
        for _, port := range ports {
            log.Log.Info("Checking port for conflict", "IP", ip, "Port", port)
            if _, inUse := portUsage[port]; inUse {
                log.Log.Info("Port conflict detected", "IP", ip, "Port", port)
                conflict = true
                break
            }
        }

        if conflict {
            log.Log.Info("IP is in conflict, skipping", "IP", ip)
            continue
        }

        // Allocate IP and ports
        log.Log.Info("Allocating IP and ports", "IP", ip, "Ports", ports)
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

        // Save allocations to ConfigMap
        log.Log.Info("Saving new allocation", "Namespace", namespace, "Service", service, "IP", ip, "Ports", ports)
        err := saveAllocations()
        if err != nil {
            log.Log.Error(err, "Failed to save IP allocations after allocation")
            return nil, err
        }

        log.Log.Info("IP and ports allocated successfully", "Namespace", namespace, "Service", service, "IP", ip, "Ports", ports)
        return allocation, nil
    }

    log.Log.Error(nil, "No available IPs found that can accommodate the requested ports", "Namespace", namespace, "Service", service)
    return nil, errors.New("no available IPs with required ports")
}

func ReleaseAllocation(namespace, service string) error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    key := fmt.Sprintf("%s/%s", namespace, service)
    log.Log.Info("Releasing IP allocation", "Namespace", namespace, "Service", service)

    alloc, exists := allocations[key]
    if !exists {
        log.Log.Info("No allocation found to release", "Namespace", namespace, "Service", service)
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

    // Save allocations to ConfigMap
    err := saveAllocations()
    if err != nil {
        log.Log.Error(err, "Failed to save IP allocations after release")
        return err
    }

    log.Log.Info("IP allocation released successfully", "Namespace", namespace, "Service", service)
    return nil
}

func GetAllocatedIPs() (map[string]bool, error) {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    log.Log.Info("Retrieving all allocated IPs")
    ips := make(map[string]bool)
    for _, alloc := range allocations {
        ips[alloc.IP] = true
    }
    log.Log.Info("Allocated IPs retrieved", "IPs", ips)
    return ips, nil
}
