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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourcePoolMode controls how GPUs on nodes in the pool are allocated.
// +kubebuilder:validation:Enum=Exclusive;Shared
type ResourcePoolMode string

const (
	// PoolModeExclusive allocates whole GPUs exclusively to workloads.
	PoolModeExclusive ResourcePoolMode = "Exclusive"
	// PoolModeShared multiplexes GPUs across multiple interactive sandboxes.
	PoolModeShared ResourcePoolMode = "Shared"
)

// WorkloadKind identifies a category of AI workload permitted on a ResourcePool.
// +kubebuilder:validation:Enum=Sandbox;TrainJob;Inference
type WorkloadKind string

const (
	WorkloadSandbox   WorkloadKind = "Sandbox"
	WorkloadTrainJob  WorkloadKind = "TrainJob"
	WorkloadInference WorkloadKind = "Inference"
)

// ResourcePoolVisibility controls which Projects may bind to a ResourcePool or InstanceType.
// +kubebuilder:validation:Enum=Public;Restricted
type ResourcePoolVisibility string

const (
	// VisibilityPublic allows any Project with quota to use the resource.
	VisibilityPublic ResourcePoolVisibility = "Public"
	// VisibilityRestricted limits usage to Projects listed in allowedProjects.
	VisibilityRestricted ResourcePoolVisibility = "Restricted"
)

// ResourcePoolSharing configures GPU multiplexing for a Shared ResourcePool.
type ResourcePoolSharing struct {
	// maxInstancesPerNode limits how many shared interactive sandboxes may run
	// concurrently on a single node in this pool (e.g. 16 per 8-GPU node).
	// +optional
	// +kubebuilder:default=16
	// +kubebuilder:validation:Minimum=1
	MaxInstancesPerNode int32 `json:"maxInstancesPerNode,omitempty"`
}

// ResourcePoolSpotPolicy configures whether idle capacity in an Exclusive pool
// can be borrowed by preemptible Spot workloads.
type ResourcePoolSpotPolicy struct {
	// lendIdleToSpot allows idle GPUs in this pool to be borrowed by Spot queues
	// and reclaimed immediately when guaranteed workloads arrive.
	// +optional
	LendIdleToSpot bool `json:"lendIdleToSpot,omitempty"`
}

// ResourcePoolAccessControl limits which Projects can see and bind to a ResourcePool.
type ResourcePoolAccessControl struct {
	// visibility defaults to Public.
	// +optional
	// +kubebuilder:default=Public
	Visibility ResourcePoolVisibility `json:"visibility,omitempty"`
	// allowedProjects lists Project names allowed to use this pool when visibility is Restricted.
	// +optional
	AllowedProjects []string `json:"allowedProjects,omitempty"`
}

// IdleShutdownRule defines pool-level automatic shutdown policy for idle interactive sandboxes.
// Only supported on Exclusive pools.
type IdleShutdownRule struct {
	// enabled turns on idle shutdown for all sandboxes running in this pool.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// idleDuration is how long a sandbox must remain idle before countdown begins.
	// +optional
	IdleDuration *metav1.Duration `json:"idleDuration,omitempty"`
	// countdownDuration is the grace period before shutdown (0m, 15m, 30m, 45m or 60m).
	// +optional
	CountdownDuration *metav1.Duration `json:"countdownDuration,omitempty"`
}

// ResourcePoolRules holds automated governance policies attached to a ResourcePool.
type ResourcePoolRules struct {
	// idleShutdown configures automatic release of idle sandboxes in Exclusive pools.
	// +optional
	IdleShutdown *IdleShutdownRule `json:"idleShutdown,omitempty"`
}

// ResourcePoolSpec defines the desired state of ResourcePool.
// +kubebuilder:validation:XValidation:rule="self.mode != 'Shared' || !has(self.rules) || !has(self.rules.idleShutdown) || !self.rules.idleShutdown.enabled",message="Shared resource pools do not support idleShutdown rules"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Shared' || !has(self.spotPolicy) || !self.spotPolicy.lendIdleToSpot",message="Shared resource pools cannot lend capacity to Spot"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Shared' || !has(self.allowedWorkloads) || self.allowedWorkloads.all(w, w == 'Sandbox')",message="Shared resource pools only allow Sandbox workloads"
type ResourcePoolSpec struct {
	// displayName is shown in the UI.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// mode selects Exclusive (whole GPU) or Shared (multiplexed dev sandboxes). Defaults to Exclusive.
	// +optional
	// +kubebuilder:default=Exclusive
	Mode ResourcePoolMode `json:"mode,omitempty"`
	// zone restricts membership to nodes in this availability zone (topology.kubernetes.io/zone).
	// +optional
	Zone string `json:"zone,omitempty"`
	// nodes explicitly assigns nodes to this pool. Nodes not assigned to any
	// named pool belong to the built-in default pool.
	// +optional
	Nodes []string `json:"nodes,omitempty"`
	// nodeSelector dynamically selects nodes matching these labels for this pool
	// when not explicitly assigned to another pool.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// allowedWorkloads restricts which workload categories may run in this pool.
	// Defaults to all kinds for Exclusive pools and [Sandbox] for Shared pools.
	// +optional
	AllowedWorkloads []WorkloadKind `json:"allowedWorkloads,omitempty"`
	// sharing configures GPU multiplexing limits when mode is Shared.
	// +optional
	Sharing *ResourcePoolSharing `json:"sharing,omitempty"`
	// spotPolicy configures idle lending to Spot queues when mode is Exclusive.
	// +optional
	SpotPolicy *ResourcePoolSpotPolicy `json:"spotPolicy,omitempty"`
	// accessControl controls which Projects can discover and bind to this pool.
	// +optional
	AccessControl ResourcePoolAccessControl `json:"accessControl,omitempty"`
	// rules configures pool-level operational policies such as idle shutdown.
	// +optional
	Rules *ResourcePoolRules `json:"rules,omitempty"`
}

// ResourcePoolGPUStatus summarizes GPU capacity of one class in a ResourcePool.
type ResourcePoolGPUStatus struct {
	// class is the normalized GPU class, e.g. h100-80gb or rtx-5090.
	Class string `json:"class"`
	// product is the nvidia.com/gpu.product label value.
	Product string `json:"product"`
	// total GPUs across schedulable nodes in the pool.
	Total int32 `json:"total"`
	// allocated GPUs currently requested by running pods.
	Allocated int32 `json:"allocated"`
	// free GPUs available on schedulable nodes.
	Free int32 `json:"free"`
}

// LaunchableInstanceType reports how many instances of a given InstanceType can
// be scheduled immediately in this pool, accounting for per-node fragmentation.
type LaunchableInstanceType struct {
	// instanceType is the InstanceType name, e.g. rtx-5090.x8 or rtx-5090.shared.
	InstanceType string `json:"instanceType"`
	// count is the number of instances that fit right now.
	Count int32 `json:"count"`
}

// NodePoolState describes a node's membership state in a ResourcePool.
// +kubebuilder:validation:Enum=Ready;TransferBlocked;ZoneMismatch;NotFound;Conflict
type NodePoolState string

const (
	NodePoolReady           NodePoolState = "Ready"
	NodePoolTransferBlocked NodePoolState = "TransferBlocked"
	NodePoolZoneMismatch    NodePoolState = "ZoneMismatch"
	NodePoolNotFound        NodePoolState = "NotFound"
	NodePoolConflict        NodePoolState = "Conflict"
)

// ResourcePoolNodeStatus reports per-node capacity and transfer state.
type ResourcePoolNodeStatus struct {
	Node          string        `json:"node"`
	Zone          string        `json:"zone,omitempty"`
	Ready         bool          `json:"ready"`
	TotalGPUs     int32         `json:"totalGPUs,omitempty"`
	AllocatedGPUs int32         `json:"allocatedGPUs,omitempty"`
	FreeGPUs      int32         `json:"freeGPUs,omitempty"`
	UsedSlots     int32         `json:"usedSlots,omitempty"`
	MaxSlots      int32         `json:"maxSlots,omitempty"`
	State         NodePoolState `json:"state"`
	Message       string        `json:"message,omitempty"`
}

// Condition types of a ResourcePool.
const (
	ConditionPoolReady           = "Ready"
	ConditionNodeTransferBlocked = "NodeTransferBlocked"
)

// ResourcePoolStatus defines the observed state of ResourcePool.
type ResourcePoolStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// totalNodes is the number of nodes assigned to this pool.
	// +optional
	TotalNodes int32 `json:"totalNodes,omitempty"`
	// readyNodes is the number of schedulable, Ready nodes in this pool.
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`
	// gpus summarizes capacity per GPU class in this pool.
	// +optional
	GPUs []ResourcePoolGPUStatus `json:"gpus,omitempty"`
	// launchable reports fragmentation-aware launchable counts per InstanceType.
	// +optional
	Launchable []LaunchableInstanceType `json:"launchable,omitempty"`
	// nodes reports node-level availability and transfer guard status.
	// +optional
	Nodes []ResourcePoolNodeStatus `json:"nodes,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=frp,categories=project
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.totalNodes`
// +kubebuilder:printcolumn:name="ReadyNodes",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 40",message="resource pool name must be a DNS label of at most 40 characters"

// ResourcePool partitions cluster compute nodes into isolated Exclusive or
// Shared pools with Kueue ResourceFlavor integration, idle-node transfer guards,
// and pool-level operational policies.
type ResourcePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ResourcePool
	// +required
	Spec ResourcePoolSpec `json:"spec"`

	// status defines the observed state of ResourcePool
	// +optional
	Status ResourcePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ResourcePoolList contains a list of ResourcePool
type ResourcePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ResourcePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ResourcePool{}, &ResourcePoolList{})
		return nil
	})
}
