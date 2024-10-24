package main

import (
	"context"
	"flag"
	"os"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/sergiochamba/nginx-lb-operator/controllers"
	"github.com/sergiochamba/nginx-lb-operator/utils"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "nginx-lb-operator",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.ServiceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("nginx-lb-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Service")
		os.Exit(1)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Wait group to prevent exit until everything finishes
	var wg sync.WaitGroup
	wg.Add(1) // Add 1 for the cache sync and VRID allocation

	// Start the manager in a goroutine
	go func() {
		setupLog.Info("Starting manager...")
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			setupLog.Error(err, "problem running manager")
			os.Exit(1)
		}
		wg.Done() // Mark as done when manager starts successfully
	}()

	// Wait for the cache to synchronize before starting other operations
	setupLog.Info("Waiting for cache sync...")
	if !mgr.GetCache().WaitForCacheSync(context.Background()) {
		setupLog.Error(err, "Cache sync failed")
		os.Exit(1)
	}
	setupLog.Info("Cache sync successful")

	// Allocate VRIDs after cache sync is complete
	if err := utils.GetOrAllocateVRIDsOnStartup(context.Background(), mgr.GetClient()); err != nil {
		setupLog.Error(err, "Failed to allocate VRIDs at operator startup")
		os.Exit(1)
	}

	setupLog.Info("Manager cache synced and VRID allocation complete. Starting manager...")

	// Wait for goroutine to finish
	wg.Wait()
}
