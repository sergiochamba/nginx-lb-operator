package ipam

import (
    "errors"
    "fmt"
    "sync"
)

type Allocation struct {
    Namespace string
    Service   string
    IP        string
    Ports     []int32
}

var (
    ipPool = []string{
        "10.1.1.55",
        "10.1.1.56",
        "10.1.1.57",
    }

    allocations      = make(map[string]*Allocation) // Key: namespace/service
    ipPortUsage      = make(map[string]map[int32]string)
    allocationMutex  sync.Mutex
)

// AllocateIPAndPorts assigns an IP and ports to a service
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
            return allocation, nil
        }
    }

    return nil, errors.New("no available IPs with required ports")
}

// ReleaseAllocation frees the IP and ports for a service
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
    return nil
}

// ReleaseIPAndPorts is deprecated, use ReleaseAllocation instead
func ReleaseIPAndPorts(allocation *Allocation) {
    ReleaseAllocation(allocation.Namespace, allocation.Service)
}