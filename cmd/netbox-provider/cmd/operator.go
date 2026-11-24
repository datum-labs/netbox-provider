// SPDX-License-Identifier: AGPL-3.0-only

package cmd

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.miloapis.com/instrumentation/buildinfo"
	"go.miloapis.com/instrumentation/logging"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/config"
	"go.miloapis.com/netbox-provider/internal/controller"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))
	utilruntime.Must(netboxv1alpha1.AddToScheme(scheme))
}

func newOperatorCommand(info BuildInfo) *cobra.Command {
	var (
		leaderElectionNamespace string
		watchNamespace          string
		probeAddr               string
		serverConfigFile        string
	)

	opts := zap.Options{}

	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Run the netbox-provider operator (controller-runtime manager)",
		RunE: func(cmd *cobra.Command, args []string) error {
			buildinfo.SetVersion(info.Version)
			ctrl.SetLogger(logging.New(&opts))

			setupLog := ctrl.Log.WithName("setup")
			setupLog.Info("starting netbox-provider operator",
				"version", info.Version,
				"gitCommit", info.GitCommit,
				"gitTreeState", info.GitTreeState,
				"buildDate", info.BuildDate,
			)

			var serverConfig config.NetBoxSourceOperator
			var configData []byte
			if len(serverConfigFile) > 0 {
				var err error
				configData, err = os.ReadFile(serverConfigFile)
				if err != nil {
					return fmt.Errorf("reading server config from %q: %w", serverConfigFile, err)
				}
			}

			if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
				return fmt.Errorf("decoding server config: %w", err)
			}

			setupLog.Info("server config loaded", "kubeconfigPath", serverConfig.KubeconfigPath,
				"fleetKubeconfigPath", serverConfig.Fleet.KubeconfigPath)

			cfg, err := serverConfig.RestConfig()
			if err != nil {
				return fmt.Errorf("loading rest config: %w", err)
			}

			ctx := ctrl.SetupSignalHandler()

			bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("creating bootstrap client: %w", err)
			}

			metricsServerOptions := serverConfig.MetricsServer.Options(ctx, bootstrapClient)

			var webhookServer webhook.Server
			if serverConfig.WebhookServer != nil {
				webhookServer = webhook.NewServer(
					serverConfig.WebhookServer.Options(ctx, bootstrapClient),
				)
			} else {
				setupLog.Info("webhookServer not configured; admission webhook server disabled")
			}

			// NetBoxSources, NetBoxMappings and token Secrets are read and
			// watched only in the watched namespace, so the service account
			// needs no cluster-wide role (see config/README.md). Secrets are
			// cached as metadata only.
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:                  scheme,
				Cache:                   controller.CacheOptions(watchNamespace),
				Metrics:                 metricsServerOptions,
				WebhookServer:           webhookServer,
				HealthProbeBindAddress:  probeAddr,
				LeaderElection:          leaderElectionNamespace != "",
				LeaderElectionID:        "netbox-provider-manager.netbox.miloapis.com",
				LeaderElectionNamespace: leaderElectionNamespace,
			})
			if err != nil {
				return fmt.Errorf("starting manager: %w", err)
			}

			// Fleet records are read and created through an uncached client so
			// every plan is made against current state, never a stale cache.
			fleetCfg, err := serverConfig.FleetRestConfig()
			if err != nil {
				return fmt.Errorf("loading Fleet rest config: %w", err)
			}
			fleetClient, err := client.New(fleetCfg, client.Options{})
			if err != nil {
				return fmt.Errorf("creating Fleet client: %w", err)
			}

			if err = (&controller.NetBoxSourceReconciler{
				Client:    mgr.GetClient(),
				APIReader: mgr.GetAPIReader(),
				Fleet:     fleetClient,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("creating NetBoxSource controller: %w", err)
			}

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				return fmt.Errorf("setting up health check: %w", err)
			}
			if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
				return fmt.Errorf("setting up ready check: %w", err)
			}

			setupLog.Info("starting manager")
			if err := mgr.Start(ctx); err != nil {
				return fmt.Errorf("running manager: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	cmd.Flags().StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Namespace of the leader-election lease. Setting it enables leader election, so only one replica reconciles.")
	cmd.Flags().StringVar(&watchNamespace, "namespace", "",
		"Only namespace whose NetBoxSources, NetBoxMappings and token Secrets are read and watched. Empty reads every namespace, watches the metadata of every Secret, and needs cluster-wide access.")
	cmd.Flags().StringVar(&serverConfigFile, "config", "", "Path to the operator config file.")

	// zap.Options.BindFlags accepts *flag.FlagSet (stdlib). Bridge via pflag's
	// AddGoFlagSet so the zap flags are surfaced on the cobra command.
	zapFlags := flag.NewFlagSet("zap", flag.ContinueOnError)
	opts.BindFlags(zapFlags)
	cmd.Flags().AddGoFlagSet(zapFlags)

	return cmd
}
