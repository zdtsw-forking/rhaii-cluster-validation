package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opendatahub-io/rhaii-cluster-validation/deploy"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks/rdma"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/config"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	agentLabelKey   = "app"
	agentLabelValue = "rhaii-validator"
	configMapName   = "rhaii-validate-config"
	reportCMName    = "rhaii-validate-report"
	defaultTimeout  = 5 * time.Minute
	annotationKey   = "rhaii.opendatahub.io/validation-status"
	annotationDone  = "done"
	annotationError = "error"
)

// Options configures the controller behavior.
type Options struct {
	Kubeconfig   string
	Namespace    string
	Image        string
	Timeout      time.Duration
	ConfigFile   string
	ServerNode   string
	ClientNodes  []string
	Debug        bool   // Skip cleanup so user can exec into pods for debugging
	OutputFormat string // "table" (default) or "json"
	CheckMode    string // "all" (default), "gpu", "networking"
}

// Controller orchestrates agent deployment, result collection, and cleanup.
type Controller struct {
	client       kubernetes.Interface
	opts         Options
	cfg          config.PlatformConfig
	output       io.Writer
	platform     config.Platform
	gpuVendor    config.GPUVendor // auto-detected from node labels
	gpuNodeLabel string           // label used to discover GPU nodes (empty = fallback to resources)
	gpuNodes     []string         // discovered GPU node names
	jobs         []jobrunner.Job
}

// AddJob registers a multi-node job to run when --bandwidth is enabled.
func (c *Controller) AddJob(j jobrunner.Job) {
	c.jobs = append(c.jobs, j)
}

// storeReport saves the JSON report to a ConfigMap so it persists after cleanup.
func (c *Controller) storeReport(ctx context.Context, reports []checks.NodeReport, jobResults []jobrunner.JobResult) error {
	type jsonReport struct {
		Platform   string               `json:"platform"`
		Timestamp  string               `json:"timestamp"`
		Nodes      []checks.NodeReport  `json:"nodes"`
		JobResults []jobrunner.JobResult `json:"job_results,omitempty"`
		Summary    map[string]int       `json:"summary"`
		Status     string               `json:"status"`
	}

	pass, warn, fail, skip := 0, 0, 0, 0
	for _, report := range reports {
		for _, r := range report.Results {
			switch r.Status {
			case checks.StatusPass:
				pass++
			case checks.StatusWarn:
				warn++
			case checks.StatusFail:
				fail++
			case checks.StatusSkip:
				skip++
			}
		}
	}
	for _, jr := range jobResults {
		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}

	status := "READY"
	if fail > 0 {
		status = "NOT READY"
	} else if warn > 0 {
		status = "READY (with warnings)"
	}

	r := jsonReport{
		Platform:   string(c.platform),
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Nodes:      reports,
		JobResults: jobResults,
		Summary:    map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:     status,
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reportCMName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{agentLabelKey: agentLabelValue},
		},
		Data: map[string]string{
			"report.json": string(data),
		},
	}

	// Update if exists, create if not
	existing, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, reportCMName, metav1.GetOptions{})
	if err == nil {
		existing.Data = cm.Data
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(err) {
		_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	}

	if err != nil {
		return err
	}

	fmt.Fprintf(c.output, "  Report stored in ConfigMap %s/%s\n", c.opts.Namespace, reportCMName)
	return nil
}

// printDebugHelp lists actual pod names and useful debug commands.
func (c *Controller) printDebugHelp(ctx context.Context, gpuNodes []string) {
	ns := c.opts.Namespace

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "=== DEBUG MODE ===")
	fmt.Fprintln(c.output, "Pods kept alive for debugging.")
	fmt.Fprintln(c.output, "")

	// List actual agent pods
	pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{agentLabelKey: agentLabelValue}.AsSelector().String(),
	})
	if err == nil && len(pods.Items) > 0 {
		fmt.Fprintln(c.output, "Agent pods:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  %s (node: %s, status: %s)\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "View logs:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s %s\n", ns, pod.Name)
		}
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "Exec into pod:")
		for _, pod := range pods.Items {
			fmt.Fprintf(c.output, "  kubectl exec -it -n %s %s -- bash\n", ns, pod.Name)
		}
	}

	// List job resources
	jobs, err := c.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app=rhaii-validate-job",
	})
	if err == nil && len(jobs.Items) > 0 {
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "Job specs:")
		for _, j := range jobs.Items {
			fmt.Fprintf(c.output, "  kubectl get job %s -n %s -o yaml\n", j.Name, ns)
		}
		fmt.Fprintln(c.output, "")
		fmt.Fprintln(c.output, "Job logs:")
		for _, j := range jobs.Items {
			fmt.Fprintf(c.output, "  kubectl logs -n %s -l job-name=%s\n", ns, j.Name)
		}
	}

	fmt.Fprintln(c.output, "")
	fmt.Fprintln(c.output, "Debug commands inside agent pod:")
	fmt.Fprintln(c.output, "  chroot /host nvidia-smi")
	fmt.Fprintln(c.output, "  chroot /host ibv_devices")
	fmt.Fprintln(c.output, "  chroot /host ibstat")
	fmt.Fprintln(c.output, "  cat /host/proc/driver/nvidia/version")
	fmt.Fprintln(c.output, "  ls /dev/nvidia*")
	fmt.Fprintln(c.output, "")
	fmt.Fprintf(c.output, "Cleanup: kubectl rhaii-validate clean\n")
}

// Cleanup removes all validation resources from the cluster.
func (c *Controller) Cleanup() error {
	ctx := context.Background()
	fmt.Fprintln(c.output, "Cleaning up all validation resources...")

	// Delete all validation jobs
	propagation := metav1.DeletePropagationBackground
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=rhaii-validate-job",
	})
	if err == nil {
		for _, j := range jobs.Items {
			_ = c.client.BatchV1().Jobs(c.opts.Namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
		}
		if len(jobs.Items) > 0 {
			fmt.Fprintf(c.output, "  Deleted %d job(s)\n", len(jobs.Items))
		}
	}

	if err := c.cleanupAll(ctx); err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}
	fmt.Fprintln(c.output, "Done")
	return nil
}

// New creates a new Controller.
func New(opts Options, output io.Writer) (*Controller, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		loadingRules.ExplicitPath = opts.Kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	if opts.Namespace == "" {
		opts.Namespace = "rhaii-validation"
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}

	return &Controller{
		client: client,
		opts:   opts,
		output: output,
	}, nil
}

// Run executes the full validation lifecycle.
func (c *Controller) Run(ctx context.Context) error {
	fmt.Fprintln(c.output, "=== RHAII Cluster Validation ===")
	fmt.Fprintln(c.output)

	// Step 1: Cleanup previous runs (DaemonSet + leftover jobs)
	fmt.Fprintln(c.output, "[Step 1] Cleaning up previous runs...")
	if err := c.cleanupDaemonSet(ctx); err != nil {
		fmt.Fprintf(c.output, "  Warning: cleanup failed: %v\n", err)
	}
	c.cleanupJobs(ctx)

	// Step 2: Ensure namespace exists
	fmt.Fprintln(c.output, "[Step 2] Ensuring namespace exists...")
	if err := c.ensureNamespace(ctx); err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	// Step 3: Ensure RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding)
	fmt.Fprintln(c.output, "[Step 3] Ensuring RBAC...")
	if err := c.ensureRBAC(ctx); err != nil {
		return fmt.Errorf("failed to create RBAC: %w", err)
	}

	// Step 4: Detect platform and create config ConfigMap
	fmt.Fprintln(c.output, "[Step 4] Detecting platform and creating config...")
	if err := c.detectAndCreateConfig(ctx); err != nil {
		return fmt.Errorf("failed to create platform config: %w", err)
	}

	// OpenShift: grant privileged SCC to service account
	if c.platform == config.PlatformOCP {
		if err := c.ensureOpenShiftSCC(ctx); err != nil {
			fmt.Fprintf(c.output, "  Warning: failed to create SCC binding: %v\n", err)
		}
	}

	// Step 5: Discover GPU nodes
	fmt.Fprintln(c.output, "[Step 5] Discovering GPU nodes...")
	gpuNodes, err := c.discoverGPUNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU nodes: %w", err)
	}
	c.gpuNodes = gpuNodes
	if len(gpuNodes) == 0 {
		fmt.Fprintln(c.output, "  No GPU nodes found. Nothing to validate.")
		return nil
	}
	fmt.Fprintf(c.output, "  Found %d GPU node(s): %s\n", len(gpuNodes), strings.Join(gpuNodes, ", "))

	// Step 6: Deploy agent DaemonSet
	fmt.Fprintln(c.output, "[Step 6] Deploying agent DaemonSet...")
	if err := c.deployAgent(ctx); err != nil {
		return fmt.Errorf("failed to deploy agent: %w", err)
	}

	// Step 7: Wait for agents and collect results
	fmt.Fprintln(c.output, "[Step 7] Waiting for agents to complete and collecting results...")
	reports, err := c.waitAndCollect(ctx, gpuNodes)
	if err != nil {
		fmt.Fprintf(c.output, "  Warning: collection error: %v\n", err)
	}

	// Step 8: Run multi-node jobs (automatically when 2+ GPU nodes and jobs registered)
	var jobResults []jobrunner.JobResult
	if len(c.jobs) > 0 && len(gpuNodes) >= 2 {
		fmt.Fprintln(c.output, "[Step 8] Running multi-node tests...")
		jr, err := c.runBandwidthJobs(ctx, gpuNodes, reports)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: bandwidth test error: %v\n", err)
		}
		jobResults = jr
	}

	// Store report in ConfigMap (persists after cleanup)
	if err := c.storeReport(ctx, reports, jobResults); err != nil {
		fmt.Fprintf(c.output, "  Warning: failed to store report: %v\n", err)
	}

	// Print report
	var hasFailures bool
	if c.opts.OutputFormat == "json" {
		hasFailures = c.printJSONReport(reports, jobResults)
	} else {
		hasFailures = c.printReport(reports, jobResults)
	}

	// Cleanup or keep for debugging
	if c.opts.Debug {
		c.printDebugHelp(ctx, gpuNodes)
	} else {
		fmt.Fprintln(c.output, "Cleaning up...")
		if err := c.cleanupAll(ctx); err != nil {
			fmt.Fprintf(c.output, "  Warning: cleanup failed: %v\n", err)
		}
	}

	if len(reports) == 0 && len(gpuNodes) > 0 {
		if c.opts.Debug {
			return fmt.Errorf("failed to collect reports — pods kept alive for debugging")
		}
		return fmt.Errorf("failed to collect any reports from %d GPU node(s)", len(gpuNodes))
	}
	if hasFailures {
		return fmt.Errorf("validation failed: one or more checks reported FAIL")
	}

	return nil
}

func (c *Controller) detectAndCreateConfig(ctx context.Context) error {
	// Detect platform from cluster nodes
	c.platform = config.DetectPlatform(ctx, c.client)
	fmt.Fprintf(c.output, "  Detected platform: %s\n", c.platform)

	// Load embedded defaults (+ optional override from --config file)
	cfg, err := config.Load(c.platform, c.opts.ConfigFile)
	if err != nil {
		return fmt.Errorf("failed to load platform config: %w", err)
	}

	// Serialize config to YAML
	cfgYAML, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize platform config: %w", err)
	}

	// Create ConfigMap with the platform config
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: c.opts.Namespace,
			Labels:    map[string]string{agentLabelKey: agentLabelValue},
		},
		Data: map[string]string{
			"platform.yaml": string(cfgYAML),
		},
	}

	// Check if ConfigMap already exists (user may have pre-created or customized it)
	existing, err := c.client.CoreV1().ConfigMaps(c.opts.Namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err == nil {
		// ConfigMap exists — merge user's overrides on top of detected defaults
		if existingYAML, ok := existing.Data["platform.yaml"]; ok {
			// Unmarshal user config on top of detected defaults (only set fields are overridden)
			if yamlErr := yaml.Unmarshal([]byte(existingYAML), &cfg); yamlErr != nil {
				fmt.Fprintf(c.output, "  Warning: failed to parse existing ConfigMap YAML: %v\n", yamlErr)
			}
		}
		c.cfg = cfg
		fmt.Fprintf(c.output, "  ConfigMap %s/%s already exists, using existing config (platform: %s)\n",
			c.opts.Namespace, configMapName, cfg.Platform)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// ConfigMap doesn't exist — create it with detected defaults
	_, err = c.client.CoreV1().ConfigMaps(c.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	c.cfg = cfg
	fmt.Fprintf(c.output, "  Created ConfigMap %s/%s (platform: %s)\n", c.opts.Namespace, configMapName, c.platform)
	fmt.Fprintf(c.output, "  To customize: kubectl edit configmap %s -n %s\n", configMapName, c.opts.Namespace)
	return nil
}

// gpuNodeSelectors maps vendor to the node label used to discover GPU nodes.
var gpuNodeSelectors = []struct {
	vendor   config.GPUVendor
	selector string
}{
	{config.GPUVendorNVIDIA, "nvidia.com/gpu.present=true"},
	{config.GPUVendorAMD, "amd.com/gpu.present=true"},
}

func (c *Controller) discoverGPUNodes(ctx context.Context) ([]string, error) {
	// Try each vendor's node selector until we find GPU nodes
	// Try label-based discovery first
	for _, gs := range gpuNodeSelectors {
		nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: gs.selector,
		})
		if err != nil {
			continue
		}
		if len(nodes.Items) > 0 {
			c.gpuVendor = gs.vendor
			c.gpuNodeLabel = gs.selector
			var names []string
			for _, node := range nodes.Items {
				names = append(names, node.Name)
			}
			fmt.Fprintf(c.output, "  GPU vendor: %s (auto-detected from node labels)\n", gs.vendor)
			return names, nil
		}
	}

	// Fallback: scan all nodes for GPU resources in allocatable
	allNodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	var names []string
	for _, node := range allNodes.Items {
		for _, resName := range gpuResourceNames {
			if qty, ok := node.Status.Allocatable[resName]; ok && qty.Value() > 0 {
				names = append(names, node.Name)
				if c.gpuVendor == "" {
					if strings.Contains(string(resName), "nvidia") {
						c.gpuVendor = config.GPUVendorNVIDIA
					} else if strings.Contains(string(resName), "amd") {
						c.gpuVendor = config.GPUVendorAMD
					}
				}
				break
			}
		}
	}
	if len(names) > 0 {
		fmt.Fprintf(c.output, "  GPU vendor: %s (auto-detected from node resources)\n", c.gpuVendor)
	}
	return names, nil
}

// gpuResourceNames are the known extended resource names for GPUs across vendors.
var gpuResourceNames = []corev1.ResourceName{
	"nvidia.com/gpu",
	"amd.com/gpu",
}

// detectGPUResources scans GPU nodes and returns the GPU resource name and the
// minimum allocatable count across all nodes.
func (c *Controller) detectGPUResources(ctx context.Context, gpuNodes []string) (string, int64) {
	var detectedResource string
	var minCount int64

	for _, nodeName := range gpuNodes {
		node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		for _, resName := range gpuResourceNames {
			if qty, ok := node.Status.Allocatable[resName]; ok {
				count := qty.Value()
				if count > 0 {
					detectedResource = string(resName)
					if minCount == 0 || count < minCount {
						minCount = count
					}
					break
				}
			}
		}
	}
	return detectedResource, minCount
}


func (c *Controller) ensureNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.opts.Namespace,
		},
	}
	_, err := c.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (c *Controller) ensureRBAC(ctx context.Context) error {
	// Split multi-document YAML and apply each resource
	docs := splitYAMLDocuments(deploy.RBACYAML)

	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}

		// Peek at the kind to decide how to unmarshal
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := k8syaml.Unmarshal(doc, &meta); err != nil {
			continue
		}

		switch meta.Kind {
		case "Namespace":
			// Skip — handled by ensureNamespace with the user's --namespace flag
			continue

		case "ServiceAccount":
			var sa corev1.ServiceAccount
			if err := k8syaml.Unmarshal(doc, &sa); err != nil {
				return fmt.Errorf("failed to parse ServiceAccount: %w", err)
			}
			sa.Namespace = c.opts.Namespace
			_, err := c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Create(ctx, &sa, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ServiceAccount: %w", err)
			}

		case "ClusterRole":
			var cr rbacv1.ClusterRole
			if err := k8syaml.Unmarshal(doc, &cr); err != nil {
				return fmt.Errorf("failed to parse ClusterRole: %w", err)
			}
			_, err := c.client.RbacV1().ClusterRoles().Create(ctx, &cr, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRole: %w", err)
			}

		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			if err := k8syaml.Unmarshal(doc, &crb); err != nil {
				return fmt.Errorf("failed to parse ClusterRoleBinding: %w", err)
			}
			// Update the subject namespace to match --namespace
			for i := range crb.Subjects {
				if crb.Subjects[i].Kind == "ServiceAccount" {
					crb.Subjects[i].Namespace = c.opts.Namespace
				}
			}
			_, err := c.client.RbacV1().ClusterRoleBindings().Create(ctx, &crb, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ClusterRoleBinding: %w", err)
			}

		default:
			fmt.Fprintf(c.output, "  Warning: skipping unknown RBAC resource kind %q\n", meta.Kind)
		}
	}

	return nil
}

// splitYAMLDocuments splits a multi-document YAML byte slice on "---" separators.
func splitYAMLDocuments(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range strings.Split(string(data), "\n---") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			docs = append(docs, []byte(trimmed))
		}
	}
	return docs
}

func (c *Controller) deployAgent(ctx context.Context) error {
	var ds appsv1.DaemonSet
	if err := k8syaml.Unmarshal(deploy.DaemonSetYAML, &ds); err != nil {
		return fmt.Errorf("failed to parse embedded daemonset.yaml: %w", err)
	}

	// Override dynamic fields
	ds.Namespace = c.opts.Namespace
	ds.Spec.Template.Spec.Containers[0].Image = c.opts.Image

	// Apply agent resources from platform config
	if len(c.cfg.Agent.Requests) > 0 || len(c.cfg.Agent.Limits) > 0 {
		reqs := corev1.ResourceRequirements{}
		if len(c.cfg.Agent.Requests) > 0 {
			reqs.Requests = make(corev1.ResourceList)
			for k, v := range c.cfg.Agent.Requests {
				qty, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid agent resource %q for %s: %w", v, k, err)
				}
				reqs.Requests[corev1.ResourceName(k)] = qty
			}
		}
		if len(c.cfg.Agent.Limits) > 0 {
			reqs.Limits = make(corev1.ResourceList)
			for k, v := range c.cfg.Agent.Limits {
				qty, err := resource.ParseQuantity(v)
				if err != nil {
					return fmt.Errorf("invalid agent limit %q for %s: %w", v, k, err)
				}
				reqs.Limits[corev1.ResourceName(k)] = qty
			}
		}
		ds.Spec.Template.Spec.Containers[0].Resources = reqs
	}

	// Pass auto-detected vendor so agent knows which checks to run
	if c.gpuVendor != "" {
		container := &ds.Spec.Template.Spec.Containers[0]
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "GPU_VENDOR",
			Value: string(c.gpuVendor),
		})
		checkMode := c.opts.CheckMode
		if checkMode == "" {
			checkMode = "all"
		}
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "CHECK_MODE",
			Value: checkMode,
		})
	}

	// Set node selector: use GPU label if available, otherwise use hostname list
	if c.gpuNodeLabel != "" {
		parts := strings.SplitN(c.gpuNodeLabel, "=", 2)
		if len(parts) == 2 {
			ds.Spec.Template.Spec.NodeSelector = map[string]string{parts[0]: parts[1]}
		}
	} else if len(c.gpuNodes) > 0 {
		// No GPU label — use node affinity to target discovered GPU nodes
		ds.Spec.Template.Spec.NodeSelector = nil
		ds.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpIn,
							Values:   c.gpuNodes,
						}},
					}},
				},
			},
		}
		fmt.Fprintf(c.output, "  Using hostname affinity for %d GPU node(s) (no GPU label found)\n", len(c.gpuNodes))
	}

	_, err := c.client.AppsV1().DaemonSets(c.opts.Namespace).Create(ctx, &ds, metav1.CreateOptions{})
	return err
}

func (c *Controller) waitAndCollect(ctx context.Context, gpuNodes []string) ([]checks.NodeReport, error) {
	timeout := time.After(c.opts.Timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	selector := labels.Set{agentLabelKey: agentLabelValue}.AsSelector().String()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for agents after %v", c.opts.Timeout)
		case <-ticker.C:
			pods, err := c.client.CoreV1().Pods(c.opts.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				continue
			}

			// Check annotation set by the agent when it finishes.
			ready := 0
			for _, pod := range pods.Items {
				status := pod.Annotations[annotationKey]
				if status == annotationDone || status == annotationError {
					ready++
				}
			}

			fmt.Fprintf(c.output, "  Workers ready: %d/%d\n", ready, len(gpuNodes))

			if ready >= len(gpuNodes) {
				return c.collectResults(ctx, pods.Items)
			}
		}
	}
}

func (c *Controller) collectResults(ctx context.Context, pods []corev1.Pod) ([]checks.NodeReport, error) {
	var reports []checks.NodeReport

	for _, pod := range pods {
		report, err := c.collectFromPod(ctx, pod)
		if err != nil {
			fmt.Fprintf(c.output, "  Warning: %v\n", err)
			continue
		}
		reports = append(reports, *report)
	}

	return reports, nil
}

func (c *Controller) collectFromPod(ctx context.Context, pod corev1.Pod) (*checks.NodeReport, error) {
	// Try current logs first (agent sets "done" annotation before exiting),
	// fall back to previous logs if the container has already restarted.
	stream, err := c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		stream, err = c.client.CoreV1().Pods(c.opts.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Previous: true,
		}).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get logs from %s: %w", pod.Name, err)
		}
	}
	defer stream.Close()

	report, err := parseReport(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to parse report from %s: %w", pod.Name, err)
	}
	return report, nil
}

func parseReport(r io.Reader) (*checks.NodeReport, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Skip stderr lines until we find the start of JSON
	var jsonLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			jsonLines = append(jsonLines, line)
			// Collect remaining lines (json.Decoder will stop at the right place)
			for scanner.Scan() {
				jsonLines = append(jsonLines, scanner.Text())
			}
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading logs: %w", err)
	}

	if len(jsonLines) == 0 {
		return nil, fmt.Errorf("no JSON report found in logs")
	}

	// Use json.Decoder to read exactly one JSON object, ignoring trailing stderr lines
	decoder := json.NewDecoder(strings.NewReader(strings.Join(jsonLines, "\n")))
	var report checks.NodeReport
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &report, nil
}

func (c *Controller) runBandwidthJobs(ctx context.Context, gpuNodes []string, reports []checks.NodeReport) ([]jobrunner.JobResult, error) {
	if len(c.jobs) == 0 {
		fmt.Fprintf(c.output, "  No jobs registered, skipping bandwidth tests\n")
		return nil, nil
	}
	if c.gpuVendor == config.GPUVendorAMD {
		fmt.Fprintf(c.output, "  AMD GPU detected, skipping bandwidth jobs (NVIDIA-only images)\n")
		return nil, nil
	}
	if len(gpuNodes) < 2 {
		return nil, fmt.Errorf("need at least 2 GPU nodes for bandwidth tests (have %d)", len(gpuNodes))
	}

	c.configureJobs(ctx, gpuNodes)

	// Build topology map from node reports
	topoMap := buildTopologyMap(reports)
	if len(topoMap) > 0 {
		fmt.Fprintf(c.output, "  Topology available for %d node(s)\n", len(topoMap))
	}

	// Expand RDMA jobs: one per GPU-NIC pair from topology
	jobs, skipResults := c.expandRDMAJobs(ctx, gpuNodes, topoMap, reports)

	runner := jobrunner.New(c.client, c.opts.Namespace, c.opts.Image, c.opts.Timeout, c.output, c.opts.Debug)

	var results []jobrunner.JobResult
	results = append(results, skipResults...)

	// User-specified nodes: star topology (1 server, N clients)
	if c.opts.ServerNode != "" || len(c.opts.ClientNodes) > 0 {
		serverNode, clientNodes := c.resolveStarNodes(gpuNodes)
		jr, err := runner.RunStar(ctx, jobs, serverNode, clientNodes)
		return append(results, jr...), err
	}

	// Default: ring topology (every node tested as both server and client)
	jr, err := runner.RunRing(ctx, jobs, gpuNodes)
	return append(results, jr...), err
}

// buildTopologyMap extracts topology from node reports, keyed by node name.
func buildTopologyMap(reports []checks.NodeReport) map[string]*checks.NodeTopology {
	m := make(map[string]*checks.NodeTopology)
	for _, r := range reports {
		if r.Topology != nil && len(r.Topology.Pairs) > 0 {
			m[r.Node] = r.Topology
		}
	}
	return m
}

// expandRDMAJobs creates per-GPU-NIC RDMA jobs from topology.
// For iperf3 (TCP), topology doesn't matter — keep one job.
// For ib-write-bw, create one job per GPU-NIC pair so every NIC is tested.
func (c *Controller) expandRDMAJobs(ctx context.Context, gpuNodes []string, topoMap map[string]*checks.NodeTopology, reports []checks.NodeReport) ([]jobrunner.Job, []jobrunner.JobResult) {
	if len(topoMap) == 0 {
		return c.jobs, nil
	}

	// Find the first topology (all nodes should have same GPU count)
	var topo *checks.NodeTopology
	for _, t := range topoMap {
		topo = t
		break
	}

	// Check if RDMA resource is configured in jobs.requests or jobs.limits
	rdmaAvailable := false
	for _, resources := range []map[string]string{c.cfg.Jobs.Requests, c.cfg.Jobs.Limits} {
		for k := range resources {
			if strings.Contains(k, "rdma") || strings.Contains(k, "roce") || strings.Contains(k, "hca") {
				rdmaAvailable = true
				break
			}
		}
	}

	var jobs []jobrunner.Job
	for _, job := range c.jobs {
		if job.Name() != "ib-write-bw" {
			// Non-RDMA jobs (iperf3): keep as-is
			jobs = append(jobs, job)
			continue
		}

		if !rdmaAvailable {
			fmt.Fprintf(c.output, "  Skipping RDMA jobs: no RDMA device plugin on nodes\n")
			return jobs, []jobrunner.JobResult{{
				JobName: "ib-write-bw",
				Status:  checks.StatusSkip,
				Message: "RDMA skipped: no RDMA device plugin found on nodes (rdma/* resources not in node allocatable)",
			}}
		}

		// Get original job config
		var origPodCfg *jobrunner.PodConfig
		var origServerImg, origClientImg string
		if orig, ok := job.(*rdma.RDMABandwidthJob); ok {
			origPodCfg = orig.PodCfg
			origServerImg = orig.ServerImage
			origClientImg = orig.ClientImage
		}

		// Check how many RDMA devices are requested
		// If requesting all (>= NIC count), we can specify -d per device
		// If requesting 1, let ib_write_bw auto-detect (can't control which device we get)
		rdmaCount := 0
		for k, v := range c.cfg.Jobs.Requests {
			if strings.Contains(k, "rdma") || strings.Contains(k, "roce") || strings.Contains(k, "hca") {
				fmt.Sscanf(v, "%d", &rdmaCount)
				break
			}
		}
		hasAllDevices := rdmaCount >= len(topo.Pairs)

		// Collect devices and GPU IDs for WEP
		var devices []string
		var gpuIDs []int
		uniqueDevices := make(map[string]bool)

		if hasAllDevices {
			// Requesting all RDMA devices — create one PD job per GPU-NIC pair
			for _, pair := range topo.Pairs {
				rdmaJob := rdma.NewRDMABandwidthJob(c.cfg.Thresholds.RDMABandwidthPD.Pass, nil)
				rdmaJob.PodCfg = origPodCfg
				rdmaJob.ServerImage = origServerImg
				rdmaJob.ClientImage = origClientImg
				rdmaJob.Device = pair.NICDev
				rdmaJob.UseCUDA = pair.GPUID
				jobs = append(jobs, rdmaJob)
				fmt.Fprintf(c.output, "  RDMA PD job: GPU%d ↔ %s (NUMA%d)\n", pair.GPUID, pair.NICDev, pair.NUMAID)

				if !uniqueDevices[pair.NICDev] {
					devices = append(devices, pair.NICDev)
					gpuIDs = append(gpuIDs, pair.GPUID)
					uniqueDevices[pair.NICDev] = true
				}
			}
		} else {
			// Requesting fewer RDMA devices — single PD job, auto-detect device
			rdmaJob := rdma.NewRDMABandwidthJob(c.cfg.Thresholds.RDMABandwidthPD.Pass, nil)
			rdmaJob.PodCfg = origPodCfg
			rdmaJob.ServerImage = origServerImg
			rdmaJob.ClientImage = origClientImg
			// No Device or UseCUDA set — ib_write_bw auto-detects
			jobs = append(jobs, rdmaJob)
			fmt.Fprintf(c.output, "  RDMA PD job: auto-detect device (requesting %d RDMA resource)\n", rdmaCount)
		}

		// Add WEP job if requesting all devices and multiple NICs available
		if hasAllDevices && len(devices) > 1 {
			wepJob := rdma.NewRDMAWEPJob(c.cfg.Thresholds.RDMABandwidthWEP.Pass, devices, gpuIDs)
			wepJob.PodCfg = origPodCfg
			wepJob.ServerImage = origServerImg
			wepJob.ClientImage = origClientImg
			jobs = append(jobs, wepJob)
			fmt.Fprintf(c.output, "  RDMA WEP job: %d NICs in parallel (%s)\n", len(devices), strings.Join(devices, ", "))
		} else {
			fmt.Fprintf(c.output, "  RDMA WEP skipped: only %d NIC(s), need 2+ for whole-endpoint test\n", len(devices))
		}
	}

	return jobs, nil
}

// configureJobs applies GPU resources, thresholds, and images to all registered jobs.
func (c *Controller) configureJobs(ctx context.Context, gpuNodes []string) {
	// Split config: TCP jobs get only cpu/memory, RDMA jobs get everything
	tcpCfg := &jobrunner.PodConfig{
		ResourceRequests: make(map[string]string),
		ResourceLimits:   make(map[string]string),
		Annotations:      make(map[string]string),
	}
	rdmaCfg := &jobrunner.PodConfig{
		ResourceRequests: make(map[string]string),
		ResourceLimits:   make(map[string]string),
		Annotations:      make(map[string]string),
	}

	for k, v := range c.cfg.Jobs.Requests {
		rdmaCfg.ResourceRequests[k] = v
		if k == "cpu" || k == "memory" {
			tcpCfg.ResourceRequests[k] = v
		}
	}
	for k, v := range c.cfg.Jobs.Limits {
		rdmaCfg.ResourceLimits[k] = v
		if k == "cpu" || k == "memory" {
			tcpCfg.ResourceLimits[k] = v
		}
	}
	for k, v := range c.cfg.Jobs.Annotations {
		tcpCfg.Annotations[k] = v
		rdmaCfg.Annotations[k] = v
	}

	for _, job := range c.jobs {
		if configurable, ok := job.(jobrunner.Configurable); ok {
			if strings.HasPrefix(job.Name(), "ib-") {
				configurable.SetPodConfig(rdmaCfg)
			} else {
				configurable.SetPodConfig(tcpCfg)
			}
		}
	}

	// Thresholds from platform config
	for _, job := range c.jobs {
		if tc, ok := job.(jobrunner.ThresholdConfigurable); ok {
			switch job.Name() {
			case "iperf3-tcp":
				tc.SetThreshold(c.cfg.Thresholds.TCPBandwidth.Pass)
			case "ib-write-bw":
				tc.SetThreshold(c.cfg.Thresholds.RDMABandwidthPD.Pass)
			}
		}
	}

	// Container images from image config
	for _, job := range c.jobs {
		if imgConfig, ok := job.(jobrunner.ImageConfigurable); ok {
			configKey := jobConfigKey(job.Name())
			if configKey == "" {
				continue
			}
			jobImage := c.cfg.Images.GetJobImage(configKey)
			if jobImage == "" {
				continue
			}
			if imgConfig.GetServerImage() == "" {
				if setter, ok := job.(interface{ SetServerImage(string) }); ok {
					setter.SetServerImage(jobImage)
				}
			}
			if imgConfig.GetClientImage() == "" {
				if setter, ok := job.(interface{ SetClientImage(string) }); ok {
					setter.SetClientImage(jobImage)
				}
			}
			fmt.Fprintf(c.output, "  Job %s: using image %s\n", job.Name(), jobImage)
		}
	}
}

// jobConfigKey maps job names to image config keys.
func jobConfigKey(jobName string) string {
	if strings.HasPrefix(jobName, "ib-write-bw") {
		return "rdma"
	}
	switch jobName {
	case "iperf3-tcp":
		return "iperf3"
	case "nccl-allreduce":
		return "nccl"
	default:
		return ""
	}
}

// resolveStarNodes returns the server and client nodes for star topology.
func (c *Controller) resolveStarNodes(gpuNodes []string) (string, []string) {
	serverNode := c.opts.ServerNode
	clientNodes := c.opts.ClientNodes
	if serverNode == "" {
		serverNode = gpuNodes[0]
	}
	if len(clientNodes) == 0 {
		for _, n := range gpuNodes {
			if n != serverNode {
				clientNodes = append(clientNodes, n)
			}
		}
	}
	return serverNode, clientNodes
}

// ensureOpenShiftSCC creates a ClusterRoleBinding for the privileged SCC on OpenShift.
func (c *Controller) ensureOpenShiftSCC(ctx context.Context) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rhaii-validator-scc",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "rhaii-validator",
			Namespace: c.opts.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:openshift:scc:privileged",
		},
	}

	_, err := c.client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	fmt.Fprintln(c.output, "  OpenShift: granted privileged SCC to rhaii-validator")
	return nil
}

// cleanupJobs deletes all validation jobs and waits for them to be fully removed.
func (c *Controller) cleanupJobs(ctx context.Context) {
	propagation := metav1.DeletePropagationForeground
	jobs, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=rhaii-validate-job",
	})
	if err != nil || len(jobs.Items) == 0 {
		return
	}

	for _, j := range jobs.Items {
		_ = c.client.BatchV1().Jobs(c.opts.Namespace).Delete(ctx, j.Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
	}
	fmt.Fprintf(c.output, "  Deleting %d leftover job(s)...\n", len(jobs.Items))

	// Wait for jobs to be fully deleted
	for i := 0; i < 30; i++ {
		remaining, err := c.client.BatchV1().Jobs(c.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=rhaii-validate-job",
		})
		if err != nil || len(remaining.Items) == 0 {
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// cleanupDaemonSet removes only the agent DaemonSet, preserving the ConfigMap.
// Used at the start of a run to clean up leftover DaemonSets from previous runs
// without destroying user-customized ConfigMaps.
func (c *Controller) cleanupDaemonSet(ctx context.Context) error {
	err := c.client.AppsV1().DaemonSets(c.opts.Namespace).Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// cleanupAll removes the agent DaemonSet and RBAC resources.
// ConfigMap is preserved so users can edit and rerun without losing customizations.
func (c *Controller) cleanupAll(ctx context.Context) error {
	if err := c.cleanupDaemonSet(ctx); err != nil {
		return err
	}

	for _, del := range []func() error{
		func() error {
			return c.client.CoreV1().ServiceAccounts(c.opts.Namespace).Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoleBindings().Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
		},
		func() error {
			// OpenShift SCC binding (no-op on non-OCP)
			return c.client.RbacV1().ClusterRoleBindings().Delete(ctx, "rhaii-validator-scc", metav1.DeleteOptions{})
		},
		func() error {
			return c.client.RbacV1().ClusterRoles().Delete(ctx, "rhaii-validator", metav1.DeleteOptions{})
		},
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Controller) printReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	fmt.Fprintln(c.output)
	fmt.Fprintln(c.output, "=== Validation Report ===")
	fmt.Fprintf(c.output, "Platform: %s\n", c.platform)

	// Print topology if available
	hasTopology := false
	for _, report := range reports {
		if report.Topology != nil && len(report.Topology.Pairs) > 0 {
			if !hasTopology {
				fmt.Fprintln(c.output)
				fmt.Fprintln(c.output, "GPU-NIC Topology:")
				hasTopology = true
			}
			var pairDescs []string
			for _, p := range report.Topology.Pairs {
				pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA%d)", p.GPUID, p.NICDev, p.NUMAID))
			}
			fmt.Fprintf(c.output, "  %s: %s\n", report.Node, strings.Join(pairDescs, ", "))
		}
	}

	fmt.Fprintln(c.output)

	pass, warn, fail, skip := 0, 0, 0, 0

	fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n", "GROUP", "CHECK", "NODE", "STATUS", "MESSAGE")
	fmt.Fprintln(c.output, strings.Repeat("-", 130))

	for _, report := range reports {
		for _, r := range report.Results {
			node := r.Node
			if node == "" {
				node = "-"
			}
			fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
				r.Category, r.Name, node, r.Status, r.Message)

			if r.Remediation != "" {
				fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s Fix: %s\n",
					"", "", "", "", r.Remediation)
			}

			switch r.Status {
			case checks.StatusPass:
				pass++
			case checks.StatusWarn:
				warn++
			case checks.StatusFail:
				fail++
			case checks.StatusSkip:
				skip++
			}
		}
	}

	// Print job results (bandwidth tests)
	for _, jr := range jobResults {
		node := jr.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(c.output, "%-20s %-30s %-35s %-8s %s\n",
			"bandwidth", jr.JobName, node, jr.Status, jr.Message)

		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}

	fmt.Fprintln(c.output)
	fmt.Fprintf(c.output, "Summary: %d PASS | %d WARN | %d FAIL | %d SKIP\n", pass, warn, fail, skip)

	if fail > 0 {
		fmt.Fprintln(c.output, "Status:  NOT READY - resolve FAIL items before deploying")
	} else if warn > 0 {
		fmt.Fprintln(c.output, "Status:  READY (with warnings)")
	} else {
		fmt.Fprintln(c.output, "Status:  READY")
	}

	fmt.Fprintln(c.output)
	fmt.Fprintln(c.output, "Report:")
	fmt.Fprintf(c.output, "  kubectl get cm %s -n %s -o jsonpath='{.data.report\\.json}' | jq .\n", reportCMName, c.opts.Namespace)
	fmt.Fprintln(c.output)

	return fail > 0
}

func (c *Controller) printJSONReport(reports []checks.NodeReport, jobResults []jobrunner.JobResult) bool {
	type jsonReport struct {
		Platform   string                `json:"platform"`
		Nodes      []checks.NodeReport   `json:"nodes"`
		JobResults []jobrunner.JobResult  `json:"job_results,omitempty"`
		Summary    map[string]int        `json:"summary"`
		Status     string                `json:"status"`
	}

	pass, warn, fail, skip := 0, 0, 0, 0
	for _, report := range reports {
		for _, r := range report.Results {
			switch r.Status {
			case checks.StatusPass:
				pass++
			case checks.StatusWarn:
				warn++
			case checks.StatusFail:
				fail++
			case checks.StatusSkip:
				skip++
			}
		}
	}
	for _, jr := range jobResults {
		switch jr.Status {
		case checks.StatusPass:
			pass++
		case checks.StatusWarn:
			warn++
		case checks.StatusFail:
			fail++
		}
	}

	status := "READY"
	if fail > 0 {
		status = "NOT READY"
	} else if warn > 0 {
		status = "READY (with warnings)"
	}

	r := jsonReport{
		Platform:   string(c.platform),
		Nodes:      reports,
		JobResults: jobResults,
		Summary:    map[string]int{"pass": pass, "warn": warn, "fail": fail, "skip": skip},
		Status:     status,
	}

	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(c.output, string(data))

	return fail > 0
}
