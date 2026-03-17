package config

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DetectPlatform auto-detects the cloud platform from node labels and provider IDs.
func DetectPlatform(ctx context.Context, client kubernetes.Interface) Platform {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return PlatformUnknown
	}

	// Check all nodes — first node might be a master without platform labels.
	// OCP checked first: OpenShift can run on AWS/Azure, so label-based detection
	// must precede provider-ID-based detection to avoid misclassification.
	for _, node := range nodes.Items {
		labels := node.Labels

		// OCP (check first — OCP can run on AWS/Azure)
		if _, ok := labels["node.openshift.io/os_id"]; ok {
			return PlatformOCP
		}

		// CoreWeave
		for key := range labels {
			if strings.Contains(key, "coreweave") {
				return PlatformCoreWeave
			}
		}
	}

	// Cloud providers (by provider ID — only if no platform-specific labels found)
	for _, node := range nodes.Items {
		providerID := node.Spec.ProviderID
		labels := node.Labels

		// AKS
		if strings.HasPrefix(providerID, "azure://") {
			return PlatformAKS
		}
		if _, ok := labels["kubernetes.azure.com/cluster"]; ok {
			return PlatformAKS
		}

		// EKS
		if strings.HasPrefix(providerID, "aws://") {
			return PlatformEKS
		}
		if _, ok := labels["eks.amazonaws.com/nodegroup"]; ok {
			return PlatformEKS
		}
	}

	return PlatformUnknown
}
