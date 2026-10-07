package main

import (
	"flag"
	"os"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/martin31821/eks-webhack/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	// clientgoscheme covers core v1, admissionregistration v1, discovery v1, coordination v1, etc.
	_ = clientgoscheme.AddToScheme(scheme)
	// apiextensionsv1 (CRDs) is not part of clientgoscheme.
	_ = apiextensionsv1.AddToScheme(scheme)

	// Explicit references silence unused-import linters for these packages.
	_ = admissionv1.SchemeGroupVersion
	_ = discoveryv1.SchemeGroupVersion
}

func main() {
	var metricsAddr string
	var probeAddr string
	var leaderElect bool
	var leaderElectionNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Namespace for leader election lease (defaults to POD_NAMESPACE env var).")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if leaderElectionNamespace == "" {
		leaderElectionNamespace = os.Getenv("POD_NAMESPACE")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "eks-webhack.martin31821.github.io",
		LeaderElectionNamespace: leaderElectionNamespace,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create ServiceReconciler")
		os.Exit(1)
	}

	if err := (&controller.EndpointProxyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create EndpointProxyReconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting eks-webhack manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
