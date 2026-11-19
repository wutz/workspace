// Package kf names the Kubeflow and Kueue APIs the Project controller uses.
// Kueue objects are exchanged as unstructured, so only their GVKs are modelled.
package kf

import "k8s.io/apimachinery/pkg/runtime/schema"

// Group of the Kubeflow Trainer v2 API, which project members may read.
const Group = "trainer.kubeflow.org"

var (
	KueueGroupVersion        = schema.GroupVersion{Group: "kueue.x-k8s.io", Version: "v1beta2"}
	LocalQueueGVK            = KueueGroupVersion.WithKind("LocalQueue")
	ClusterQueueGVK          = KueueGroupVersion.WithKind("ClusterQueue")
	ResourceFlavorGVK        = KueueGroupVersion.WithKind("ResourceFlavor")
	WorkloadPriorityClassGVK = KueueGroupVersion.WithKind("WorkloadPriorityClass")
)

// PriorityClass defines a standard Kueue WorkloadPriorityClass provisioned by the operator.
type PriorityClass struct {
	Name        string
	Value       int32
	Description string
}

// StandardPriorityClasses lists the four workload priority tiers aligned with
// online inference, interactive dev sandboxes, batch training and preemptible spot.
var StandardPriorityClasses = []PriorityClass{
	{Name: "workspace-inference", Value: 1000, Description: "Non-preemptible online inference services"},
	{Name: "workspace-interactive", Value: 750, Description: "Interactive development sandboxes"},
	{Name: "workspace-training", Value: 500, Description: "Guaranteed training and batch jobs"},
	{Name: "workspace-spot", Value: 100, Description: "Preemptible spot workloads borrowing idle capacity"},
}
