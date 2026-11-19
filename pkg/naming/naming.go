// Package naming holds object names, label keys and annotations set by the
// operator. Other platforms that read them import this package.
package naming

import (
	"regexp"
	"strings"
)

// Labels set by the operator.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is the LabelManagedBy value of every object the operator creates.
	ManagedBy = "workspace"
	// LabelProject is set on project namespaces and every object the operator
	// creates for a project (value = Project name).
	LabelProject = "workspace.wutz.dev/project"
	// LabelMember is set on member ServiceAccounts and token Secrets.
	LabelMember = "workspace.wutz.dev/member"
	// LabelGPUClass is set on generated InstanceTypes and ResourceFlavors.
	LabelGPUClass = "workspace.wutz.dev/gpu-class"
	// LabelPool is set on Nodes, ResourceFlavors, LocalQueues and ClusterQueues
	// to anchor physical compute pool membership.
	LabelPool = "workspace.wutz.dev/pool"
	// LabelPoolMode is set on Nodes and queues (exclusive or shared).
	LabelPoolMode = "workspace.wutz.dev/pool-mode"
	// PoolModeExclusive and PoolModeShared are the values of LabelPoolMode.
	PoolModeExclusive = "exclusive"
	PoolModeShared    = "shared"
	// LabelDefaultPool marks the built-in default ResourcePool.
	LabelDefaultPool = "workspace.wutz.dev/default-pool"
	// LabelSharedVolume marks the auto-provisioned RWX shared PVC in a project namespace.
	LabelSharedVolume = "workspace.wutz.dev/shared-volume"
	// LabelPublicDataset marks projected public dataset descriptors in a project namespace.
	LabelPublicDataset = "workspace.wutz.dev/public-dataset"
	// LabelTopologyZone is the standard Kubernetes availability zone label.
	LabelTopologyZone = "topology.kubernetes.io/zone"
	// Node labels from NVIDIA GPU feature discovery.
	LabelGPUProduct = "nvidia.com/gpu.product"
	LabelGPUMemory  = "nvidia.com/gpu.memory"
)

// Taints, extended resources and annotations managed by the operator.
const (
	// TaintSharedPool prevents Exclusive workloads from landing on Shared pool nodes.
	TaintSharedPool = "workspace.wutz.dev/shared-pool"
	// ResourceSharedSlot is the logical slot resource used to limit shared sandboxes per node/queue.
	ResourceSharedSlot = "workspace.wutz.dev/shared-slot"

	// AnnotationTrashRetention propagates Project.Spec.TrashRetention to namespaces and PVCs.
	AnnotationTrashRetention = "workspace.wutz.dev/trash-retention"
	// AnnotationSpotMaxQueueDuration records Spot single-attempt queue timeout on the spot LocalQueue.
	AnnotationSpotMaxQueueDuration = "workspace.wutz.dev/spot-max-queue-duration"
	// AnnotationSpotMaxRequeueCount records Spot max requeue attempts on the spot LocalQueue.
	AnnotationSpotMaxRequeueCount = "workspace.wutz.dev/spot-max-requeue-count"
	// AnnotationAllowedWorkloads records comma-separated allowed workload kinds on a LocalQueue.
	AnnotationAllowedWorkloads = "workspace.wutz.dev/allowed-workloads"
	// AnnotationIdleShutdownEnabled mirrors pool idle-shutdown policy to queues/namespaces.
	AnnotationIdleShutdownEnabled = "workspace.wutz.dev/idle-shutdown-enabled"
)

// Fixed names inside a project namespace or cluster.
const (
	DefaultPoolName    = "default"
	LocalQueueName     = "default"
	SpotQueueName      = "spot"
	RegistrySecretName = "workspace-registry"
	NetworkPolicyName  = "workspace-isolation"
	ResourceQuotaName  = "workspace-quota"
	LimitRangeName     = "workspace-defaults"
	SharedVolumeName   = "workspace-shared"
)

// ClusterRolePrefix + role (admin, edit, view) is bound per member in the project
// namespace (see config/rbac/workspace_roles.yaml).
const ClusterRolePrefix = "workspace-"

// Namespace is the namespace of a project.
func Namespace(prefix, project string) string { return prefix + project }

// ClusterQueue is the default Kueue ClusterQueue of a project.
func ClusterQueue(namespace string) string { return namespace }

// PoolClusterQueue is the Kueue ClusterQueue of a project for one ResourcePool.
func PoolClusterQueue(namespace, pool string) string {
	if pool == "" || pool == DefaultPoolName {
		return namespace
	}
	return namespace + "-" + pool
}

// SpotClusterQueue is the preemptible Spot ClusterQueue of a project.
func SpotClusterQueue(namespace string) string { return namespace + "-" + SpotQueueName }

// PoolCohort is the Kueue Cohort shared by ClusterQueues using one ResourcePool.
func PoolCohort(pool string) string { return "cohort-pool-" + pool }

// SpotCohort is the Kueue Cohort through which Spot queues borrow idle GPUs
// from lending Exclusive ResourcePools.
const SpotCohort = "cohort-workspace-spot"

// ProjectClusterRole is the per-project ClusterRole (and binding) granting
// members read access to the Project, its ClusterQueue, authorized ResourcePools
// and cluster-wide catalogues.
func ProjectClusterRole(project string) string { return "project:project:" + project }

// MemberServiceAccount is the ServiceAccount of a member in the members
// namespace. Project and member names are DNS labels, so the dot keeps
// names unique.
func MemberServiceAccount(project, member string) string { return project + "." + member }

// MemberTokenSecret is the long-lived token Secret of a member.
func MemberTokenSecret(project, member string) string { return project + "." + member + ".token" }

// MemberRoleBinding binds a member to its role in the project namespace.
// Non-alphanumeric characters (such as '@' in OIDC emails) are sanitized.
func MemberRoleBinding(member string) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(member), "-"), "-")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "-")
	}
	return "project-member-" + s
}

// PublicDatasetConfigMap is the ConfigMap name projecting a platform read-only dataset.
func PublicDatasetConfigMap(dataset string) string { return "dataset-" + dataset }

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// GPUClass normalises a nvidia.com/gpu.product value (NVIDIA-GeForce-RTX-5090)
// to a short DNS label (rtx-5090).
func GPUClass(product string) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(product), "-"), "-")
	for _, p := range []string{"nvidia-", "geforce-", "tesla-"} {
		s = strings.TrimPrefix(s, p)
	}
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// InstanceTypeName is the InstanceType of count GPUs of a class, e.g. rtx-5090.x2.
func InstanceTypeName(class string, count int32) string {
	return class + ".x" + itoa(count)
}

// SharedInstanceTypeName is the InstanceType derived for a GPU class in a Shared pool.
func SharedInstanceTypeName(class string) string {
	return class + ".shared"
}

// CPUInstanceTypeName is a CPU-only InstanceType name, e.g. cpu.x4.
func CPUInstanceTypeName(cores int32) string {
	return "cpu.x" + itoa(cores)
}

// ResourceFlavor is the legacy/default Kueue ResourceFlavor of a GPU class.
func ResourceFlavor(class string) string { return "project-" + class }

// PoolResourceFlavor is the pool-scoped Kueue ResourceFlavor for a GPU class.
func PoolResourceFlavor(pool, class string) string { return "pool-" + pool + "-" + class }

// PoolSharedFlavor is the Kueue ResourceFlavor for a Shared ResourcePool.
func PoolSharedFlavor(pool string) string { return "pool-" + pool + "-shared" }

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var b [11]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Keys of unstructured object maps.
const (
	KeyAPIVersion = "apiVersion"
	KeyKind       = "kind"
	KeyMetadata   = "metadata"
	KeyName       = "name"
	KeyNamespace  = "namespace"
	KeyLabels     = "labels"
	KeySpec       = "spec"
	KeyStatus     = "status"
)
