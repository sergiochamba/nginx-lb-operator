package main

import (
    "flag"
    "os"
    "time"

    "k8s.io/apimachinery/pkg/runtime"
    utilruntime "k8s.io/apimachinery/pkg/util/runtime"
    clientgoscheme "k8s.io/client-go/kubernetes/scheme"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/log/zap"
    metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

    "github.com/sergiochamba/nginx-lb-operator/controllers"
    "github.com/sergiochamba/nginx-lb-operator/pkg/ipam"
    "github.com/sergiochamba/nginx-lb-operator/pkg/nginx"
    // +kubebuilder:scaffold:imports
)

var (
    scheme   = runtime.NewScheme()
    setupLog = ctrl.Log.WithName("setup")
)

func init() {
    utilruntime.Must(clientgoscheme.AddToScheme(scheme))

    // +kubebuilder:scaffold:scheme
}

func main() {
    var metricsAddr string
    var enableLeaderElection bool
    flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
    flag.BoolVar(&enableLeaderElection, "leader-elect", false,
        "Enable leader election for controller manager.")
    flag.Parse()

    ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

    mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
        Scheme:             scheme,
        Metrics: metricsserver.Options{
            BindAddress: metricsAddr,
        },
        LeaderElection:   enableLeaderElection,
        LeaderElectionID: "nginx-lb-operator-lock",
    })
    if err != nil {
        setupLog.Error(err, "unable to start manager")
        os.Exit(1)
    }

    if err = (&controllers.ServiceReconciler{
        Client: mgr.GetClient(),
        Scheme: mgr.GetScheme(),
    }).SetupWithManager(mgr); err != nil {
        setupLog.Error(err, "unable to create controller", "controller", "Service")
        os.Exit(1)
    }

    // Add a Runnable to initialize IPAM and NGINX once the manager is started
    err = mgr.Add(ctrl.RunnableFunc(func(ctx context.Context) error {
        // Initialize IPAM module
        if err := ipam.Init(mgr.GetClient()); err != nil {
            setupLog.Error(err, "unable to initialize IPAM module")
            return err
        }

        // Initialize NGINX module
        if err := nginx.Init(mgr.GetClient()); err != nil {
            setupLog.Error(err, "unable to initialize NGINX module")
            return err
        }
        
        setupLog.Info("Successfully initialized IPAM and NGINX modules")
        return nil
    }))
    if err != nil {
        setupLog.Error(err, "unable to add runnable for initializing IPAM and NGINX")
        os.Exit(1)
    }

    // +kubebuilder:scaffold:builder

    setupLog.Info("starting manager")
    if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
        setupLog.Error(err, "problem running manager")
        os.Exit(1)
    }
}
