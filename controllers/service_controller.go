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
    finalizerName = "nginx-lb-operator.finalizers.sergiochamba.com"
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
    err := r.Get(ctx, req.NamespacedName, svc)
    if err != nil {
        if errors.IsNotFound(err) {
            // Service not found, handle deletion
            err = r.handleServiceDeletion(ctx, req.NamespacedName.Namespace, req.NamespacedName.Name)
            if err != nil {
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

    // Handle the service
    err = r.handleService(ctx, svc)
    if err != nil {
        logger.Error(err, "Failed to handle service")
        // Requeue after a delay
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }

    return ctrl.Result{}, nil
}

func (r *ServiceReconciler) handleService(ctx context.Context, svc *corev1.Service) error {
    logger := log.FromContext(ctx)

    // Extract service port
    if len(svc.Spec.Ports) != 1 {
        err := fmt.Errorf("service %s/%s must have exactly one port", svc.Namespace, svc.Name)
        logger.Error(err, "Invalid service ports")
        return err
    }
    servicePort := svc.Spec.Ports[0].Port

    // Begin transaction-like behavior
    allocated := false
    configApplied := false

    var allocation *ipam.Allocation

    defer func() {
        if !configApplied {
            if allocated {
                // Release IP allocation
                if err := ipam.ReleaseAllocation(allocation.Namespace, allocation.Service); err != nil {
                    logger.Error(err, "Failed to release IP allocation during rollback")
                }
            }
        }
    }()

    // Allocate IP and ports
    var err error
    allocation, err = ipam.AllocateIPAndPorts(svc.Namespace, svc.Name, []int32{servicePort})
    if err != nil {
        logger.Error(err, "IP allocation failed")
        return err
    }
    allocated = true
    logger.Info("Allocated IP and ports", "IP", allocation.IP, "Ports", allocation.Ports)

    // Fetch endpoints
    endpoints := &corev1.Endpoints{}
    err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, endpoints)
    if err != nil {
        logger.Error(err, "Failed to get Endpoints")
        return err
    }

    // Extract node IPs for endpoints
    nodeIPs, err := r.getNodeIPsForEndpoints(ctx, endpoints)
    if err != nil {
        logger.Error(err, "Failed to get node IPs for endpoints")
        return err
    }

    if len(nodeIPs) == 0 {
        logger.Error(fmt.Errorf("no endpoints available"), "No endpoints for service")
        return fmt.Errorf("no endpoints available for service %s/%s", svc.Namespace, svc.Name)
    }

    // Configure NGINX
    err = nginx.ConfigureService(allocation, servicePort, nodeIPs)
    if err != nil {
        logger.Error(err, "Failed to configure NGINX")
        return err
    }
    configApplied = true
    logger.Info("Configured NGINX for service", "service", svc.Name)

    // Update Keepalived configurations
    err = nginx.UpdateKeepalivedConfigs()
    if err != nil {
        logger.Error(err, "Failed to update Keepalived configurations")
        return err
    }
    logger.Info("Updated Keepalived configurations")

    // Update service status
    svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
        {IP: allocation.IP},
    }
    err = r.Status().Update(ctx, svc)
    if err != nil {
        logger.Error(err, "Failed to update Service status")
        return err
    }
    logger.Info("Updated service status with LoadBalancer IP", "IP", allocation.IP)

    return nil
}

func (r *ServiceReconciler) handleServiceDeletion(ctx context.Context, namespace, name string) error {
    logger := log.FromContext(ctx)

    logger.Info("Handling service deletion", "service", name, "namespace", namespace)

    // Release IP and ports
    err := ipam.ReleaseAllocation(namespace, name)
    if err != nil {
        logger.Error(err, "Failed to release IP allocation")
        return err
    }

    // Remove NGINX configuration
    err = nginx.RemoveServiceConfiguration(namespace, name)
    if err != nil {
        logger.Error(err, "Failed to remove NGINX configuration")
        return err
    }
    logger.Info("Removed NGINX configuration for service", "service", name)

    // Update Keepalived configurations
    err = nginx.UpdateKeepalivedConfigs()
    if err != nil {
        logger.Error(err, "Failed to update Keepalived configurations")
        return err
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
            err := r.Get(ctx, types.NamespacedName{Name: *addr.NodeName}, node)
            if err != nil {
                return nil, err
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
