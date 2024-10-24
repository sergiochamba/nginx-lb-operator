package controllers

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors" // Corrected import
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/sergiochamba/nginx-lb-operator/utils"
)

// ServiceReconciler reconciles Service objects
type ServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Setting up the controller
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(
			&corev1.Endpoints{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicate.Funcs{
				// Log and filter create events for Endpoints
				CreateFunc: func(e event.CreateEvent) bool {
					return r.isLoadBalancerService(e.Object)
				},
				// Log and filter delete events for Endpoints
				DeleteFunc: func(e event.DeleteEvent) bool {
					return r.isLoadBalancerService(e.Object)
				},
				// Log and filter update events for Endpoints
				UpdateFunc: func(e event.UpdateEvent) bool {
					return r.isLoadBalancerService(e.ObjectNew)
				},
			}),
		).
		Complete(r)
}

// Define the helper method for the ServiceReconciler struct
func (r *ServiceReconciler) isLoadBalancerService(endpoints client.Object) bool {
	ctx := context.Background()

	// Extract the namespace and name of the associated Service from the Endpoints
	ep := endpoints.(*corev1.Endpoints)
	serviceName := ep.Name
	namespace := ep.Namespace

	// Fetch the associated Service object
	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, svc); err != nil {
		ctrl.Log.Info("unable to fetch associated Service for Endpoints - Service could have been deleted", "endpoints", serviceName)
		return false
	}

	// Check if the Service is of type LoadBalancer
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		ctrl.Log.Info("LoadBalancer Service Update detected", "service", serviceName, "namespace", namespace)
		return true
	}

	return false
}

// Reconcile handles the reconciliation of the Service resource.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Service instance
	service := &corev1.Service{}
	err := r.Get(ctx, req.NamespacedName, service)
	if err != nil {
		if errors.IsNotFound(err) {
			// Service not found; it might have been deleted after reconcile request
			return r.handleDeletedService(ctx, req.NamespacedName)
		}
		// Error reading the object - requeue the request
		return ctrl.Result{}, err
	}

	// Only proceed if the service is of type LoadBalancer
	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ctrl.Result{}, nil
	}

	// Handle finalizer for cleanup
	finalizerName := "sergiochamba.com/nginx-lb-operator-finalizer"

	if service.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted
		if !utils.ContainsString(service.ObjectMeta.Finalizers, finalizerName) {
			service.ObjectMeta.Finalizers = append(service.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(ctx, service); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if utils.ContainsString(service.ObjectMeta.Finalizers, finalizerName) {
			// Our finalizer is present, so let's handle any external dependency
			if err := r.finalizeService(ctx, service); err != nil {
				return ctrl.Result{}, err
			}
			// Remove finalizer and update
			service.ObjectMeta.Finalizers = utils.RemoveString(service.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(ctx, service); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Main reconciliation logic
	if err := r.reconcileService(ctx, service); err != nil {
		log.Error(err, "Failed to reconcile service")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileService handles the main reconciliation logic for the service
func (r *ServiceReconciler) reconcileService(ctx context.Context, service *corev1.Service) error {
	log := log.FromContext(ctx)
	svcKey := client.ObjectKeyFromObject(service)

	// Check if IP is already allocated
	ipAllocated, err := utils.IsIPAllocatedToService(ctx, r.Client, service)
	if err != nil {
		log.Error(err, "Failed to check if IP is allocated to service", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "IPAllocationError", "Failed to check IP allocation")
		return err
	}

	var ip string
	if !ipAllocated {
		// Allocate IP
		ip, err = utils.AllocateIP(ctx, r.Client, service)
		if err != nil {
			log.Error(err, "Failed to allocate IP to service", "service", svcKey)
			r.Recorder.Event(service, corev1.EventTypeWarning, "IPAllocationFailed", "Failed to allocate IP")
			return err
		}
		log.Info("Allocated IP to service", "service", svcKey, "ip", ip)
		r.Recorder.Event(service, corev1.EventTypeNormal, "IPAllocated", "IP allocated successfully")
	} else {
		// Retrieve allocated IP
		ip, err = utils.GetAllocatedIPForService(ctx, r.Client, service)
		if err != nil {
			log.Error(err, "Failed to get allocated IP for service", "service", svcKey)
			r.Recorder.Event(service, corev1.EventTypeWarning, "GetIPError", "Failed to retrieve allocated IP")
			return err
		}
	}

	// Fetch the already allocated VRIDs (done at startup)
	vrid1, vrid2, err := utils.GetOrAllocateVRIDs(ctx, r.Client)
	if err != nil {
		log.Error(err, "Failed to retrieve VRIDs")
		r.Recorder.Event(service, corev1.EventTypeWarning, "VRIDError", "Failed to retrieve VRIDs")
		return err
	}

	// Configure Keepalived
	if err := utils.ConfigureKeepalived(ctx, r.Client, vrid1, vrid2); err != nil {
		log.Error(err, "Failed to configure Keepalived", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "KeepalivedError", "Failed to configure Keepalived")
		return err
	}
	log.Info("Updated Keepalived configuration")

	// Wait for 3 seconds for Keepalived to apply changes
	log.Info("Waiting for Keepalived to apply VIPs", "duration", "3s")
	r.Recorder.Event(service, corev1.EventTypeNormal, "Waiting", "Waiting for Keepalived to apply VIPs")
	time.Sleep(3 * time.Second)

	// Configure NGINX
	if err := utils.ConfigureNGINX(ctx, r.Client, service, ip); err != nil {
		log.Error(err, "Failed to configure NGINX for service", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "NGINXConfigError", "Failed to configure NGINX")
		return err
	}
	log.Info("Configured NGINX for service", "service", svcKey)
	r.Recorder.Event(service, corev1.EventTypeNormal, "NGINXConfigured", "NGINX configured successfully")

	// Refetch the latest version of the service before updating the status
	if err := r.Get(ctx, svcKey, service); err != nil {
		log.Error(err, "Failed to refetch service before status update", "service", svcKey)
		return err
	}

	// Update the Service status with the allocated LoadBalancer IP
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
		{
			IP: ip, // The IP you've allocated for the service
		},
	}

	// Update the service status in the cluster
	if err := r.Status().Update(ctx, service); err != nil {
		log.Error(err, "Failed to update service status with LoadBalancer IP", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "StatusUpdateFailed", "Failed to update service status")
		return err
	}
	log.Info("Updated service status with LoadBalancer IP", "service", svcKey, "ip", ip)
	r.Recorder.Event(service, corev1.EventTypeNormal, "StatusUpdated", "Service status updated with LoadBalancer IP")

	return nil
}

// finalizeService handles cleanup when a service is deleted
func (r *ServiceReconciler) finalizeService(ctx context.Context, service *corev1.Service) error {
	log := log.FromContext(ctx)
	svcKey := client.ObjectKeyFromObject(service)

	// Remove NGINX configuration
	if err := utils.RemoveNGINXConfig(ctx, r.Client, service); err != nil {
		log.Error(err, "Failed to remove NGINX configuration for service", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "NGINXRemovalFailed", "Failed to remove NGINX configuration")
		return err
	}
	log.Info("Removed NGINX configuration for service", "service", svcKey)
	r.Recorder.Event(service, corev1.EventTypeNormal, "NGINXRemoved", "NGINX configuration removed successfully")

	// Release IP
	if err := utils.ReleaseIP(ctx, r.Client, service); err != nil {
		log.Error(err, "Failed to release IP for service", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "ReleaseIPFailed", "Failed to release IP")
		return err
	}
	log.Info("Released IP for service", "service", svcKey)
	r.Recorder.Event(service, corev1.EventTypeNormal, "IPReleased", "IP released successfully")

	// Update Keepalived configuration
	vrid1, vrid2, err := utils.GetOrAllocateVRIDs(ctx, r.Client) // Updated function call
	if err != nil {
		log.Error(err, "Failed to get VRIDs during finalization")
		r.Recorder.Event(service, corev1.EventTypeWarning, "VRIDError", "Failed to get VRIDs during finalization")
		return err
	}
	if err := utils.ConfigureKeepalived(ctx, r.Client, vrid1, vrid2); err != nil {
		log.Error(err, "Failed to update Keepalived during finalization", "service", svcKey)
		r.Recorder.Event(service, corev1.EventTypeWarning, "KeepalivedUpdateError", "Failed to update Keepalived")
		return err
	}
	log.Info("Updated Keepalived configuration during finalization", "service", svcKey)
	r.Recorder.Event(service, corev1.EventTypeNormal, "KeepalivedUpdated", "Keepalived updated successfully")

	return nil
}

// handleDeletedService handles the scenario where the service was deleted before reconciliation
func (r *ServiceReconciler) handleDeletedService(ctx context.Context, namespacedName client.ObjectKey) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Service not found; it might have been deleted", "service", namespacedName)

	// Create a dummy service object to pass to the cleanup functions
	//service := &corev1.Service{
	//	ObjectMeta: metav1.ObjectMeta{
	//		Name:      namespacedName.Name,
	//		Namespace: namespacedName.Namespace,
	//	},
	//}

	// Perform finalization
	//if err := r.finalizeService(ctx, service); err != nil {
	//	log.Error(err, "Failed to finalize deleted service", "service", namespacedName)
	//	return ctrl.Result{}, err
	//}

	return ctrl.Result{}, nil
}
