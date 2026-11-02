/*
Copyright 2026.

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
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	seaplanedb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"
	hd "github.com/openshift-online/rosa-seaplane-kube-applier/seaplane-dynamo/dynamodb"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/controller"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo/statusstream"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/metrics"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/oidc"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/silence"
)

var setupLog = ctrl.Log.WithName("setup")

func main() {
	var metricsAddr string
	var probeAddr string
	var awsRegion string
	var baseDomain string
	var maxConcurrentReconciles int
	var watcherPollInterval time.Duration
	var watcherRelistInterval time.Duration
	var watcherMaxLookback time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&awsRegion, "aws-region", "", "AWS region for DynamoDB and EKS (required).")
	flag.StringVar(&baseDomain, "base-domain", "", "DNS base domain for hosted clusters (required).")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 10,
		"Maximum number of concurrent reconciles per controller.")
	flag.DurationVar(&watcherPollInterval, "dynamo-poll-interval", 0,
		"How often the DynamoDB GSI fast poller runs (0 uses the default: 15s).")
	flag.DurationVar(&watcherRelistInterval, "dynamo-relist-interval", 0,
		"How often the DynamoDB full consistent relist runs (0 uses the default: 5m).")
	flag.DurationVar(&watcherMaxLookback, "dynamo-max-lookback", 0,
		"Maximum lookback window for the GSI fast poller (0 defaults to --dynamo-relist-interval).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if awsRegion == "" {
		setupLog.Error(nil, "--aws-region is required")
		os.Exit(1)
	}
	if baseDomain == "" {
		setupLog.Error(nil, "--base-domain is required")
		os.Exit(1)
	}

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		setupLog.Error(nil, "POSTGRES_DSN environment variable is required")
		os.Exit(1)
	}

	replicaCount := envInt("REPLICA_COUNT", 1)
	ordinal, err := podOrdinal()
	if err != nil {
		setupLog.Error(err, "Failed to parse pod ordinal from hostname")
		os.Exit(1)
	}

	setupLog.Info("shard config",
		"replicaCount", replicaCount,
		"ordinal", ordinal,
	)

	signalCtx := ctrl.SetupSignalHandler()
	ctx := context.Background()

	// Load AWS config — credentials come from Pod Identity / IRSA automatically.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(awsRegion))
	if err != nil {
		setupLog.Error(err, "Failed to load AWS config")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "Failed to add v1alpha1 to scheme")
		os.Exit(1)
	}
	if err := hypershiftv1beta1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "Failed to add HyperShift v1beta1 to scheme")
		os.Exit(1)
	}

	log := ctrl.Log.WithName("pgruntime")

	mgr, err := seaplanedb.NewManager(seaplanedb.Options{
		Scheme: scheme,
		DSN:    dsn,
		Shard: &seaplanedb.ShardConfig{
			Mod:   replicaCount,
			Owned: []int{ordinal},
			UnshardedGVKs: []schema.GroupVersionKind{
				v1alpha1.SchemeGroupVersion.WithKind("ManagementCluster"),
			},
		},
		Logger:                 log,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "Failed to create pgruntime manager")
		os.Exit(1)
	}

	dynamoDBClient := dynamodb.NewFromConfig(awsCfg)
	dynamoClient := dynamo.NewClient(dynamoDBClient)

	rcfg := render.RegionalConfig{
		BaseDomainSuffix: baseDomain,
		AWSRegion:        awsRegion,
		// Development override: when set, every HostedCluster is pinned to this
		// control-plane-operator image (empty disables the override).
		ControlPlaneOperatorImage: os.Getenv("CONTROL_PLANE_OPERATOR_IMAGE"),
	}

	eventRouter := controller.NewEventRouter()
	clusterStatusEvents := make(chan event.GenericEvent, 256)
	nodePoolStatusEvents := make(chan event.GenericEvent, 256)
	manifestStatusEvents := make(chan event.GenericEvent, 256)

	if err := (&controller.ClusterReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Dynamo:                  dynamoClient,
		RegionalConfig:          rcfg,
		StatusEvents:            clusterStatusEvents,
		EventRouter:             eventRouter,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cluster")
		os.Exit(1)
	}
	if err := (&controller.DNSReservationReconciler{
		Client:                  mgr.GetClient(),
		APIReader:               mgr.GetAPIReader(),
		Scheme:                  mgr.GetScheme(),
		RegionalConfig:          rcfg,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "dnsreservation")
		os.Exit(1)
	}
	if err := (&controller.NodePoolReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Dynamo:                  dynamoClient,
		StatusEvents:            nodePoolStatusEvents,
		EventRouter:             eventRouter,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "nodepool")
		os.Exit(1)
	}
	if err := (&controller.PlacementReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "placement")
		os.Exit(1)
	}
	if err := (&controller.ManifestReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Dynamo:                  dynamoClient,
		StatusEvents:            manifestStatusEvents,
		EventRouter:             eventRouter,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "manifest")
		os.Exit(1)
	}
	oidcClient := oidc.NewAWSClient(awsCfg)
	if err := (&controller.OidcConfigReconciler{
		Client:                  mgr.GetClient(),
		APIReader:               mgr.GetAPIReader(),
		Scheme:                  mgr.GetScheme(),
		OIDC:                    oidcClient,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "oidcconfig")
		os.Exit(1)
	}

	alertmanagerURL := os.Getenv("ALERTMANAGER_URL")
	if alertmanagerURL != "" {
		// ALERTMANAGER_URL is the Alertmanager v2 API base URL (for example
		// https://alertmanager.example.com). When set, the cluster silence
		// reconciler is registered; when unset, lifecycle silencing is disabled.
		setupLog.Info("Registering cluster silence reconciler")
		silenceMetrics := metrics.NewSilenceMetrics(crmetrics.Registry)
		if err := (&controller.SilenceReconciler{
			Client:                  mgr.GetClient(),
			SilenceClient:           silence.NewAlertmanagerClient(alertmanagerURL, nil),
			Metrics:                 silenceMetrics,
			MaxConcurrentReconciles: maxConcurrentReconciles,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create controller", "controller", "cluster-silence")
			os.Exit(1)
		}
	} else {
		setupLog.Info("ALERTMANAGER_URL not set; cluster silence reconciler disabled")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	streamMgr := statusstream.NewManager(
		dynamoDBClient,
		mgr.GetClient(),
		[]string{dynamo.TableSuffixStatusApplyDesires, dynamo.TableSuffixStatusReadDesires},
		func(documentID string, _ hd.Item) { eventRouter.Dispatch(documentID) },
		ctrl.Log.WithName("statusstream"),
		hd.Options{
			PollInterval:      watcherPollInterval,
			RelistInterval:    watcherRelistInterval,
			MaxLookbackWindow: watcherMaxLookback,
		},
	)
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go streamMgr.Run(watchCtx, 5*time.Second)

	setupLog.Info("Starting pgruntime manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

func envInt(key string, fallback int) int {
	s := os.Getenv(key)
	if s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		setupLog.Error(err, "Invalid integer env var", "key", key, "value", s)
		os.Exit(1)
	}
	return v
}

// podOrdinal extracts the StatefulSet ordinal from the hostname.
// e.g. "seaplane-operator-2" → 2. Returns 0 if no trailing number.
func podOrdinal() (int, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return 0, fmt.Errorf("get hostname: %w", err)
	}
	parts := strings.Split(hostname, "-")
	last := parts[len(parts)-1]
	ordinal, err := strconv.Atoi(last)
	if err != nil {
		return 0, nil
	}
	return ordinal, nil
}
