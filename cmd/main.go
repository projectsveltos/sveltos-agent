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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/clusterproxy"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
	"github.com/projectsveltos/libsveltos/lib/logsettings"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
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
	setupLog            = ctrl.Log.WithName("setup")
	diagnosticsAddress  string
	insecureDiagnostics bool
	runMode             string
	deployedCluster     string
	clusterNamespace    string
	clusterName         string
	clusterType         string
	restConfigQPS       float32
	restConfigBurst     int
	webhookPort         int
	syncPeriod          time.Duration
	healthAddr          string
	version             string
)

// Add RBAC for the authorized diagnostics endpoint.
//+kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
//+kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

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
	registerForLogSettings(ctx, ctrl.Log.WithName("log-setter"))

	// If the agent is running in the management cluster, the token to access the managed
	// cluster can expire and be renewed. In such a case, it is needed to create a new manager,
	// start again all controllers and the evaluation manager.
	// A context with a Done channel is created and if we detect the token has expired (by
	// getting unathorized errors), the context channel is closed which terminates the manager
	// and the evaluation manager.
	for {
		ctxWithCancel, cancel := context.WithCancel(ctx)
		restConfig := ctrl.GetConfigOrDie()
		if deployedCluster != managedCluster {
			// if sveltos-agent is running in the management cluster, get the kubeconfig
			// of the managed cluster
			restConfig = getManagedClusterRestConfig(ctxWithCancel, restConfig, ctrl.Log.WithName("get-kubeconfig"))
		}
		restConfig.QPS = restConfigQPS
		restConfig.Burst = restConfigBurst

		ctrlOptions := ctrl.Options{
			Scheme:                 scheme,
			Metrics:                getDiagnosticsOptions(),
			HealthProbeBindAddress: healthAddr,
			WebhookServer: webhook.NewServer(
				webhook.Options{
					Port: webhookPort,
				}),
			Cache: cache.Options{
				SyncPeriod: &syncPeriod,
			},
			Controller: config.Controller{
				// This is needed to avoid the controller with name xyz already exists
				SkipNameValidation: ptr.To(true),
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

		const intervalInSecond = int64(3)
		evaluation.InitializeManager(ctxWithCancel, mgr.GetLogger(),
			mgr.GetConfig(), mgr.GetClient(), clusterNamespace, clusterName, version,
			libsveltosv1beta1.ClusterType(clusterType), intervalInSecond, doSendReports)

		go startControllers(ctxWithCancel, mgr, sendReports)

		//+kubebuilder:scaffold:builder

		if deployedCluster != managedCluster {
			// if sveltos-agent is running in the management cluster, get the kubeconfig
			// of the managed cluster
			go restartIfNeeded(ctxWithCancel, cancel, restConfig, ctrl.Log.WithName("restarter"))
		}

		setupChecks(mgr)

		setupLog.Info("starting manager")
		if err := mgr.Start(ctxWithCancel); err != nil {
			setupLog.Error(err, "problem running manager")
			os.Exit(1)
		}

		evaluationMgr := evaluation.GetManager()
		evaluationMgr.Reset()
	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&diagnosticsAddress, "diagnostics-address", ":8443",
		"The address the diagnostics endpoint binds to. Per default metrics are served via https and with"+
			"authentication/authorization. To serve via http and without authentication/authorization set --insecure-diagnostics."+
			"If --insecure-diagnostics is not set the diagnostics endpoint also serves pprof endpoints and an endpoint to change the log level.")

	fs.BoolVar(&insecureDiagnostics, "insecure-diagnostics", false,
		"Enable insecure diagnostics serving. For more details see the description of --diagnostics-address.")

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
		"Indicate whether sveltos-agent was deployed in the managed or the management cluster. "+
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

	flag.StringVar(
		&version,
		"version",
		"",
		"indicates sveltos-agent version",
	)

	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")

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

func startClassifierReconciler(ctx context.Context, mgr manager.Manager, sendReports controllers.Mode) {
	for {
		isPresent, err := isCRDPresent(ctx, mgr.GetClient(), "classifiers.lib.projectsveltos.io")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		if isPresent {
			setupLog.V(logs.LogInfo).Info("start classifier/node controller")
			nodeReconciler := &controllers.NodeReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
				Config: mgr.GetConfig(),
			}
			if err := nodeReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "Node")
				os.Exit(1)
			}

			// Do not change order. ClassifierReconciler initializes classification manager.
			// NodeReconciler uses classification manager.
			classifierReconciler := &controllers.ClassifierReconciler{
				Client:             mgr.GetClient(),
				Scheme:             mgr.GetScheme(),
				RunMode:            sendReports,
				Mux:                sync.RWMutex{},
				GVKClassifiers:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
				VersionClassifiers: libsveltosset.Set{},
				ClusterNamespace:   clusterNamespace,
				ClusterName:        clusterName,
				ClusterType:        libsveltosv1beta1.ClusterType(clusterType),
			}
			if err := classifierReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "Classifier")
				os.Exit(1)
			}
		}
		break
	}
}

func startHealthCheckReconciler(ctx context.Context, mgr manager.Manager, sendReports controllers.Mode) {
	for {
		isPresent, err := isCRDPresent(ctx, mgr.GetClient(), "healthchecks.lib.projectsveltos.io")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		if isPresent {
			setupLog.V(logs.LogInfo).Info("start healthCheck/healthCheckReport controllers")
			healthCheckReconciler := &controllers.HealthCheckReconciler{
				Client:           mgr.GetClient(),
				Scheme:           mgr.GetScheme(),
				RunMode:          sendReports,
				Mux:              sync.RWMutex{},
				GVKHealthChecks:  make(map[schema.GroupVersionKind]*libsveltosset.Set),
				ClusterNamespace: clusterNamespace,
				ClusterName:      clusterName,
				ClusterType:      libsveltosv1beta1.ClusterType(clusterType),
			}
			if err := healthCheckReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "HealthCheck")
				os.Exit(1)
			}

			healthCheckReportReconciler := &controllers.HealthCheckReportReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}
			if err := healthCheckReportReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "HealthCheckReport")
				os.Exit(1)
			}
		}

		break
	}
}

func startEventSourceReconciler(ctx context.Context, mgr manager.Manager, sendReports controllers.Mode) {
	for {
		isPresent, err := isCRDPresent(ctx, mgr.GetClient(), "eventsources.lib.projectsveltos.io")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		if isPresent {
			setupLog.V(logs.LogInfo).Info("start eventSource/eventReport controllers")
			eventSourceReconciler := &controllers.EventSourceReconciler{
				Client:           mgr.GetClient(),
				Scheme:           mgr.GetScheme(),
				RunMode:          sendReports,
				Mux:              sync.RWMutex{},
				GVKEventSources:  make(map[schema.GroupVersionKind]*libsveltosset.Set),
				ClusterNamespace: clusterNamespace,
				ClusterName:      clusterName,
				ClusterType:      libsveltosv1beta1.ClusterType(clusterType),
			}
			if err := eventSourceReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "EventSource")
				os.Exit(1)
			}

			eventReportReconciler := &controllers.EventReportReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}
			if err := eventReportReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "EventReport")
				os.Exit(1)
			}
		}

		break
	}
}

func startReloaderReconciler(ctx context.Context, mgr manager.Manager, sendReports controllers.Mode) {
	for {
		isPresent, err := isCRDPresent(ctx, mgr.GetClient(), "reloaders.lib.projectsveltos.io")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		if isPresent {
			setupLog.V(logs.LogInfo).Info("start reloader controllers")
			reloaderReconciler := &controllers.ReloaderReconciler{
				Client:           mgr.GetClient(),
				Scheme:           mgr.GetScheme(),
				RunMode:          sendReports,
				Mux:              sync.RWMutex{},
				GVKReloaders:     make(map[schema.GroupVersionKind]*libsveltosset.Set),
				ClusterNamespace: clusterNamespace,
				ClusterName:      clusterName,
				ClusterType:      libsveltosv1beta1.ClusterType(clusterType),
			}
			if err := reloaderReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "Reloader")
				os.Exit(1)
			}
		}

		break
	}
}

func startControllers(ctx context.Context, mgr manager.Manager, sendReports controllers.Mode) {
	for {
		// wait for debugging configuration CRD so we know we are ready to check for other CRDs
		// This must always exist. So isCRDPresent is not used, rather we keep trying till we
		// successfully get it
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := mgr.GetClient().Get(ctx, types.NamespacedName{Name: "debuggingconfigurations.lib.projectsveltos.io"}, crd)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		setupLog.V(logsettings.LogInfo).Info("verified debuggingconfigurations")
		break
	}

	startClassifierReconciler(ctx, mgr, sendReports)

	startHealthCheckReconciler(ctx, mgr, sendReports)

	startEventSourceReconciler(ctx, mgr, sendReports)

	startReloaderReconciler(ctx, mgr, sendReports)
}

func getManagedClusterRestConfig(ctx context.Context, cfg *rest.Config, logger logr.Logger) *rest.Config {
	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", clusterType, clusterNamespace, clusterName))
	logger.V(logsettings.LogInfo).Info("get secret with kubeconfig")

	// When running in the management cluster, sveltos-agent will need
	// to access Secret and Cluster/SveltosCluster (to verify existence)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := libsveltosv1beta1.AddToScheme(s); err != nil {
		panic(1)
	}

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		panic(1)
	}

	// In this mode, sveltos-agent is running in the management cluster.
	// It access the managed cluster from here.
	var currentCfg *rest.Config
	currentCfg, err = clusterproxy.GetKubernetesRestConfig(ctx, c, clusterNamespace, clusterName, "", "",
		libsveltosv1beta1.ClusterType(clusterType),
		textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get secret: %v", err))
		panic(1)
	}

	return currentCfg
}

func isCRDPresent(ctx context.Context, c client.Client, crdName string) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}

	err := c.Get(ctx, types.NamespacedName{Name: crdName}, crd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// getDiagnosticsOptions returns metrics options which can be used to configure a Manager.
func getDiagnosticsOptions() metricsserver.Options {
	// If "--insecure-diagnostics" is set, serve metrics via http
	// and without authentication/authorization.
	if insecureDiagnostics {
		return metricsserver.Options{
			BindAddress:   diagnosticsAddress,
			SecureServing: false,
		}
	}

	// If "--insecure-diagnostics" is not set, serve metrics via https
	// and with authentication/authorization. As the endpoint is protected,
	// we also serve pprof endpoints and an endpoint to change the log level.
	return metricsserver.Options{
		BindAddress:    diagnosticsAddress,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}
}

func restartIfNeeded(ctx context.Context, cancel context.CancelFunc, restConfig *rest.Config, logger logr.Logger) {
	for {
		const interval = 10 * time.Second
		time.Sleep(interval)
		_, err := k8s_utils.GetKubernetesVersion(ctx, restConfig, logger)
		if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("IsUnauthorized/IsForbidden %v. Cancel context", err))
			cancel()
			break
		}
	}
}

func registerForLogSettings(ctx context.Context, logger logr.Logger) {
	restConfig := ctrl.GetConfigOrDie()
	logsettings.RegisterForLogSettings(ctx,
		libsveltosv1beta1.ComponentSveltosAgent, logger,
		restConfig)
}
