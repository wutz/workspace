package resourcepool

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/instancetype"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
)

func makeGPUNode(name, zone, currentPool string, gpus int64, ready bool) corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	labels := map[string]string{
		naming.LabelGPUProduct: "NVIDIA-GeForce-RTX-5090",
		naming.LabelGPUMemory:  "32607",
	}
	if zone != "" {
		labels[naming.LabelTopologyZone] = zone
	}
	if currentPool != "" {
		labels[naming.LabelPool] = currentPool
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:       resource.MustParse("128"),
				corev1.ResourceMemory:    resource.MustParse("1Ti"),
				instancetype.ResourceGPU: *resource.NewQuantity(gpus, resource.DecimalSI),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func makePod(ns, name, node string, cpu, mem string, gpus int64, sharedSlots int64) corev1.Pod {
	req := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
	if gpus > 0 {
		req[instancetype.ResourceGPU] = *resource.NewQuantity(gpus, resource.DecimalSI)
	}
	if sharedSlots > 0 {
		req[corev1.ResourceName(naming.ResourceSharedSlot)] = *resource.NewQuantity(sharedSlots, resource.DecimalSI)
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: req}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func testITCfg() platform.InstanceTypes {
	c, _ := platform.Parse([]byte("instanceTypes: {reserved: {cpu: '8', memory: 64Gi}, sizes: [1, 2, 4, 8]}"))
	return c.InstanceTypes
}

func TestPoolAssignmentPrecedenceAndDefault(t *testing.T) {
	n1 := makeGPUNode("n1", "az-a", "", 8, true)
	n2 := makeGPUNode("n2", "az-a", "", 8, true)
	n2.Labels["tier"] = "shared-dev"
	n3 := makeGPUNode("n3", "az-a", "", 8, true)

	pools := []v1.ResourcePool{
		{ObjectMeta: metav1.ObjectMeta{Name: naming.DefaultPoolName}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "llm-exclusive"}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive, Nodes: []string{"n1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "dev-shared"}, Spec: v1.ResourcePoolSpec{
			Mode:         v1.PoolModeShared,
			NodeSelector: map[string]string{"tier": "shared-dev"},
			Sharing:      &v1.ResourcePoolSharing{MaxInstancesPerNode: 12},
		}},
	}
	_, its := instancetype.ComputeAll([]corev1.Node{n1, n2, n3}, nil, pools, testITCfg(), metav1.Now())
	plan := Compute(pools, []corev1.Node{n1, n2, n3}, nil, its, nil, testITCfg())

	if plan.Nodes["n1"].Pool != "llm-exclusive" || plan.Nodes["n1"].WantSharedTaint {
		t.Fatalf("n1 assignment = %+v", plan.Nodes["n1"])
	}
	if plan.Nodes["n2"].Pool != "dev-shared" || !plan.Nodes["n2"].WantSharedTaint {
		t.Fatalf("n2 assignment = %+v", plan.Nodes["n2"])
	}
	if plan.Nodes["n3"].Pool != naming.DefaultPoolName || plan.Nodes["n3"].WantSharedTaint {
		t.Fatalf("n3 assignment = %+v", plan.Nodes["n3"])
	}

	if plan.Pools["llm-exclusive"].Status.TotalNodes != 1 || plan.Pools["llm-exclusive"].Status.ReadyNodes != 1 {
		t.Fatalf("llm-exclusive status = %+v", plan.Pools["llm-exclusive"].Status)
	}
	if got := plan.Pools["dev-shared"].Status.Launchable; len(got) != 1 || got[0].Count != 12 {
		t.Fatalf("dev-shared launchable = %+v", got)
	}
	if len(plan.Pools["dev-shared"].Flavors) != 1 || !plan.Pools["dev-shared"].Flavors[0].SharedTaint {
		t.Fatalf("dev-shared flavors = %+v", plan.Pools["dev-shared"].Flavors)
	}
}

func TestConflictZoneAndMissingNode(t *testing.T) {
	n1 := makeGPUNode("n1", "az-a", "pool-b", 8, true)
	n2 := makeGPUNode("n2", "az-b", "", 8, true)

	pools := []v1.ResourcePool{
		{ObjectMeta: metav1.ObjectMeta{Name: naming.DefaultPoolName}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive, Zone: "az-a", Nodes: []string{"n1", "n2", "ghost"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pool-b"}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive, Zone: "az-a", Nodes: []string{"n1"}}},
	}

	plan := Compute(pools, []corev1.Node{n1, n2}, nil, nil, nil, testITCfg())
	// n1 is currently held by pool-b, so pool-b retains it and pool-a sees a Conflict.
	if plan.Nodes["n1"].Pool != "pool-b" {
		t.Fatalf("n1 holder = %+v, want pool-b", plan.Nodes["n1"])
	}
	// n2 has az-b != az-a, so it stays in default and pool-a records ZoneMismatch.
	if plan.Nodes["n2"].Pool != naming.DefaultPoolName {
		t.Fatalf("n2 holder = %+v, want default", plan.Nodes["n2"])
	}

	states := map[string]v1.NodePoolState{}
	for _, ns := range plan.Pools["pool-a"].Status.Nodes {
		states[ns.Node] = ns.State
	}
	if states["n1"] != v1.NodePoolConflict || states["n2"] != v1.NodePoolZoneMismatch || states["ghost"] != v1.NodePoolNotFound {
		t.Fatalf("pool-a node states = %+v", states)
	}
}

func TestIdleNodeTransferGuard(t *testing.T) {
	n1 := makeGPUNode("n1", "az-a", naming.DefaultPoolName, 8, true)
	pools := []v1.ResourcePool{
		{ObjectMeta: metav1.ObjectMeta{Name: naming.DefaultPoolName}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "llm-exclusive"}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive, Nodes: []string{"n1"}}},
	}
	managedNS := map[string]bool{"project-demo": true}

	// Active GPU pod blocks transfer from default -> llm-exclusive.
	busyPods := []corev1.Pod{
		makePod("kube-system", "kube-proxy-abc", "n1", "100m", "128Mi", 0, 0), // system daemon must NOT block
		makePod("project-demo", "train-worker-0", "n1", "16", "64Gi", 4, 0),
	}
	plan := Compute(pools, []corev1.Node{n1}, busyPods, nil, managedNS, testITCfg())
	if plan.Nodes["n1"].Pool != naming.DefaultPoolName {
		t.Fatalf("expected transfer blocked, got %+v", plan.Nodes["n1"])
	}
	res := plan.Pools["llm-exclusive"]
	if !res.TransferBlocked || !strings.Contains(res.BlockedMessage, "project-demo/train-worker-0") {
		t.Fatalf("expected TransferBlocked with pod name, got blocked=%v msg=%q", res.TransferBlocked, res.BlockedMessage)
	}

	// Once only system daemon remains, transfer succeeds.
	idlePods := []corev1.Pod{
		makePod("kube-system", "kube-proxy-abc", "n1", "100m", "128Mi", 0, 0),
	}
	plan2 := Compute(pools, []corev1.Node{n1}, idlePods, nil, managedNS, testITCfg())
	if plan2.Nodes["n1"].Pool != "llm-exclusive" || plan2.Pools["llm-exclusive"].TransferBlocked {
		t.Fatalf("expected transfer to succeed once idle: node=%+v pool=%+v", plan2.Nodes["n1"], plan2.Pools["llm-exclusive"])
	}
}

func TestFragmentationAwareLaunchable(t *testing.T) {
	// Two 8-GPU nodes in pool-a:
	// n1 has 5 GPUs used -> 3 GPUs free -> can launch 3x1, 1x2, 0x4, 0x8.
	// n2 has 2 GPUs used -> 6 GPUs free -> can launch 6x1, 3x2, 1x4, 0x8.
	// Total across pool: x1=9, x2=4, x4=1, x8=0 (even though 9 free GPUs > 8!).
	n1 := makeGPUNode("n1", "az-a", "pool-a", 8, true)
	n2 := makeGPUNode("n2", "az-a", "pool-a", 8, true)
	pools := []v1.ResourcePool{
		{ObjectMeta: metav1.ObjectMeta{Name: naming.DefaultPoolName}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}, Spec: v1.ResourcePoolSpec{Mode: v1.PoolModeExclusive, Nodes: []string{"n1", "n2"}}},
	}
	pods := []corev1.Pod{
		makePod("project-demo", "p1", "n1", "40", "200Gi", 5, 0),
		makePod("project-demo", "p2", "n2", "20", "100Gi", 2, 0),
	}
	_, its := instancetype.ComputeAll([]corev1.Node{n1, n2}, pods, pools, testITCfg(), metav1.Now())
	plan := Compute(pools, []corev1.Node{n1, n2}, pods, its, map[string]bool{"project-demo": true}, testITCfg())

	got := map[string]int32{}
	for _, l := range plan.Pools["pool-a"].Status.Launchable {
		got[l.InstanceType] = l.Count
	}
	want := map[string]int32{
		"rtx-5090.x1": 9,
		"rtx-5090.x2": 4,
		"rtx-5090.x4": 1,
		"rtx-5090.x8": 0,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("launchable[%s] = %d, want %d (all=%+v)", k, got[k], w, got)
		}
	}
	gpus := plan.Pools["pool-a"].Status.GPUs
	if len(gpus) != 1 || gpus[0].Total != 16 || gpus[0].Allocated != 7 || gpus[0].Free != 9 {
		t.Fatalf("pool GPUs = %+v", gpus)
	}
}

func TestProjectAllowedAndEffectiveHelpers(t *testing.T) {
	pub := &v1.ResourcePool{Spec: v1.ResourcePoolSpec{AccessControl: v1.ResourcePoolAccessControl{Visibility: v1.VisibilityPublic}}}
	res := &v1.ResourcePool{Spec: v1.ResourcePoolSpec{
		Mode:          v1.PoolModeShared,
		AccessControl: v1.ResourcePoolAccessControl{Visibility: v1.VisibilityRestricted, AllowedProjects: []string{"demo"}},
	}}
	if !ProjectAllowed(pub, "anyone") || !ProjectAllowed(res, "demo") || ProjectAllowed(res, "other") {
		t.Fatal("ProjectAllowed mismatch")
	}
	if !slices.Equal(EffectiveAllowedWorkloads(res), []v1.WorkloadKind{v1.WorkloadSandbox}) {
		t.Fatalf("shared workloads = %+v", EffectiveAllowedWorkloads(res))
	}
	if MaxSharedSlots(res) != DefaultMaxSharedInstancesPerNode {
		t.Fatalf("MaxSharedSlots = %d", MaxSharedSlots(res))
	}
}
