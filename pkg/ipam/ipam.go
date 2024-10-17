package ipam

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "sync"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"
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
    ipPoolConfigMapName      = "ip-pool-config"
    ipAllocationsConfigMapName = "ip-allocations"
    configMapNamespace       = "nginx-lb-operator-system"
)

func Init(client client.Client) error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    k8sClient = client
    allocations = make(map[string]*Allocation)
    ipPortUsage = make(map[string]map[int32]string)

    // Load IP pool from ConfigMap
    err := loadIPPool()
    if err != nil {
        return err
    }

    // Load allocations from ConfigMap
    err = loadAllocations()
    if err != nil {
        return err
    }

    return nil
}

func loadIPPool() error {
    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    err := k8sClient.Get(ctx, types.NamespacedName{Name: ipPoolConfigMapName, Namespace: configMapNamespace}, cm)
    if err != nil {
        return err
    }

    ipPoolData, exists := cm.Data["ip_pool"]
    if !exists {
        return errors.New("ip_pool not found in ConfigMap")
    }

    ipPool = []string{}
    for _, ip := range parseIPPool(ipPoolData) {
        ipPool = append(ipPool, ip)
    }

    return nil
}

func loadAllocations() error {
    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    err := k8sClient.Get(ctx, types.NamespacedName{Name: ipAllocationsConfigMapName, Namespace: configMapNamespace}, cm)
    if err != nil {
        if errors.IsNotFound(err) {
            // ConfigMap doesn't exist yet
            return nil
        }
        return err
    }

    data, exists := cm.Data["allocations"]
    if !exists {
        return nil
    }

    var storedAllocations []*Allocation
    err = json.Unmarshal([]byte(data), &storedAllocations)
    if err != nil {
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

    return nil
}

func saveAllocations() error {
    allocationMutex.Lock()
    defer allocationMutex.Unlock()

    ctx := context.Background()
    cm := &corev1.ConfigMap{}
    cm.Name = ipAllocationsConfigMapName
    cm.Namespace = configMapNamespace

    data, err := json.Marshal(allocationsToList())
    if err != nil {
        return err
    }

    cm.Data = map[string]string{
        "allocations": string(data),
    }

    err = k8sClient.Update(ctx, cm)
    if err != nil {
        if errors.IsNotFound(err) {
            // Create the ConfigMap
            err = k8sClient.Create(ctx, cm)
            if err != nil {
                return err
            }
        } else {
            return err
        }
    }

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

    // Check if allocation already exists
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
            if svc, inUse := portUsage[port]; inUse {
                conflict = true
                break
            }
        }

        if !conflict {
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

            // Save allocations to ConfigMap
            err := saveAllocations()
            if err != nil {
                return nil, err
            }

            return allocation, nil
        }
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

    // Save allocations to ConfigMap
    err := saveAllocations()
    if err != nil {
        return err
    }

    return nil
}

func parseIPPool(data string) []string {
    ips := []string{}
    for _, line := range strings.Split(data, "\n") {
        ip := strings.TrimSpace(line)
        if ip != "" {
            ips = append(ips, ip)
        }
    }
    return ips
}