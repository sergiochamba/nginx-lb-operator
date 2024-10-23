package controllers

import (
    "context"
    "fmt"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/errors"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/handler"
    "sigs.k8s.io/controller-runtime/pkg/log"
    "sigs.k8s.io/controller-runtime/pkg/predicate"
    "sigs.k8s.io/controller-runtime/pkg/builder"
    "github.com/sergiochamba/nginx-lb-operator/pkg/ipam"
    "github.com/sergiochamba/nginx-lb-operator/pkg/nginx"
)

const (
    finalizerName = "sergiochamba.com/nginx-lb-operator-finalizer"
)

type ServiceReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Service{}).
        Watches(
            &corev1.Endpoints{},
            &handler.EnqueueRequestForObject{},
            builder.WithPredicates(predicate.GenerationChangedPredicate{}),
        ).
        Complete(r)
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Fetch the Service instance
    svc := &corev1.Service{}
    if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
        if errors.IsNotFound(err) {
            // Service not found, handle deletion
            if err := r.handleServiceDeletion(ctx, req.NamespacedName.Namespace, req.NamespacedName.Name); err != nil {
                logger.Error(err, "Failed to handle service deletion")
                return ctrl.Result{}, err
            }
            return ctrl.Result{}, nil
        }
        logger.Error(err, "Failed to get Service")
        return ctrl.Result{}, err
    }

    // Process only LoadBalancer services
    if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
        return ctrl.Result{}, nil
    }

    // Add finalizer if not present and service is not being deleted
    if svc.ObjectMeta.DeletionTimestamp.IsZero() && !containsString(svc.ObjectMeta.Finalizers, finalizerName) {
        svc.ObjectMeta.Finalizers = append(svc.ObjectMeta.Finalizers, finalizerName)
        if err := r.Update(ctx, svc); err != nil {
            logger.Error(err, "Failed to add finalizer")
            return ctrl.Result{}, err
        }
    }

    // Handle deletion
    if !svc.ObjectMeta.DeletionTimestamp.IsZero() {
        if containsString(svc.ObjectMeta.Finalizers, finalizerName) {
            // Our finalizer is present, so handle cleanup
            if err := r.handleServiceDeletion(ctx, svc.Namespace, svc.Name); err != nil {
                logger.Error(err, "Failed to handle service deletion")
                return ctrl.Result{}, err
            }
            // Remove finalizer and update
            svc.ObjectMeta.Finalizers = removeString(svc.ObjectMeta.Finalizers, finalizerName)
            if err := r.Update(ctx, svc); err != nil {
                logger.Error(err, "Failed to remove finalizer")
                return ctrl.Result{}, err
            }
        }
        return ctrl.Result{}, nil
    }

    // Retry fetching endpoints to handle cases where they are not immediately available (with exponential backoff)
    var endpoints corev1.Endpoints
    maxRetries := 5
    for i := 0; i < maxRetries; i++ {
        if err := r.Get(ctx, req.NamespacedName, &endpoints); err != nil {
            logger.Error(err, "Failed to get Endpoints, retrying...")
            time.Sleep(time.Second * time.Duration(2<<i)) // Exponential backoff (2, 4, 8, 16, etc.)
            continue
        }
        if len(endpoints.Subsets) == 0 {
            logger.Info("No endpoints available, retrying...")
            time.Sleep(time.Second * time.Duration(2<<i))
            continue
        }
        break
    }

    if len(endpoints.Subsets) == 0 {
        logger.Info("No endpoints available for service, requeueing...", "Namespace", svc.Namespace, "Service", svc.Name)
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }

    // Handle the service
    if err := r.handleService(ctx, svc); err != nil {
        logger.Error(err, "Failed to handle service")
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }

    return ctrl.Result{}, nil
}

func (r *ServiceReconciler) handleService(ctx context.Context, svc *corev1.Service) error {
    logger := log.FromContext(ctx)

    // Extract service port
    if len(svc.Spec.Ports) != 1 {
        return fmt.Errorf("service %s/%s must have exactly one port", svc.Namespace, svc.Name)
    }
    servicePort := svc.Spec.Ports[0].Port

    // Begin transaction-like behavior
    allocated := false
    configApplied := false
    var allocation *ipam.Allocation

    defer func() {
        if !configApplied && allocated {
            // Rollback IP allocation if configuration was not applied
            if err := ipam.ReleaseAllocation(allocation.Namespace, allocation.Service); err != nil {
                logger.Error(err, "Failed to release IP allocation during rollback")
            }
        }
    }()

    // Allocate IP and ports
    var err error
    if allocation, err = ipam.AllocateIPAndPorts(svc.Namespace, svc.Name, []int32{servicePort}); err != nil {
        return fmt.Errorf("IP allocation failed: %w", err)
    }
    allocated = true
    logger.Info("Allocated IP and ports", "IP", allocation.IP, "Ports", allocation.Ports)

    // Allocate or retrieve the VRID
    vridConfigMapName := fmt.Sprintf("%s-%s-vrid-allocations", svc.Namespace, svc.Name)
    vrid, err := nginx.GetOrAllocateVRID(vridConfigMapName)
    if err != nil {
        return fmt.Errorf("failed to get or allocate VRID: %w", err)
    }
    logger.Info("Using VRID for service", "VRID", vrid)

    // Fetch endpoints
    endpoints := &corev1.Endpoints{}
    if err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, endpoints); err != nil {
        return fmt.Errorf("failed to get Endpoints: %w", err)
    }

    // Extract node IPs for endpoints
    nodeIPs, err := r.getNodeIPsForEndpoints(ctx, endpoints)
    if err != nil {
        return fmt.Errorf("failed to get node IPs for endpoints: %w", err)
    }

    if len(nodeIPs) == 0 {
        return fmt.Errorf("no endpoints available for service %s/%s", svc.Namespace, svc.Name)
    }

    // Update Keepalived configurations BEFORE configuring NGINX
    logger.Info("Updating Keepalived configurations")
    if err = nginx.UpdateKeepalivedConfigs(); err != nil {
        return fmt.Errorf("failed to update Keepalived configurations: %w", err)
    }
    logger.Info("Keepalived configurations updated successfully")

    // Wait for Keepalived to stabilize the new IP
    time.Sleep(5 * time.Second) // Add a fixed delay to allow the IP to be applied

    // Configure NGINX AFTER Keepalived is updated
    logger.Info("Configuring NGINX for service", "Service", svc.Name)
    if err = nginx.ConfigureService(allocation, servicePort, nodeIPs); err != nil {
        return fmt.Errorf("failed to configure NGINX: %w", err)
    }
    configApplied = true
    logger.Info("Configured NGINX for service", "Service", svc.Name)

    // Update service status with the allocated LoadBalancer IP
    svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
        {IP: allocation.IP},
    }
    if err = r.Status().Update(ctx, svc); err != nil {
        return fmt.Errorf("failed to update Service status: %w", err)
    }
    logger.Info("Updated service status with LoadBalancer IP", "IP", allocation.IP)

    return nil
}

func (r *ServiceReconciler) handleServiceDeletion(ctx context.Context, namespace, name string) error {
    logger := log.FromContext(ctx)

    logger.Info("Handling service deletion", "service", name, "namespace", namespace)

    // Release IP and ports
    if err := ipam.ReleaseAllocation(namespace, name); err != nil {
        return fmt.Errorf("failed to release IP allocation: %w", err)
    }

    // Remove NGINX configuration
    if err := nginx.RemoveServiceConfiguration(namespace, name); err != nil {
        return fmt.Errorf("failed to remove NGINX configuration: %w", err)
    }
    logger.Info("Removed NGINX configuration for service", "service", name)

    // Update Keepalived configurations AFTER NGINX removal
    if err := nginx.UpdateKeepalivedConfigs(); err != nil {
        return fmt.Errorf("failed to update Keepalived configurations: %w", err)
    }
    logger.Info("Updated Keepalived configurations")

    logger.Info("Successfully handled service deletion", "service", name)
    return nil
}

func (r *ServiceReconciler) getNodeIPsForEndpoints(ctx context.Context, endpoints *corev1.Endpoints) ([]string, error) {
    nodeIPs := []string{}
    for _, subset := range endpoints.Subsets {
        for _, addr := range subset.Addresses {
            if addr.NodeName == nil {
                continue
            }
            node := &corev1.Node{}
            if err := r.Get(ctx, types.NamespacedName{Name: *addr.NodeName}, node); err != nil {
                return nil, fmt.Errorf("failed to get node %s: %w", *addr.NodeName, err)
            }
            for _, address := range node.Status.Addresses {
                if address.Type == corev1.NodeInternalIP {
                    nodeIPs = append(nodeIPs, address.Address)
                    break
                }
            }
        }
    }
    return nodeIPs, nil
}

// Helper functions
func containsString(slice []string, s string) bool {
    for _, item := range slice {
        if item == s {
            return true
        }
    }
    return false
}

func removeString(slice []string, s string) []string {
    result := []string{}
    for _, item := range slice {
        if item != s {
            result = append(result, item)
        }
    }
    return result
}