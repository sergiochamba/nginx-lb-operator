package controllers

import (
    "context"
    "fmt"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/errors"
    "k8s.io/apimachinery/pkg/types"
    "k8s.io/apimachinery/pkg/runtime"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/log"

    "github.com/sergiochamba/nginx-lb-operator/pkg/ipam"
    "github.com/sergiochamba/nginx-lb-operator/pkg/nginx"
)

const (
    finalizerName = "nginx-lb-operator.finalizers.yourdomain.com"
)

type ServiceReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Service{}).
        Owns(&corev1.Endpoints{}).
        Complete(r)
}

// Reconcile function
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Fetch the Service instance
    svc := &corev1.Service{}
    err := r.Get(ctx, req.NamespacedName, svc)
    if err != nil {
        if errors.IsNotFound(err) {
            // Service not found, handle deletion
            err = r.handleServiceDeletion(ctx, req.NamespacedName)
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
            if err := r.handleServiceDeletion(ctx, svc); err != nil {
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

    // Extract service ports
    ports := []int32{}
    for _, port := range svc.Spec.Ports {
        ports = append(ports, port.Port)
    }

    // Begin transaction-like behavior
    allocated := false
    ipAssigned := false
    configApplied := false

    defer func() {
        if !configApplied {
            // Rollback actions
            if ipAssigned {
                // Remove IP from NGINX server
                if err := nginx.RemoveIPFromNginxServer(allocation.IP); err != nil {
                    logger.Error(err, "Failed to remove IP from NGINX server during rollback")
                }
            }
            if allocated {
                // Release IP allocation
                if err := ipam.ReleaseAllocation(allocation.Namespace, allocation.Service); err != nil {
                    logger.Error(err, "Failed to release IP allocation during rollback")
                }
            }
        }
    }()

    // Allocate IP and ports
    allocation, err := ipam.AllocateIPAndPorts(svc.Namespace, svc.Name, ports)
    if err != nil {
        logger.Error(err, "IP allocation failed")
        return err
    }
    allocated = true
    logger.Info("Allocated IP and ports", "IP", allocation.IP, "Ports", allocation.Ports)

    // Assign IP to NGINX server
    err = nginx.AssignIPToNginxServer(allocation.IP)
    if err != nil {
        logger.Error(err, "Failed to assign IP to NGINX server")
        return err
    }
    ipAssigned = true
    logger.Info("Assigned IP to NGINX server", "IP", allocation.IP)

    // Fetch endpoints
    endpoints := &corev1.Endpoints{}
    err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, endpoints)
    if err != nil {
        logger.Error(err, "Failed to get Endpoints")
        return err
    }

    // Extract endpoint IPs
    endpointIPs := []string{}
    for _, subset := range endpoints.Subsets {
        for _, addr := range subset.Addresses {
            endpointIPs = append(endpointIPs, addr.IP)
        }
    }
    if len(endpointIPs) == 0 {
        logger.Error(fmt.Errorf("no endpoints available"), "No endpoints for service")
        return fmt.Errorf("no endpoints available for service %s/%s", svc.Namespace, svc.Name)
    }

    // Configure NGINX
    err = nginx.ConfigureService(allocation, ports, endpointIPs)
    if err != nil {
        logger.Error(err, "Failed to configure NGINX")
        return err
    }
    configApplied = true
    logger.Info("Configured NGINX for service", "service", svc.Name)

    // Validate configuration (e.g., health checks)
    // TODO: Implement health checks if needed

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

func (r *ServiceReconciler) handleServiceDeletion(ctx context.Context, svc *corev1.Service) error {
    logger := log.FromContext(ctx)

    logger.Info("Handling service deletion", "service", svc.Name, "namespace", svc.Namespace)

    // Remove NGINX configuration
    err := nginx.RemoveServiceConfiguration(svc.Namespace, svc.Name)
    if err != nil {
        logger.Error(err, "Failed to remove NGINX configuration")
        return err
    }

    // Release IP and ports
    err = ipam.ReleaseAllocation(svc.Namespace, svc.Name)
    if err != nil {
        logger.Error(err, "Failed to release IP allocation")
        return err
    }

    logger.Info("Successfully handled service deletion", "service", svc.Name)
    return nil
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