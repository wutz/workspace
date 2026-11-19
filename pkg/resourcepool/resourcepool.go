// Package resourcepool computes physical node pool membership, idle-node
// transfer guards, per-pool GPU summaries, and fragmentation-aware launchable
// instance counts. It is a pure function of ResourcePools, Nodes, Pods and
// InstanceTypes.
package resourcepool

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/instancetype"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
)

// DefaultMaxSharedInstancesPerNode matches NeoGPU's 16 shared dev sandboxes per 8-GPU node.
const DefaultMaxSharedInstancesPerNode int32 = 16

// NodeAssignment describes the reconciled pool labels and taint for one Node.
type NodeAssignment struct {
	NodeName        string
	Pool            string
	Mode            v1.ResourcePoolMode
	WantSharedTaint bool
}

// PoolFlavor describes one Kueue ResourceFlavor required by a ResourcePool.
type PoolFlavor struct {
	Name        string
	Pool        string
	Mode        v1.ResourcePoolMode
	GPUClass    string
	GPUProduct  string
	SharedTaint bool
}

// PoolResult is the computed status and Kueue flavors of one ResourcePool.
type PoolResult struct {
	Pool            string
	Mode            v1.ResourcePoolMode
	Status          v1.ResourcePoolStatus
	TransferBlocked bool
	BlockedMessage  string
	Flavors         []PoolFlavor
}

// Plan holds the reconciled node assignments and per-pool statuses across the cluster.
type Plan struct {
	Nodes map[string]NodeAssignment
	Pools map[string]PoolResult
}

// ProjectAllowed reports whether project may bind to and view pool.
func ProjectAllowed(pool *v1.ResourcePool, project string) bool {
	if pool == nil {
		return false
	}
	if pool.Spec.AccessControl.Visibility != v1.VisibilityRestricted {
		return true
	}
	return slices.Contains(pool.Spec.AccessControl.AllowedProjects, project)
}

// EffectiveMode returns the pool's mode, defaulting to Exclusive.
func EffectiveMode(pool *v1.ResourcePool) v1.ResourcePoolMode {
	if pool != nil && pool.Spec.Mode == v1.PoolModeShared {
		return v1.PoolModeShared
	}
	return v1.PoolModeExclusive
}

// EffectiveAllowedWorkloads returns the permitted workload kinds for pool.
func EffectiveAllowedWorkloads(pool *v1.ResourcePool) []v1.WorkloadKind {
	if EffectiveMode(pool) == v1.PoolModeShared {
		return []v1.WorkloadKind{v1.WorkloadSandbox}
	}
	if pool != nil && len(pool.Spec.AllowedWorkloads) > 0 {
		return slices.Clone(pool.Spec.AllowedWorkloads)
	}
	return []v1.WorkloadKind{v1.WorkloadSandbox, v1.WorkloadTrainJob, v1.WorkloadInference}
}

// MaxSharedSlots returns the per-node shared sandbox limit for a Shared pool.
func MaxSharedSlots(pool *v1.ResourcePool) int32 {
	if pool != nil && pool.Spec.Sharing != nil && pool.Spec.Sharing.MaxInstancesPerNode > 0 {
		return pool.Spec.Sharing.MaxInstancesPerNode
	}
	return DefaultMaxSharedInstancesPerNode
}

// ModeLabelValue returns the lowercase label value for workspace.wutz.dev/pool-mode.
func ModeLabelValue(mode v1.ResourcePoolMode) string {
	if mode == v1.PoolModeShared {
		return "shared"
	}
	return "exclusive"
}

type nodeUsage struct {
	usedGPU      int64
	usedCPU      int64
	usedMem      int64
	otherCPU     int64
	otherMem     int64
	usedSlots    int32
	blockingPods []string
}

func computeNodeUsage(pods []corev1.Pod, managedNamespaces map[string]bool) map[string]*nodeUsage {
	out := map[string]*nodeUsage{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		u := out[p.Spec.NodeName]
		if u == nil {
			u = &nodeUsage{}
			out[p.Spec.NodeName] = u
		}
		req := instancetype.PodRequests(p)
		gpus := quantity(req, instancetype.ResourceGPU)
		if gpus > 0 {
			u.usedGPU += gpus
			u.usedCPU += req.Cpu().MilliValue()
			u.usedMem += req.Memory().Value()
		} else {
			u.otherCPU += req.Cpu().MilliValue()
			u.otherMem += req.Memory().Value()
		}
		if isSharedSlotPod(p, req) {
			u.usedSlots++
		}
		if isBlockingWorkloadPod(p, gpus, req, managedNamespaces) {
			u.blockingPods = append(u.blockingPods, p.Namespace+"/"+p.Name)
		}
	}
	for _, u := range out {
		slices.Sort(u.blockingPods)
	}
	return out
}

func isSharedSlotPod(p *corev1.Pod, req corev1.ResourceList) bool {
	if p.Labels[naming.LabelPoolMode] == "shared" {
		return true
	}
	if q, ok := req[corev1.ResourceName(naming.ResourceSharedSlot)]; ok && q.Value() > 0 {
		return true
	}
	return false
}

// isBlockingWorkloadPod identifies active workload pods that prevent moving a
// node between ResourcePools (matching NeoGPU's idle-node transfer rule).
func isBlockingWorkloadPod(
	p *corev1.Pod, gpus int64, req corev1.ResourceList, managedNamespaces map[string]bool,
) bool {
	if p.Annotations["kubernetes.io/config.mirror"] != "" {
		return false
	}
	for _, owner := range p.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return false
		}
	}
	if gpus > 0 || isSharedSlotPod(p, req) {
		return true
	}
	if p.Labels[naming.LabelProject] != "" || managedNamespaces[p.Namespace] {
		return true
	}
	return false
}

func quantity(l corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := l[name]; ok {
		return q.Value()
	}
	return 0
}

func matchesSelector(nodeLabels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if nodeLabels[k] != v {
			return false
		}
	}
	return true
}

func floorDiv(a, b int64) int64 {
	if b <= 0 || a <= 0 {
		return 0
	}
	return a / b
}

// Compute builds the cluster-wide ResourcePool assignment plan, enforcing
// zone constraints and idle-node transfer guards, and computes per-pool GPU
// capacity and fragmentation-aware launchable instance counts.
func Compute(
	pools []v1.ResourcePool,
	nodes []corev1.Node,
	pods []corev1.Pod,
	instanceTypes []v1.InstanceType,
	managedNamespaces map[string]bool,
	cfg platform.InstanceTypes,
) Plan {
	poolByName := make(map[string]*v1.ResourcePool, len(pools))
	poolNames := make([]string, 0, len(pools))
	for i := range pools {
		p := pools[i].DeepCopy()
		poolByName[p.Name] = p
		poolNames = append(poolNames, p.Name)
	}
	slices.Sort(poolNames)

	nodeByName := make(map[string]*corev1.Node, len(nodes))
	nodeNames := make([]string, 0, len(nodes))
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
		nodeNames = append(nodeNames, nodes[i].Name)
	}
	slices.Sort(nodeNames)

	usage := computeNodeUsage(pods, managedNamespaces)

	desiredPool, poolIssues := assignPools(poolByName, poolNames, nodeByName, nodeNames)
	effectivePool, blockedTarget, blockedSource := guardTransfers(
		desiredPool, nodeByName, nodeNames, poolByName, usage,
	)

	// Step 3: Build per-node assignments and per-pool member node lists.
	plan := Plan{
		Nodes: make(map[string]NodeAssignment, len(effectivePool)),
		Pools: make(map[string]PoolResult, len(poolNames)),
	}
	poolNodes := map[string][]string{}
	for _, nName := range nodeNames {
		pName, ok := effectivePool[nName]
		if !ok {
			continue
		}
		p := poolByName[pName]
		mode := EffectiveMode(p)
		plan.Nodes[nName] = NodeAssignment{
			NodeName:        nName,
			Pool:            pName,
			Mode:            mode,
			WantSharedTaint: mode == v1.PoolModeShared,
		}
		poolNodes[pName] = append(poolNodes[pName], nName)
	}

	resCPU := cfg.Reserved.CPU.MilliValue()
	resMem := cfg.Reserved.Memory.Value()

	// Step 4: Compute status, flavors and fragmentation-aware launchable counts per pool.
	for _, pName := range poolNames {
		p := poolByName[pName]
		mode := EffectiveMode(p)
		assigned := poolNodes[pName]
		slices.Sort(assigned)

		res := PoolResult{
			Pool: pName,
			Mode: mode,
			Status: v1.ResourcePoolStatus{
				ObservedGeneration: p.Generation,
				TotalNodes:         int32(len(assigned)),
			},
		}

		gpuSummary := map[string]*v1.ResourcePoolGPUStatus{}
		maxSlots := int32(0)
		if mode == v1.PoolModeShared {
			maxSlots = MaxSharedSlots(p)
		}

		var blockedMsgs []string
		for _, nName := range assigned {
			n := nodeByName[nName]
			u := usage[nName]
			if u == nil {
				u = &nodeUsage{}
			}
			ready := instancetype.NodeSchedulable(n, mode == v1.PoolModeShared)
			if ready {
				res.Status.ReadyNodes++
			}
			totalGPU := int32(quantity(n.Status.Allocatable, instancetype.ResourceGPU))
			allocGPU := int32(u.usedGPU)
			freeGPU := int32(0)
			if ready {
				freeGPU = max(totalGPU-allocGPU, 0)
			}
			state := v1.NodePoolReady
			msg := ""
			if bMsg, blocked := blockedSource[pName][nName]; blocked {
				state = v1.NodePoolTransferBlocked
				msg = bMsg
				blockedMsgs = append(blockedMsgs, bMsg)
			}
			res.Status.Nodes = append(res.Status.Nodes, v1.ResourcePoolNodeStatus{
				Node:          nName,
				Zone:          n.Labels[naming.LabelTopologyZone],
				Ready:         ready,
				TotalGPUs:     totalGPU,
				AllocatedGPUs: allocGPU,
				FreeGPUs:      freeGPU,
				UsedSlots:     u.usedSlots,
				MaxSlots:      maxSlots,
				State:         state,
				Message:       msg,
			})

			product := n.Labels[naming.LabelGPUProduct]
			if product != "" && totalGPU > 0 {
				cls := naming.GPUClass(product)
				gs := gpuSummary[cls]
				if gs == nil {
					gs = &v1.ResourcePoolGPUStatus{Class: cls, Product: product}
					gpuSummary[cls] = gs
				}
				if ready {
					gs.Total += totalGPU
					gs.Free += freeGPU
				}
				gs.Allocated += allocGPU
			}
		}

		// Record nodes blocked from entering this pool.
		blockedTargets := make([]string, 0, len(blockedTarget[pName]))
		for nName := range blockedTarget[pName] {
			blockedTargets = append(blockedTargets, nName)
		}
		slices.Sort(blockedTargets)
		for _, nName := range blockedTargets {
			bMsg := blockedTarget[pName][nName]
			blockedMsgs = append(blockedMsgs, bMsg)
			n := nodeByName[nName]
			res.Status.Nodes = append(res.Status.Nodes, v1.ResourcePoolNodeStatus{
				Node:    nName,
				Zone:    n.Labels[naming.LabelTopologyZone],
				Ready:   false,
				State:   v1.NodePoolTransferBlocked,
				Message: bMsg,
			})
		}

		// Record explicit issues (NotFound / Conflict / ZoneMismatch).
		if issues := poolIssues[pName]; len(issues) > 0 {
			slices.SortFunc(issues, func(a, b v1.ResourcePoolNodeStatus) int { return cmp.Compare(a.Node, b.Node) })
			res.Status.Nodes = append(res.Status.Nodes, issues...)
		}

		if len(blockedMsgs) > 0 {
			res.TransferBlocked = true
			res.BlockedMessage = strings.Join(blockedMsgs, "; ")
		}

		// Sort GPU summaries and build Kueue ResourceFlavors for this pool.
		gpuClasses := make([]string, 0, len(gpuSummary))
		for cls := range gpuSummary {
			gpuClasses = append(gpuClasses, cls)
		}
		slices.Sort(gpuClasses)
		for _, cls := range gpuClasses {
			gs := gpuSummary[cls]
			res.Status.GPUs = append(res.Status.GPUs, *gs)
			if mode == v1.PoolModeExclusive {
				res.Flavors = append(res.Flavors, PoolFlavor{
					Name:       naming.PoolResourceFlavor(pName, cls),
					Pool:       pName,
					Mode:       mode,
					GPUClass:   cls,
					GPUProduct: gs.Product,
				})
			}
		}
		if mode == v1.PoolModeShared {
			res.Flavors = append(res.Flavors, PoolFlavor{
				Name:        naming.PoolSharedFlavor(pName),
				Pool:        pName,
				Mode:        mode,
				SharedTaint: true,
			})
		}

		// Compute fragmentation-aware Launchable instance counts for this pool.
		res.Status.Launchable = computeLaunchable(
			mode, assigned, nodeByName, usage, instanceTypes, gpuSummary, maxSlots, resCPU, resMem,
		)

		plan.Pools[pName] = res
	}

	return plan
}

// assignPools decides the desired pool of every node from explicit claims,
// dynamic nodeSelectors and the built-in default pool, recording explicit
// NotFound, Conflict and ZoneMismatch issues per pool.
func assignPools(
	poolByName map[string]*v1.ResourcePool,
	poolNames []string,
	nodeByName map[string]*corev1.Node,
	nodeNames []string,
) (map[string]string, map[string][]v1.ResourcePoolNodeStatus) {
	desiredPool := map[string]string{}
	poolIssues := map[string][]v1.ResourcePoolNodeStatus{}

	// 1a. Explicit spec.nodes (named pools take precedence over default; if two
	// named pools claim the same node, the current holder wins, else alphabetical).
	claims := map[string][]string{}
	for _, pName := range poolNames {
		if pName == naming.DefaultPoolName {
			continue
		}
		p := poolByName[pName]
		for _, nName := range p.Spec.Nodes {
			if _, exists := nodeByName[nName]; !exists {
				poolIssues[pName] = append(poolIssues[pName], v1.ResourcePoolNodeStatus{
					Node:    nName,
					State:   v1.NodePoolNotFound,
					Message: "node does not exist in cluster",
				})
				continue
			}
			claims[nName] = append(claims[nName], pName)
		}
	}

	for _, nName := range nodeNames {
		n := nodeByName[nName]
		claimants := claims[nName]
		if len(claimants) == 0 {
			continue
		}
		winner := claimants[0]
		curPool := n.Labels[naming.LabelPool]
		if curPool != "" && slices.Contains(claimants, curPool) {
			winner = curPool
		}
		for _, c := range claimants {
			if c != winner {
				poolIssues[c] = append(poolIssues[c], v1.ResourcePoolNodeStatus{
					Node:    nName,
					Zone:    n.Labels[naming.LabelTopologyZone],
					State:   v1.NodePoolConflict,
					Message: fmt.Sprintf("node is already assigned to pool %s", winner),
				})
			}
		}
		p := poolByName[winner]
		if p.Spec.Zone != "" && n.Labels[naming.LabelTopologyZone] != p.Spec.Zone {
			poolIssues[winner] = append(poolIssues[winner], v1.ResourcePoolNodeStatus{
				Node:    nName,
				Zone:    n.Labels[naming.LabelTopologyZone],
				State:   v1.NodePoolZoneMismatch,
				Message: fmt.Sprintf("node zone %q does not match pool zone %q", n.Labels[naming.LabelTopologyZone], p.Spec.Zone),
			})
			continue
		}
		desiredPool[nName] = winner
	}

	// 1b. Dynamic spec.nodeSelector on named pools for nodes not explicitly claimed.
	for _, pName := range poolNames {
		if pName == naming.DefaultPoolName {
			continue
		}
		p := poolByName[pName]
		if len(p.Spec.NodeSelector) == 0 {
			continue
		}
		for _, nName := range nodeNames {
			if _, taken := desiredPool[nName]; taken {
				continue
			}
			n := nodeByName[nName]
			if !matchesSelector(n.Labels, p.Spec.NodeSelector) {
				continue
			}
			if p.Spec.Zone != "" && n.Labels[naming.LabelTopologyZone] != p.Spec.Zone {
				poolIssues[pName] = append(poolIssues[pName], v1.ResourcePoolNodeStatus{
					Node:    nName,
					Zone:    n.Labels[naming.LabelTopologyZone],
					State:   v1.NodePoolZoneMismatch,
					Message: fmt.Sprintf("node zone %q does not match pool zone %q", n.Labels[naming.LabelTopologyZone], p.Spec.Zone),
				})
				continue
			}
			desiredPool[nName] = pName
		}
	}

	// 1c. Remaining nodes fall into the built-in default pool (when it exists).
	if _, hasDefault := poolByName[naming.DefaultPoolName]; hasDefault {
		for _, nName := range nodeNames {
			if _, taken := desiredPool[nName]; !taken {
				desiredPool[nName] = naming.DefaultPoolName
			}
		}
	}

	return desiredPool, poolIssues
}

// guardTransfers applies the idle-node transfer guard: a node that still runs
// active workloads stays in its current pool instead of moving to the target.
func guardTransfers(
	desiredPool map[string]string,
	nodeByName map[string]*corev1.Node,
	nodeNames []string,
	poolByName map[string]*v1.ResourcePool,
	usage map[string]*nodeUsage,
) (map[string]string, map[string]map[string]string, map[string]map[string]string) {
	effectivePool := map[string]string{}
	blockedTarget := map[string]map[string]string{} // targetPool -> nodeName -> msg
	blockedSource := map[string]map[string]string{} // sourcePool -> nodeName -> msg

	for _, nName := range nodeNames {
		target, ok := desiredPool[nName]
		if !ok {
			continue
		}
		n := nodeByName[nName]
		cur := n.Labels[naming.LabelPool]
		// Only block if the node is migrating away from a different, still-existing pool.
		if cur != "" && cur != target {
			if _, curExists := poolByName[cur]; curExists {
				if u := usage[nName]; u != nil && len(u.blockingPods) > 0 {
					msg := fmt.Sprintf(
						"cannot transfer node from pool %s to %s while active workloads run: %s",
						cur, target, strings.Join(u.blockingPods, ", "),
					)
					effectivePool[nName] = cur
					if blockedTarget[target] == nil {
						blockedTarget[target] = map[string]string{}
					}
					blockedTarget[target][nName] = msg
					if blockedSource[cur] == nil {
						blockedSource[cur] = map[string]string{}
					}
					blockedSource[cur][nName] = msg
					continue
				}
			}
		}
		effectivePool[nName] = target
	}

	return effectivePool, blockedTarget, blockedSource
}

func computeLaunchable(
	mode v1.ResourcePoolMode,
	assigned []string,
	nodeByName map[string]*corev1.Node,
	usage map[string]*nodeUsage,
	instanceTypes []v1.InstanceType,
	gpuSummary map[string]*v1.ResourcePoolGPUStatus,
	maxSlots int32,
	resCPU, resMem int64,
) []v1.LaunchableInstanceType {
	if mode == v1.PoolModeShared {
		// Each shared GPU class in the pool reports total free sandbox slots across ready nodes.
		classes := make([]string, 0, len(gpuSummary))
		for cls := range gpuSummary {
			classes = append(classes, cls)
		}
		slices.Sort(classes)
		out := make([]v1.LaunchableInstanceType, 0, len(classes))
		for _, cls := range classes {
			var count int32
			for _, nName := range assigned {
				n := nodeByName[nName]
				if naming.GPUClass(n.Labels[naming.LabelGPUProduct]) != cls {
					continue
				}
				if !instancetype.NodeSchedulable(n, true) {
					continue
				}
				var used int32
				if u := usage[nName]; u != nil {
					used = u.usedSlots
				}
				count += max(maxSlots-used, 0)
			}
			out = append(out, v1.LaunchableInstanceType{
				InstanceType: naming.SharedInstanceTypeName(cls),
				Count:        count,
			})
		}
		return out
	}

	// Exclusive pool: evaluate every non-shared InstanceType against pool nodes.
	sortedITs := slices.Clone(instanceTypes)
	slices.SortFunc(sortedITs, func(a, b v1.InstanceType) int { return cmp.Compare(a.Name, b.Name) })
	var out []v1.LaunchableInstanceType
	for i := range sortedITs {
		it := &sortedITs[i]
		if it.Spec.Shared {
			continue
		}
		needGPU := int64(it.Spec.GPU.Count)
		needCPU := it.Spec.CPU.MilliValue()
		needMem := it.Spec.Memory.Value()
		if needGPU > 0 {
			if _, hasClass := gpuSummary[it.Spec.GPU.Class]; !hasClass {
				continue
			}
		}
		var count int32
		for _, nName := range assigned {
			n := nodeByName[nName]
			if !instancetype.NodeSchedulable(n, false) {
				continue
			}
			if needGPU > 0 {
				product := n.Labels[naming.LabelGPUProduct]
				if product == "" || naming.GPUClass(product) != it.Spec.GPU.Class {
					continue
				}
			}
			u := usage[nName]
			if u == nil {
				u = &nodeUsage{}
			}
			allocGPU := quantity(n.Status.Allocatable, instancetype.ResourceGPU)
			allocCPU := n.Status.Allocatable.Cpu().MilliValue()
			allocMem := n.Status.Allocatable.Memory().Value()
			freeCPU := allocCPU - resCPU - u.usedCPU - max(u.otherCPU-resCPU, 0)
			freeMem := allocMem - resMem - u.usedMem - max(u.otherMem-resMem, 0)

			var fits int64
			if needGPU > 0 {
				fits = min(
					floorDiv(allocGPU-u.usedGPU, needGPU),
					floorDiv(freeCPU, needCPU),
					floorDiv(freeMem, needMem),
				)
			} else {
				fits = min(floorDiv(freeCPU, needCPU), floorDiv(freeMem, needMem))
			}
			if fits > 0 {
				count += int32(fits)
			}
		}
		out = append(out, v1.LaunchableInstanceType{
			InstanceType: it.Name,
			Count:        count,
		})
	}
	return out
}
