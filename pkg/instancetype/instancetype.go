// Package instancetype derives GPU, CPU-only and Shared instance types from
// the cluster's nodes and computes how many of each fit now. It is a pure
// function of nodes, pods, resource pools and the platform settings.
package instancetype

import (
	"cmp"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
)

// GVK of the InstanceType.
var GVK = schema.GroupVersionKind{Group: "workspace.wutz.dev", Version: "v1alpha1", Kind: "InstanceType"}

// ResourceGPU is the NVIDIA device plugin resource.
const ResourceGPU corev1.ResourceName = "nvidia.com/gpu"

const (
	milliCore = 1000
	gib       = int64(1) << 30
)

// Class is a group of nodes with the same GPU product.
type Class struct {
	Name    string
	Product string
	// MemoryMiB of one GPU.
	MemoryMiB int64
	// MaxGPUs is the largest GPU count of a node in the class.
	MaxGPUs int32
	// MilliCPU and Memory (bytes) per GPU: (allocatable - reserved) / GPUs,
	// rounded down to whole cores and GiB, minimum over the nodes.
	MilliCPU int64
	Memory   int64
	Nodes    []string
}

// CPUPerGPU returns the per-GPU CPU slice as a quantity.
func (c Class) CPUPerGPU() resource.Quantity {
	return *resource.NewMilliQuantity(c.MilliCPU, resource.DecimalSI)
}

// MemoryPerGPU returns the per-GPU memory slice as a quantity.
func (c Class) MemoryPerGPU() resource.Quantity {
	return *resource.NewQuantity(c.Memory, resource.BinarySI)
}

// node is the scheduling view of a compute node.
type node struct {
	name                     string
	class                    string
	schedulable              bool
	sharedSchedulable        bool
	gpus, cpu, mem           int64 // allocatable
	usedGPU                  int64
	usedCPU, usedMem         int64 // by pods requesting GPUs
	otherCPU, otherMem       int64 // by other pods (system daemons first use the reservation)
	reservedCPU, reservedMem int64
}

func quantity(l corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := l[name]; ok {
		return q.Value()
	}
	return 0
}

// NodeSchedulable reports whether a node can accept workloads. Unschedulable,
// NotReady or hardware-fault-tainted nodes return false. When allowSharedPool
// is true, the workspace.wutz.dev/shared-pool NoSchedule taint is tolerated.
func NodeSchedulable(n *corev1.Node, allowSharedPool bool) bool {
	if n.Spec.Unschedulable {
		return false
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		if t.Key == string(ResourceGPU) && t.Effect == corev1.TaintEffectNoSchedule {
			// Treat explicit unhealthy GPU taint values as unschedulable fault markers.
			if t.Value == "unhealthy" || t.Value == "fault" {
				return false
			}
			continue
		}
		if allowSharedPool && t.Key == naming.TaintSharedPool && t.Effect == corev1.TaintEffectNoSchedule {
			continue
		}
		return false
	}
	return true
}

// PodRequests returns the effective requests of a pod as the scheduler sees
// them: max(sum(containers)+sidecars, each init container) + overhead.
func PodRequests(p *corev1.Pod) corev1.ResourceList {
	reqs := corev1.ResourceList{}
	add := func(dst, src corev1.ResourceList) {
		for k, v := range src {
			q := dst[k]
			q.Add(v)
			dst[k] = q
		}
	}
	for _, c := range p.Spec.Containers {
		add(reqs, c.Resources.Requests)
	}
	sidecars := corev1.ResourceList{}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			add(reqs, c.Resources.Requests)
			add(sidecars, c.Resources.Requests)
			continue
		}
		need := sidecars.DeepCopy()
		add(need, c.Resources.Requests)
		for k, v := range need {
			if cur, ok := reqs[k]; !ok || v.Cmp(cur) > 0 {
				reqs[k] = v.DeepCopy()
			}
		}
	}
	add(reqs, p.Spec.Overhead)
	return reqs
}

// Compute returns the GPU classes and the exclusive GPU (and configured CPU-only)
// instance types with availability.
func Compute(
	nodes []corev1.Node, pods []corev1.Pod, cfg platform.InstanceTypes, now metav1.Time,
) ([]Class, []v1.InstanceType) {
	return ComputeAll(nodes, pods, nil, cfg, now)
}

// ComputeAll returns the GPU classes and all derived instance types:
// exclusive GPU sizes, configured CPU-only sizes, and shared pool instance types.
func ComputeAll(
	nodes []corev1.Node, pods []corev1.Pod, pools []v1.ResourcePool, cfg platform.InstanceTypes, now metav1.Time,
) ([]Class, []v1.InstanceType) {
	resCPU := cfg.Reserved.CPU.MilliValue()
	resMem := cfg.Reserved.Memory.Value()

	byName := map[string]*node{}
	allNodes := map[string]*node{}
	classes := map[string]*Class{}
	for i := range nodes {
		n := &nodes[i]
		cpu := n.Status.Allocatable.Cpu().MilliValue()
		mem := n.Status.Allocatable.Memory().Value()
		gpus := quantity(n.Status.Allocatable, ResourceGPU)
		product := n.Labels[naming.LabelGPUProduct]
		nd := &node{
			name:              n.Name,
			schedulable:       NodeSchedulable(n, false),
			sharedSchedulable: NodeSchedulable(n, true),
			gpus:              gpus,
			cpu:               cpu,
			mem:               mem,
			reservedCPU:       resCPU,
			reservedMem:       resMem,
		}
		allNodes[n.Name] = nd
		if product == "" || gpus <= 0 {
			continue
		}
		cls := naming.GPUClass(product)
		nd.class = cls
		byName[n.Name] = nd
		sliceCPU := max(cpu-resCPU, 0) / gpus / milliCore * milliCore
		sliceMem := max(mem-resMem, 0) / gpus / gib * gib
		c := classes[cls]
		if c == nil {
			c = &Class{Name: cls, Product: product, MilliCPU: sliceCPU, Memory: sliceMem}
			c.MemoryMiB, _ = strconv.ParseInt(n.Labels[naming.LabelGPUMemory], 10, 64)
			classes[cls] = c
		}
		c.MilliCPU = min(c.MilliCPU, sliceCPU)
		c.Memory = min(c.Memory, sliceMem)
		c.MaxGPUs = max(c.MaxGPUs, int32(gpus))
		c.Nodes = append(c.Nodes, n.Name)
	}

	sharedUsedSlots := map[string]int32{}
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		nd := allNodes[p.Spec.NodeName]
		if nd == nil {
			continue
		}
		req := PodRequests(p)
		if g := quantity(req, ResourceGPU); g > 0 {
			nd.usedGPU += g
			nd.usedCPU += req.Cpu().MilliValue()
			nd.usedMem += req.Memory().Value()
		} else {
			nd.otherCPU += req.Cpu().MilliValue()
			nd.otherMem += req.Memory().Value()
		}
		if isSharedWorkloadPod(p) {
			sharedUsedSlots[p.Spec.NodeName]++
		}
	}

	out := make([]Class, 0, len(classes))
	for _, c := range classes {
		slices.Sort(c.Nodes)
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b Class) int { return cmp.Compare(a.Name, b.Name) })

	var types []v1.InstanceType
	for _, c := range out {
		if c.MilliCPU == 0 || c.Memory == 0 {
			continue
		}
		for _, size := range cfg.Sizes {
			if size < 1 || size > c.MaxGPUs {
				continue
			}
			types = append(types, instanceType(c, size, byName, cfg, now))
		}
	}

	// Derive CPU-only instance types when configured.
	if len(cfg.CPUSizes) > 0 {
		types = append(types, cpuInstanceTypes(nodes, allNodes, cfg.CPUSizes, now)...)
	}

	// Derive Shared instance types for GPU classes present in Shared ResourcePools.
	if len(pools) > 0 {
		types = append(types, sharedInstanceTypes(nodes, pools, out, byName, sharedUsedSlots, cfg, now)...)
	}

	return out, types
}

func isSharedWorkloadPod(p *corev1.Pod) bool {
	if p.Labels[naming.LabelPoolMode] == "shared" {
		return true
	}
	req := PodRequests(p)
	if q, ok := req[corev1.ResourceName(naming.ResourceSharedSlot)]; ok && q.Value() > 0 {
		return true
	}
	return false
}

func floorDiv(a, b int64) int64 {
	if b <= 0 || a <= 0 {
		return 0
	}
	return a / b
}

func scaledExtendedResources(base corev1.ResourceList, factor int64) corev1.ResourceList {
	if len(base) == 0 || factor <= 0 {
		return nil
	}
	out := make(corev1.ResourceList, len(base))
	for k, v := range base {
		q := resource.NewQuantity(v.Value()*factor, v.Format)
		out[k] = *q
	}
	return out
}

func instanceType(c Class, size int32, nodes map[string]*node, cfg platform.InstanceTypes, now metav1.Time) v1.InstanceType {
	n64 := int64(size)
	spec := v1.InstanceTypeSpec{
		GPU:               v1.InstanceTypeGPU{Class: c.Name, Product: c.Product, Count: size, MemoryMiB: c.MemoryMiB},
		CPU:               *resource.NewMilliQuantity(c.MilliCPU*n64, resource.DecimalSI),
		Memory:            *resource.NewQuantity(c.Memory*n64, resource.BinarySI),
		ExtendedResources: scaledExtendedResources(cfg.ExtendedResources, n64),
		NodeSelector:      map[string]string{naming.LabelGPUProduct: c.Product},
	}
	if cfg.SharedMemoryPerGPU != nil && !cfg.SharedMemoryPerGPU.IsZero() {
		spec.SharedMemory = resource.NewQuantity(cfg.SharedMemoryPerGPU.Value()*n64, resource.BinarySI)
	}
	it := v1.InstanceType{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.InstanceTypeName(c.Name, size),
			Labels: map[string]string{
				naming.LabelManagedBy: naming.ManagedBy,
				naming.LabelGPUClass:  c.Name,
			},
		},
		Spec:   spec,
		Status: v1.InstanceTypeStatus{Nodes: []v1.NodeAvailability{}, UpdateTime: &now},
	}
	needCPU, needMem := c.MilliCPU*n64, c.Memory*n64
	for _, name := range c.Nodes {
		n := nodes[name]
		a := v1.NodeAvailability{Node: name}
		if n.schedulable {
			a.Total = int32(floorDiv(n.gpus, n64))
			freeCPU := n.cpu - n.reservedCPU - n.usedCPU - max(n.otherCPU-n.reservedCPU, 0)
			freeMem := n.mem - n.reservedMem - n.usedMem - max(n.otherMem-n.reservedMem, 0)
			free := min(floorDiv(n.gpus-n.usedGPU, n64), floorDiv(freeCPU, needCPU), floorDiv(freeMem, needMem))
			a.Free = int32(min(max(free, 0), int64(a.Total)))
		}
		it.Status.Total += a.Total
		it.Status.Free += a.Free
		it.Status.Nodes = append(it.Status.Nodes, a)
	}
	return it
}

func cpuInstanceTypes(
	rawNodes []corev1.Node, allNodes map[string]*node, sizes []platform.CPUSize, now metav1.Time,
) []v1.InstanceType {
	// Prefer dedicated CPU-only nodes; fall back to all schedulable nodes if none exist.
	var cpuNodes []string
	var fallback []string
	for i := range rawNodes {
		name := rawNodes[i].Name
		nd := allNodes[name]
		if nd == nil {
			continue
		}
		if nd.gpus == 0 {
			cpuNodes = append(cpuNodes, name)
		}
		fallback = append(fallback, name)
	}
	candidates := cpuNodes
	if len(candidates) == 0 {
		candidates = fallback
	}
	slices.Sort(candidates)
	if len(candidates) == 0 {
		return nil
	}

	out := make([]v1.InstanceType, 0, len(sizes))
	for _, sz := range sizes {
		if sz.Cores <= 0 || sz.Memory.IsZero() {
			continue
		}
		needCPU := int64(sz.Cores) * milliCore
		needMem := sz.Memory.Value()
		it := v1.InstanceType{
			ObjectMeta: metav1.ObjectMeta{
				Name: naming.CPUInstanceTypeName(sz.Cores),
				Labels: map[string]string{
					naming.LabelManagedBy: naming.ManagedBy,
					naming.LabelGPUClass:  "cpu",
				},
			},
			Spec: v1.InstanceTypeSpec{
				GPU:    v1.InstanceTypeGPU{Class: "cpu", Count: 0},
				CPU:    *resource.NewMilliQuantity(needCPU, resource.DecimalSI),
				Memory: sz.Memory.DeepCopy(),
			},
			Status: v1.InstanceTypeStatus{Nodes: []v1.NodeAvailability{}, UpdateTime: &now},
		}
		for _, name := range candidates {
			nd := allNodes[name]
			a := v1.NodeAvailability{Node: name}
			if nd.schedulable {
				usableCPU := max(nd.cpu-nd.reservedCPU, 0)
				usableMem := max(nd.mem-nd.reservedMem, 0)
				a.Total = int32(min(floorDiv(usableCPU, needCPU), floorDiv(usableMem, needMem)))
				freeCPU := nd.cpu - nd.reservedCPU - nd.usedCPU - max(nd.otherCPU-nd.reservedCPU, 0)
				freeMem := nd.mem - nd.reservedMem - nd.usedMem - max(nd.otherMem-nd.reservedMem, 0)
				free := min(floorDiv(freeCPU, needCPU), floorDiv(freeMem, needMem))
				a.Free = int32(min(max(free, 0), int64(a.Total)))
			}
			it.Status.Total += a.Total
			it.Status.Free += a.Free
			it.Status.Nodes = append(it.Status.Nodes, a)
		}
		out = append(out, it)
	}
	return out
}

func sharedInstanceTypes(
	rawNodes []corev1.Node, pools []v1.ResourcePool, classes []Class,
	gpuNodes map[string]*node, usedSlots map[string]int32, cfg platform.InstanceTypes, now metav1.Time,
) []v1.InstanceType {
	// Map pool name -> max instances per node for Shared pools.
	sharedPools := map[string]int32{}
	for i := range pools {
		if pools[i].Spec.Mode != v1.PoolModeShared {
			continue
		}
		maxSlots := int32(16)
		if pools[i].Spec.Sharing != nil && pools[i].Spec.Sharing.MaxInstancesPerNode > 0 {
			maxSlots = pools[i].Spec.Sharing.MaxInstancesPerNode
		}
		sharedPools[pools[i].Name] = maxSlots
	}
	if len(sharedPools) == 0 {
		return nil
	}

	nodePool := map[string]string{}
	for i := range rawNodes {
		if p := rawNodes[i].Labels[naming.LabelPool]; p != "" {
			nodePool[rawNodes[i].Name] = p
		}
	}

	var out []v1.InstanceType
	for _, c := range classes {
		if c.MilliCPU == 0 || c.Memory == 0 {
			continue
		}
		var matched []string
		for _, nName := range c.Nodes {
			if _, ok := sharedPools[nodePool[nName]]; ok {
				matched = append(matched, nName)
			}
		}
		if len(matched) == 0 {
			continue
		}
		// NeoGPU shared dev machines see all node GPUs (e.g. 8) while sharing CPU/Mem slice per slot.
		it := v1.InstanceType{
			ObjectMeta: metav1.ObjectMeta{
				Name: naming.SharedInstanceTypeName(c.Name),
				Labels: map[string]string{
					naming.LabelManagedBy: naming.ManagedBy,
					naming.LabelGPUClass:  c.Name,
					naming.LabelPoolMode:  "shared",
				},
			},
			Spec: v1.InstanceTypeSpec{
				GPU: v1.InstanceTypeGPU{
					Class: c.Name, Product: c.Product, Count: c.MaxGPUs, MemoryMiB: c.MemoryMiB,
				},
				CPU:          *resource.NewMilliQuantity(c.MilliCPU, resource.DecimalSI),
				Memory:       *resource.NewQuantity(c.Memory, resource.BinarySI),
				Shared:       true,
				NodeSelector: map[string]string{naming.LabelGPUProduct: c.Product, naming.LabelPoolMode: "shared"},
			},
			Status: v1.InstanceTypeStatus{Nodes: []v1.NodeAvailability{}, UpdateTime: &now},
		}
		if cfg.SharedMemoryPerGPU != nil && !cfg.SharedMemoryPerGPU.IsZero() {
			q := cfg.SharedMemoryPerGPU.DeepCopy()
			it.Spec.SharedMemory = &q
		}
		for _, nName := range matched {
			nd := gpuNodes[nName]
			maxSlots := sharedPools[nodePool[nName]]
			a := v1.NodeAvailability{Node: nName}
			if nd != nil && nd.sharedSchedulable {
				a.Total = maxSlots
				a.Free = max(maxSlots-usedSlots[nName], 0)
			}
			it.Status.Total += a.Total
			it.Status.Free += a.Free
			it.Status.Nodes = append(it.Status.Nodes, a)
		}
		out = append(out, it)
	}
	return out
}
