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

type ServiceReconciler struct {
    client.Client
    Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&corev1.Service{}).
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

    // Check if the service is being deleted
    if !svc.ObjectMeta.DeletionTimestamp.IsZero() {
        // Handle deletion
        err = r.handleServiceDeletion(ctx, req.NamespacedName)
        if err != nil {
            logger.Error(err, "Failed to handle service deletion")
            return ctrl.Result{}, err
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

    // Allocate IP and ports
    allocation, err := ipam.AllocateIPAndPorts(svc.Namespace, svc.Name, ports)
    if err != nil {
        logger.Error(err, "IP allocation failed")
        return err
    }

    // Fetch endpoints
    endpoints := &corev1.Endpoints{}
    err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, endpoints)
    if err != nil {
        logger.Error(err, "Failed to get Endpoints")
        ipam.ReleaseIPAndPorts(allocation)
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
        ipam.ReleaseIPAndPorts(allocation)
        return fmt.Errorf("no endpoints available for service %s/%s", svc.Namespace, svc.Name)
    }

    // Configure NGINX
    err = nginx.ConfigureService(allocation, ports, endpointIPs)
    if err != nil {
        logger.Error(err, "Failed to configure NGINX")
        ipam.ReleaseIPAndPorts(allocation)
        return err
    }

    // Validate configuration (e.g., health checks)

    // Update service status
    svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
        {IP: allocation.IP},
    }
    err = r.Status().Update(ctx, svc)
    if err != nil {
        logger.Error(err, "Failed to update Service status")
        return err
    }

    logger.Info("Successfully reconciled service", "service", svc.Name)
    return nil
}

func (r *ServiceReconciler) handleServiceDeletion(ctx context.Context, namespacedName types.NamespacedName) error {
    logger := log.FromContext(ctx)

    // Release IP and ports
    err := ipam.ReleaseAllocation(namespacedName.Namespace, namespacedName.Name)
    if err != nil {
        logger.Error(err, "Failed to release IP allocation")
        return err
    }

    // Remove NGINX configuration
    err = nginx.RemoveServiceConfiguration(namespacedName.Namespace, namespacedName.Name)
    if err != nil {
        logger.Error(err, "Failed to remove NGINX configuration")
        return err
    }

    logger.Info("Successfully handled service deletion", "service", namespacedName.Name)
    return nil
}