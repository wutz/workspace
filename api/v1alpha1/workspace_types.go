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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GPUQuota is the number of GPUs of one class a project may run at once.
type GPUQuota struct {
	// class is the InstanceType GPU class, e.g. rtx-5090.
	// +required
	// +kubebuilder:validation:MinLength=1
	Class string `json:"class"`
	// +required
	// +kubebuilder:validation:Minimum=0
	Count int32 `json:"count"`
}

// StorageClassQuota limits storage capacity and PVC count for one StorageClass.
type StorageClassQuota struct {
	// storageClassName is the Kubernetes StorageClass name.
	// +required
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`
	// storage is the total requested capacity for PVCs using this StorageClass.
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
	// volumes is the maximum number of PVCs using this StorageClass.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Volumes *int32 `json:"volumes,omitempty"`
}

// ProjectQuota limits what a project may run and store. Unset fields take the
// platform defaults.
type ProjectQuota struct {
	// gpus is the GPU quota per GPU class in the default ResourcePool when
	// poolQuotas is unset. CPU and memory quotas follow from the per-GPU slice
	// of each class. Jobs over quota wait in the queue.
	// +optional
	// +listType=map
	// +listMapKey=class
	GPUs []GPUQuota `json:"gpus,omitempty"`
	// storage is the total requested size of all volumes.
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
	// volumes is the maximum number of volumes (PVCs).
	// +optional
	Volumes *int32 `json:"volumes,omitempty"`
	// pods is the maximum number of pods.
	// +optional
	Pods *int32 `json:"pods,omitempty"`
	// registryStorage is the Harbor project storage quota; unset uses platform default (unlimited if unset there).
	// +optional
	RegistryStorage *resource.Quantity `json:"registryStorage,omitempty"`
	// storageClasses limits capacity and PVC count per StorageClass.
	// +optional
	// +listType=map
	// +listMapKey=storageClassName
	StorageClasses []StorageClassQuota `json:"storageClasses,omitempty"`
	// extendedResources sets explicit quotas on extra hardware resources (e.g. rdma/roce_gids).
	// +optional
	ExtendedResources corev1.ResourceList `json:"extendedResources,omitempty"`
}

// PoolQuota binds a Project to a specific ResourcePool with pool-scoped limits.
type PoolQuota struct {
	// pool is the ResourcePool name (e.g. default, llm-exclusive, dev-shared).
	// +required
	// +kubebuilder:validation:MinLength=1
	Pool string `json:"pool"`
	// gpus is the GPU quota per GPU class for an Exclusive pool.
	// +optional
	// +listType=map
	// +listMapKey=class
	GPUs []GPUQuota `json:"gpus,omitempty"`
	// maxSandboxes is the maximum concurrent shared sandboxes in a Shared pool.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxSandboxes *int32 `json:"maxSandboxes,omitempty"`
	// borrowingLimit limits how many extra GPUs this Project may borrow from the pool's Cohort.
	// +optional
	// +kubebuilder:validation:Minimum=0
	BorrowingLimit *int32 `json:"borrowingLimit,omitempty"`
	// lendingLimit limits how many unused nominal GPUs this Project may lend within the pool's Cohort.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LendingLimit *int32 `json:"lendingLimit,omitempty"`
}

// SpotConfig enables preemptible Spot workloads that borrow idle capacity from
// Exclusive ResourcePools with spotPolicy.lendIdleToSpot enabled.
// +kubebuilder:validation:XValidation:rule="!has(self.maxQueueDuration) || (duration(self.maxQueueDuration) >= duration('1h') && duration(self.maxQueueDuration) <= duration('12h'))",message="spot.maxQueueDuration must be between 1h and 12h"
type SpotConfig struct {
	// enabled allows members to submit workloads to the project's spot LocalQueue.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// gpus sets the maximum concurrent Spot GPUs per GPU class.
	// +optional
	// +listType=map
	// +listMapKey=class
	GPUs []GPUQuota `json:"gpus,omitempty"`
	// maxQueueDuration is the maximum waiting duration for a single Spot queueing attempt (1h to 12h).
	// +optional
	MaxQueueDuration *metav1.Duration `json:"maxQueueDuration,omitempty"`
	// maxRequeueCount limits how many times a preempted or starved Spot workload may re-enter the queue (0 to 3).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	MaxRequeueCount *int32 `json:"maxRequeueCount,omitempty"`
}

// SharedVolumeSpec configures the project-scoped RWX shared PersistentVolumeClaim.
type SharedVolumeSpec struct {
	// enabled overrides the platform default for provisioning the workspace-shared PVC.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// size overrides the platform default shared volume size.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// storageClassName overrides the platform default RWX StorageClass.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// PublicDatasetRef projects a platform-defined read-only dataset or base model
// volume descriptor into the project namespace.
type PublicDatasetRef struct {
	// name is the platform dataset name defined in workspace-platform config.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// mountPath is an optional preferred mount path hint for workloads.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// NetworkIsolation selects the network policy of a project namespace.
// +kubebuilder:validation:Enum=Isolated;None
type NetworkIsolation string

const (
	// NetworkIsolated only admits traffic from the same project and platform namespaces.
	NetworkIsolated NetworkIsolation = "Isolated"
	// NetworkNone applies no policy.
	NetworkNone NetworkIsolation = "None"
)

// ProjectNetwork configures network isolation.
type ProjectNetwork struct {
	// isolation defaults to Isolated.
	// +optional
	// +kubebuilder:default=Isolated
	Isolation NetworkIsolation `json:"isolation,omitempty"`
	// denyInternet blocks egress outside the project (DNS and platform internal allowlists stay allowed).
	// +optional
	DenyInternet bool `json:"denyInternet,omitempty"`
}

// MemberRole is the access level of a project member.
// +kubebuilder:validation:Enum=admin;edit;view
type MemberRole string

const (
	// RoleAdmin manages workloads, shared configs and project membership metadata
	// without modifying quotas or network isolation.
	RoleAdmin MemberRole = "admin"
	// RoleEdit manages jobs, templates, volumes, files and images.
	RoleEdit MemberRole = "edit"
	// RoleView is read-only and cannot read Secrets.
	RoleView MemberRole = "view"
)

// MemberKind identifies the RBAC subject kind of a project member.
// +kubebuilder:validation:Enum=ServiceAccount;User;Group
type MemberKind string

const (
	// MemberServiceAccount creates a managed ServiceAccount and token Secret in the members namespace.
	MemberServiceAccount MemberKind = "ServiceAccount"
	// MemberUser binds an external OIDC/SSO user directly via RBAC without issuing a static SA token.
	MemberUser MemberKind = "User"
	// MemberGroup binds an external OIDC/SSO group directly via RBAC without issuing a static SA token.
	MemberGroup MemberKind = "Group"
)

// Member is a user or group of the project. ServiceAccount members get a
// ServiceAccount in the platform members namespace and a token that the admin
// exports as a kubeconfig. User and Group members bind directly to OIDC subjects.
// +kubebuilder:validation:XValidation:rule="!has(self.kind) || self.kind != 'ServiceAccount' || size(self.name) <= 40",message="ServiceAccount member name must be at most 40 characters"
type Member struct {
	// name identifies the member within the project (or the OIDC User/Group subject name).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// kind selects ServiceAccount (default), User, or Group.
	// +optional
	// +kubebuilder:default=ServiceAccount
	Kind MemberKind `json:"kind,omitempty"`
	// +optional
	// +kubebuilder:default=edit
	Role MemberRole `json:"role,omitempty"`
}

// DeletionPolicy decides what happens to project data when the Project is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionRetain revokes access but keeps the namespace and registry project.
	DeletionRetain DeletionPolicy = "Retain"
	// DeletionDelete removes the namespace (with all volumes) and registry project.
	DeletionDelete DeletionPolicy = "Delete"
)

// ProjectSpec defines the desired state of Project.
type ProjectSpec struct {
	// displayName is shown in the GUI.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// +optional
	Quota ProjectQuota `json:"quota,omitempty"`
	// poolQuotas binds this project to one or more ResourcePools with pool-scoped quotas.
	// When empty, quota.gpus applies to the built-in default pool for backward compatibility.
	// +optional
	// +listType=map
	// +listMapKey=pool
	PoolQuotas []PoolQuota `json:"poolQuotas,omitempty"`
	// spot enables preemptible Spot workloads borrowing idle capacity from lending Exclusive pools.
	// +optional
	Spot *SpotConfig `json:"spot,omitempty"`
	// sharedVolume configures the default RWX shared PersistentVolumeClaim in the project namespace.
	// +optional
	SharedVolume *SharedVolumeSpec `json:"sharedVolume,omitempty"`
	// publicDatasets lists platform-defined read-only datasets projected into the project namespace.
	// +optional
	// +listType=map
	// +listMapKey=name
	PublicDatasets []PublicDatasetRef `json:"publicDatasets,omitempty"`
	// +optional
	Network ProjectNetwork `json:"network,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Members []Member `json:"members,omitempty"`
	// trashRetention is how long deleted volumes are kept before they are
	// purged, unless a volume sets its own expiry. Defaults to the platform setting.
	// +optional
	TrashRetention *metav1.Duration `json:"trashRetention,omitempty"`
	// deletionPolicy defaults to Retain.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// MemberStatus reports the credentials or subject binding of a member.
type MemberStatus struct {
	Name string `json:"name"`
	// kind is ServiceAccount, User or Group.
	// +optional
	Kind MemberKind `json:"kind,omitempty"`
	// namespace of the ServiceAccount (the platform members namespace; empty for User/Group).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// serviceAccount of the member (empty for User/Group).
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// tokenSecret holds the long-lived token (kubernetes.io/service-account-token; empty for User/Group).
	// +optional
	TokenSecret string `json:"tokenSecret,omitempty"`
}

// QueueStatus reports one Kueue LocalQueue available inside the project namespace.
type QueueStatus struct {
	// name is the LocalQueue name inside the project namespace.
	Name string `json:"name"`
	// pool is the backing ResourcePool name (empty for cross-pool Spot queue).
	// +optional
	Pool string `json:"pool,omitempty"`
	// mode is Exclusive or Shared.
	// +optional
	Mode ResourcePoolMode `json:"mode,omitempty"`
	// clusterQueue is the backing Kueue ClusterQueue name.
	ClusterQueue string `json:"clusterQueue"`
	// spot marks a preemptible Spot queue.
	// +optional
	Spot bool `json:"spot,omitempty"`
	// allowedWorkloads lists permitted workload kinds on this queue.
	// +optional
	AllowedWorkloads []WorkloadKind `json:"allowedWorkloads,omitempty"`
}

// RegistryStatus reports the Harbor project of a project.
type RegistryStatus struct {
	// project is the Harbor project name.
	Project string `json:"project,omitempty"`
	// url is the push prefix, e.g. cr.example.com:7443/project-foo.
	URL string `json:"url,omitempty"`
	// secret in the project namespace holds the robot account (dockerconfigjson).
	Secret string `json:"secret,omitempty"`
}

// ProjectStatus defines the observed state of Project.
type ProjectStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// namespace backing the project.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// clusterQueue is the default Kueue ClusterQueue enforcing the GPU quota.
	// +optional
	ClusterQueue string `json:"clusterQueue,omitempty"`
	// localQueue is the default queue jobs in the namespace submit to.
	// +optional
	LocalQueue string `json:"localQueue,omitempty"`
	// queues lists all Kueue LocalQueues provisioned for this project (per pool and spot).
	// +optional
	Queues []QueueStatus `json:"queues,omitempty"`
	// sharedVolume is the name of the provisioned RWX shared PVC, if enabled.
	// +optional
	SharedVolume string `json:"sharedVolume,omitempty"`
	// datasets lists the public dataset descriptors projected into this project.
	// +optional
	Datasets []string `json:"datasets,omitempty"`
	// +optional
	Registry RegistryStatus `json:"registry,omitzero"`
	// +optional
	Members []MemberStatus `json:"members,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types of a Project.
const (
	ConditionReady    = "Ready"
	ConditionRegistry = "RegistryReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fws,categories=project
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.spec.network.isolation`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Registry",type=string,JSONPath=`.status.conditions[?(@.type=="RegistryReady")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 40",message="project name must be a DNS label of at most 40 characters"

// Project is a tenant: a labelled namespace with quota, network isolation,
// a Kueue queue, a Harbor project and member credentials.
type Project struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Project
	// +required
	Spec ProjectSpec `json:"spec"`

	// status defines the observed state of Project
	// +optional
	Status ProjectStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Project{}, &ProjectList{})
		return nil
	})
}
