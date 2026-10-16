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
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	functionsv1alpha1 "github.com/alexanderwink/functions-operator/api/v1alpha1"
	"github.com/alexanderwink/functions-operator/internal/controller"
	"github.com/alexanderwink/functions-operator/internal/fetcher"
	"github.com/alexanderwink/functions-operator/internal/resources"
	"github.com/alexanderwink/functions-operator/internal/source"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(functionsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var runtimeImage, interceptorHost, s3SecretNamespace string
	var latestPollInterval time.Duration
	flag.StringVar(&runtimeImage, "runtime-image", "",
		"Image with funcrt and the fetcher, used for every function pod (required).")
	flag.StringVar(&interceptorHost, "interceptor-host", "keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local",
		"Host the public ExternalName Service of each function points at (the KEDA HTTP add-on interceptor).")
	flag.StringVar(&s3SecretNamespace, "s3-secret-namespace", operatorNamespace(),
		"Namespace holding the "+resources.S3SecretName+" Secret (keys endpoint, accessKey, secretKey, useSSL). "+
			"Defaults to the operator's namespace.")
	flag.DurationVar(&latestPollInterval, "latest-poll-interval", 30*time.Second,
		"How often Functions on version \"latest\" re-check their S3 ETags.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if runtimeImage == "" {
		setupLog.Error(nil, "--runtime-image is required")
		os.Exit(1)
	}
	if latestPollInterval <= 0 {
		setupLog.Error(nil, "--latest-poll-interval must be positive", "value", latestPollInterval)
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	restCfg := ctrl.GetConfigOrDie()
	s3, err := newS3Client(restCfg, s3SecretNamespace)
	if err != nil {
		setupLog.Error(err, "Failed to configure S3 client")
		os.Exit(1)
	}

	// Only cache the workload objects this operator labels, not every
	// Deployment/Service/CronJob in the cluster.
	owned, err := labels.NewRequirement(resources.LabelFunction, selection.Exists, nil)
	if err != nil {
		setupLog.Error(err, "Failed to build cache selector")
		os.Exit(1)
	}
	ownedSel := labels.NewSelector().Add(*owned)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}: {Label: ownedSel},
			&corev1.Service{}:    {Label: ownedSel},
			&batchv1.CronJob{}:   {Label: ownedSel},
			&corev1.Pod{}:        {Label: ownedSel},
		}},
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2aaeb874.pulsen.se",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if _, err := mgr.GetRESTMapper().RESTMapping(controller.HTTPScaledObjectGVK.GroupKind(),
		controller.HTTPScaledObjectGVK.Version); err != nil {
		setupLog.Error(err, "HTTPScaledObject CRD (http.keda.sh/v1alpha1) not found: "+
			"KEDA and the KEDA HTTP add-on must be installed before the operator")
		os.Exit(1)
	}

	if err := (&controller.FunctionReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		APIReader:          mgr.GetAPIReader(),
		Resolver:           source.NewS3Resolver(s3),
		RuntimeImage:       runtimeImage,
		InterceptorHost:    interceptorHost,
		S3SecretNamespace:  s3SecretNamespace,
		LatestPollInterval: latestPollInterval,
		Now:                time.Now,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "function")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// operatorNamespace is the namespace the operator runs in, falling back to
// the kustomize default when running outside a cluster.
func operatorNamespace() string {
	if b, err := os.ReadFile(saNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "functions-operator-system"
}

// newS3Client builds the S3 client from the functions-s3-read Secret, read
// once at startup without a cache.
func newS3Client(cfg *rest.Config, ns string) (*minio.Client, error) {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	sec := &corev1.Secret{}
	key := client.ObjectKey{Namespace: ns, Name: resources.S3SecretName}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Get(ctx, key, sec); err != nil {
		return nil, fmt.Errorf("reading Secret %s (create it with keys endpoint, accessKey, secretKey, useSSL): %w", key, err)
	}
	endpoint, opts, err := s3Options(sec.Data)
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", key, err)
	}
	return minio.New(endpoint, opts)
}

// s3Options validates the functions-s3-read Secret data: endpoint, accessKey,
// secretKey and useSSL are all required, and useSSL is parsed exactly as the
// fetcher parses it (fetcher.ParseUseSSL).
func s3Options(data map[string][]byte) (string, *minio.Options, error) {
	var missing []string
	for _, k := range []string{"endpoint", "accessKey", "secretKey", "useSSL"} {
		if len(data[k]) == 0 {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("missing keys: %s", strings.Join(missing, ", "))
	}
	useSSL, err := fetcher.ParseUseSSL(string(data["useSSL"]))
	if err != nil {
		return "", nil, err
	}
	return string(data["endpoint"]), &minio.Options{
		Creds:  credentials.NewStaticV4(string(data["accessKey"]), string(data["secretKey"]), ""),
		Secure: useSSL,
		Region: "us-east-1", // same as the fetcher; avoids GetBucketLocation
	}, nil
}
