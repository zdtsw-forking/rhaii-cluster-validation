package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/annotator"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/gpu"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/networking"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/controller"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/runner"

	"github.com/spf13/cobra"
)

var (
	version      = "dev"
	defaultImage = "quay.io/opendatahub/rhaii-validator:latest"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "kubectl-rhaii_validate",
		Short:         "RHAII cluster validation - GPU, RDMA, and network checks",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `Validate GPU cluster readiness for AI/ML workloads.

  gpu          - GPU hardware checks (driver, ECC, memory)
  networking   - Network bandwidth tests (iperf3, RDMA)
  all          - Run all checks (gpu + networking)
  deps         - Check operators and CRDs (future)`,
	}

	rootCmd.AddCommand(newGPUCmd())
	rootCmd.AddCommand(newNetworkingCmd())
	rootCmd.AddCommand(newAllCmd())
	rootCmd.AddCommand(newDepsCmd())
	rootCmd.AddCommand(newCleanCmd())

	// Internal: agent mode (used by DaemonSet pods, not user-facing)
	rootCmd.AddCommand(newRunCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// --- gpu subcommand ---

func newGPUCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "gpu",
		Short: "Run GPU hardware checks on all GPU nodes",
		Long:  `Deploys agents to GPU nodes and validates GPU driver version, ECC status, and GPU health.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = "gpu"
			return runDeploy(opts, func(ctrl *controller.Controller) {
				// GPU checks only — no multi-node jobs
			})
		},
	}

	addDeployFlags(cmd, &opts)
	return cmd
}

// --- networking subcommand ---

func newNetworkingCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "networking",
		Short: "Run network bandwidth tests across GPU nodes",
		Long: `Tests TCP and RDMA bandwidth between GPU nodes using ring topology.
Requires 2+ GPU nodes. Each node is tested as both sender and receiver.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = "networking"
			return runDeploy(opts, func(ctrl *controller.Controller) {
				ctrl.AddJob(networking.NewIperfJob(0, nil))
				ctrl.AddJob(rdma.NewRDMABandwidthJob(0, nil))
			})
		},
	}

	addDeployFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run server on (default: ring topology)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Specific client nodes (default: all other GPU nodes)")
	return cmd
}

// --- all subcommand ---

func newAllCmd() *cobra.Command {
	var opts controller.Options

	cmd := &cobra.Command{
		Use:   "all",
		Short: "Run all checks (gpu + networking)",
		Long:  `Full cluster validation: GPU hardware checks on all nodes, then network bandwidth tests between nodes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.CheckMode = "all"
			return runDeploy(opts, func(ctrl *controller.Controller) {
				ctrl.AddJob(networking.NewIperfJob(0, nil))
				ctrl.AddJob(rdma.NewRDMABandwidthJob(0, nil))
			})
		},
	}

	addDeployFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.ServerNode, "server-node", "", "Node to run server on (default: ring topology)")
	cmd.Flags().StringSliceVar(&opts.ClientNodes, "client-nodes", nil, "Specific client nodes (default: all other GPU nodes)")
	return cmd
}

// --- deps subcommand (future) ---

func newDepsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deps",
		Short: "Check required operators and CRDs (coming soon)",
		Long: `Validates that required operators and CRDs are installed:
  - GPU Operator
  - Network Operator
  - RDMA shared device plugin
  - NFD (Node Feature Discovery)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Dependency checks not yet implemented.")
			fmt.Println("Planned checks:")
			fmt.Println("  - GPU Operator installed and running")
			fmt.Println("  - Network Operator installed and running")
			fmt.Println("  - RDMA shared device plugin present")
			fmt.Println("  - Node Feature Discovery labels present")
			return nil
		},
	}
	return cmd
}

// --- shared helpers ---

// runDeploy creates a controller, applies the setup function, and runs validation.
func runDeploy(opts controller.Options, setup func(*controller.Controller)) error {
	if opts.Image == "" {
		opts.Image = defaultImage
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctrl, err := controller.New(opts, os.Stdout)
	if err != nil {
		return err
	}

	setup(ctrl)
	return ctrl.Run(ctx)
}

// addDeployFlags adds common flags for commands that deploy to a cluster.
func addDeployFlags(cmd *cobra.Command, opts *controller.Options) {
	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&opts.Namespace, "namespace", "rhaii-validation", "Namespace for validation pods")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Agent container image")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Timeout for checks (default 5m)")
	cmd.Flags().StringVar(&opts.ConfigFile, "config", "", "Path to config override file")
	cmd.Flags().BoolVar(&opts.Debug, "debug", false, "Keep pods alive after run for debugging")
	cmd.Flags().StringVarP(&opts.OutputFormat, "output", "o", "table", "Output format: table or json")
}

// --- clean subcommand ---

func newCleanCmd() *cobra.Command {
	var (
		kubeconfig string
		namespace  string
	)

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove validation resources from cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := controller.Options{
				Kubeconfig: kubeconfig,
				Namespace:  namespace,
				Image:      "unused",
			}
			ctrl, err := controller.New(opts, os.Stdout)
			if err != nil {
				return err
			}
			return ctrl.Cleanup()
		},
	}

	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&namespace, "namespace", "rhaii-validation", "Namespace to clean")
	return cmd
}

// --- run subcommand (internal agent mode, used by DaemonSet pods) ---

func newRunCmd() *cobra.Command {
	var (
		nodeName     string
		bandwidth    bool
		iperfServer  string
		tcpThreshold float64
		configFile   string
		noWait       bool
	)

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run checks on current node (internal, used by DaemonSet)",
		Hidden: true, // not user-facing
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeName == "" {
				nodeName = os.Getenv("NODE_NAME")
			}
			if nodeName == "" {
				hostname, _ := os.Hostname()
				nodeName = hostname
			}

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Set up pod annotation updates (only when running in-cluster)
			setStatus := func(_ context.Context, _ string) {} // no-op for local runs
			if podName, podNS := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE"); podName != "" && podNS != "" {
				ann, err := annotator.New(podName, podNS)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to create annotator: %v\n", err)
				} else {
					setStatus = func(ctx context.Context, status string) {
						if err := ann.SetStatus(ctx, status); err != nil {
							fmt.Fprintf(os.Stderr, "Warning: failed to set status %q: %v\n", status, err)
						}
					}
				}
			}

			setStatus(ctx, annotator.StatusStarting)

			cfg, err := config.Load(config.PlatformUnknown, configFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to load config: %v, using defaults\n", err)
				cfg, _ = config.GetConfig(config.PlatformAKS)
			}
			fmt.Fprintf(os.Stderr, "Platform config: %s\n", cfg.Platform)

			r := runner.New(nodeName, os.Stdout)

			// Register checks based on mode (from CHECK_MODE env set by controller)
			vendor := os.Getenv("GPU_VENDOR")
			checkMode := os.Getenv("CHECK_MODE")
			if checkMode == "" {
				checkMode = "all"
			}

			// GPU checks (for "gpu" and "all" modes)
			if checkMode == "gpu" || checkMode == "all" {
				switch config.GPUVendor(vendor) {
				case config.GPUVendorAMD:
					r.AddCheck(gpu.NewAMDDriverCheck(nodeName, cfg.GPU.MinDriverVersion))
					r.AddCheck(gpu.NewAMDECCCheck(nodeName))
				default:
					r.AddCheck(gpu.NewDriverCheck(nodeName, cfg.GPU.MinDriverVersion))
					r.AddCheck(gpu.NewECCCheck(nodeName))
				}
			}

			// Networking checks (for "networking" and "all" modes)
			if checkMode == "networking" || checkMode == "all" {
				r.AddCheck(gpu.NewTopologyCheck(nodeName))
				r.AddCheck(rdma.NewDevicesCheck(nodeName))
				r.AddCheck(rdma.NewStatusCheck(nodeName))
			}


			if bandwidth {
				threshold := cfg.Thresholds.TCPBandwidth.Pass
				if cmd.Flags().Changed("tcp-threshold") {
					threshold = tcpThreshold
				}
				r.AddCheck(networking.NewTCPBandwidthCheck(nodeName, iperfServer, threshold))
			}

			setStatus(ctx, annotator.StatusRunning)

			report, err := r.Run(ctx)

			if err != nil {
				setStatus(ctx, annotator.StatusError)
			} else {
				setStatus(ctx, annotator.StatusDone)
			}

			hasFailures := err == nil && runner.HasFailures(report)

			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			} else if hasFailures {
				fmt.Fprintf(os.Stderr, "Validation failed: one or more checks reported FAIL\n")
			} else {
				fmt.Fprintf(os.Stderr, "Validation complete: all checks passed\n")
			}

			if noWait {
				if err != nil {
					return err
				}
				if hasFailures {
					return fmt.Errorf("validation failed: one or more checks reported FAIL")
				}
				return nil
			}

			fmt.Fprintf(os.Stderr, "Waiting for controller to collect results...\n")
			<-ctx.Done()
			return nil
		},
	}

	cmd.Flags().StringVar(&nodeName, "node-name", "", "Node name (auto-detected)")
	cmd.Flags().BoolVar(&bandwidth, "bandwidth", false, "Run bandwidth tests")
	cmd.Flags().StringVar(&iperfServer, "iperf-server", "", "iperf3 server IP")
	cmd.Flags().Float64Var(&tcpThreshold, "tcp-threshold", 25.0, "TCP bandwidth pass threshold (Gbps)")
	cmd.Flags().StringVar(&configFile, "config", "", "Path to config file")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Exit after checks (for local/CI)")

	return cmd
}
