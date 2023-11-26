/*
Copyright 2022. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	"github.com/projectsveltos/libsveltos/lib/clusterproxy"
	"github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/sveltos-agent/controllers"
	"github.com/projectsveltos/sveltos-agent/pkg/evaluation"
	//+kubebuilder:scaffold:imports
)

const (
	noReports = "do-not-send-reports"

	managedCluster = "managed-cluster"
)

var (
	setupLog         = ctrl.Log.WithName("setup")
	metricsAddr      string
	probeAddr        string
	runMode          string
	deployedCluster  string
	clusterNamespace string
	clusterName      string
	clusterType      string
	restConfigQPS    float32
	restConfigBurst  int
	webhookPort      int
	syncPeriod       time.Duration
)

func main() {
	scheme, err := controllers.InitScheme()
	if err != nil {
		os.Exit(1)
	}

	klog.InitFlags(nil)

	initFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(klog.Background())

	ctx := ctrl.SetupSignalHandler()

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst
	if deployedCluster != managedCluster {
		// if sveltos-agent is running in the management cluster, get the kubeconfig
		// of the managed cluster
		restConfig = getManagedClusterRestConfig(ctx, restConfig, ctrl.Log.WithName("get-kubeconfig"))
	}

	logsettings.RegisterForLogSettings(ctx,
		libsveltosv1alpha1.ComponentClassifierAgent, ctrl.Log.WithName("log-setter"),
		restConfig)

	ctrlOptions := ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(
			webhook.Options{
				Port: webhookPort,
			}),
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
		},
	}

	mgr, err := ctrl.NewManager(restConfig, ctrlOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	doSendReports := true
	sendReports := controllers.SendReports // do not send reports
	if runMode == noReports {
		sendReports = controllers.DoNotSendReports
		doSendReports = false
	}

	const intervalInSecond = 10
	evaluation.InitializeManager(ctx, mgr.GetLogger(),
		mgr.GetConfig(), mgr.GetClient(), clusterNamespace, clusterName, libsveltosv1alpha1.ClusterType(clusterType),
		intervalInSecond, doSendReports)

	startControllers(mgr, sendReports)
	//+kubebuilder:scaffold:builder

	setupChecks(mgr)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&metricsAddr,
		"metrics-bind-address",
		":8080",
		"The address the metric endpoint binds to.")

	fs.StringVar(&probeAddr,
		"health-probe-bind-address",
		":8081",
		"The address the probe endpoint binds to.")

	flag.StringVar(
		&runMode,
		"run-mode",
		noReports,
		"indicates whether reports will be sent to management cluster or just created locally",
	)

	flag.StringVar(
		&deployedCluster,
		"current-cluster",
		managedCluster,
		"Indicate whether drift-detection-manager was deployed in the managed or the management cluster. "+
			"Possible options are managed-cluster or management-cluster.",
	)

	flag.StringVar(
		&clusterNamespace,
		"cluster-namespace",
		"",
		"cluster namespace",
	)

	flag.StringVar(
		&clusterName,
		"cluster-name",
		"",
		"cluster name",
	)

	flag.StringVar(
		&clusterType,
		"cluster-type",
		"",
		"cluster type",
	)

	const defautlRestConfigQPS = 40
	fs.Float32Var(&restConfigQPS, "kube-api-qps", defautlRestConfigQPS,
		fmt.Sprintf("Maximum queries per second from the controller client to the Kubernetes API server. Defaults to %d",
			defautlRestConfigQPS))

	const defaultRestConfigBurst = 60
	fs.IntVar(&restConfigBurst, "kube-api-burst", defaultRestConfigBurst,
		fmt.Sprintf("Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server. Default %d",
			defaultRestConfigBurst))

	const defaultWebhookPort = 9443
	fs.IntVar(&webhookPort, "webhook-port", defaultWebhookPort,
		"Webhook Server port")

	const defaultSyncPeriod = 10
	fs.DurationVar(&syncPeriod, "sync-period", defaultSyncPeriod*time.Minute,
		fmt.Sprintf("The minimum interval at which watched resources are reconciled (e.g. 15m). Default: %d minutes",
			defaultSyncPeriod))
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

func startControllers(mgr manager.Manager, sendReports controllers.Mode) {
	var err error
	// Do not change order. ClassifierReconciler initializes classification manager.
	// NodeReconciler uses classification manager.
	if err = (&controllers.ClassifierReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		RunMode:            sendReports,
		Mux:                sync.RWMutex{},
		GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
		VersionClassifiers: libsveltosset.Set{},
		ClusterNamespace:   clusterNamespace,
		ClusterName:        clusterName,
		ClusterType:        libsveltosv1alpha1.ClusterType(clusterType),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Classifier")
		os.Exit(1)
	}

	if err = (&controllers.NodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: mgr.GetConfig(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	if err = (&controllers.HealthCheckReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		RunMode:          sendReports,
		Mux:              sync.RWMutex{},
		GVKHealthChecks:  make(map[schema.GroupVersionKind]*libsveltosset.Set),
		ClusterNamespace: clusterNamespace,
		ClusterName:      clusterName,
		ClusterType:      libsveltosv1alpha1.ClusterType(clusterType),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HealthCheck")
		os.Exit(1)
	}

	if err = (&controllers.HealthCheckReportReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HealthCheckReport")
		os.Exit(1)
	}

	if err = (&controllers.EventSourceReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		RunMode:          sendReports,
		Mux:              sync.RWMutex{},
		GVKEventSources:  make(map[schema.GroupVersionKind]*libsveltosset.Set),
		ClusterNamespace: clusterNamespace,
		ClusterName:      clusterName,
		ClusterType:      libsveltosv1alpha1.ClusterType(clusterType),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EventSource")
		os.Exit(1)
	}

	if err = (&controllers.EventReportReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EventReport")
		os.Exit(1)
	}

	if err = (&controllers.ReloaderReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		RunMode:          sendReports,
		Mux:              sync.RWMutex{},
		GVKReloaders:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
		ClusterNamespace: clusterNamespace,
		ClusterName:      clusterName,
		ClusterType:      libsveltosv1alpha1.ClusterType(clusterType),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Reloader")
		os.Exit(1)
	}
}

func getManagedClusterRestConfig(ctx context.Context, cfg *rest.Config, logger logr.Logger) *rest.Config {
	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", clusterType, clusterNamespace, clusterName))
	logger.V(logsettings.LogInfo).Info("get secret with kubeconfig")

	// When running in the management cluster, drift-detection-manager will need
	// to access Secret and Cluster/SveltosCluster (to verify existence)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := libsveltosv1alpha1.AddToScheme(s); err != nil {
		panic(1)
	}

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		panic(1)
	}

	// In this mode, drift-detection-manager is running in the management cluster.
	// It access the managed cluster from here.
	var currentCfg *rest.Config
	currentCfg, err = clusterproxy.GetKubernetesRestConfig(ctx, c, clusterNamespace, clusterName, "", "",
		libsveltosv1alpha1.ClusterType(clusterType), klogr.New())
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get secret: %v", err))
		panic(1)
	}

	return currentCfg
}
