package naming

import "testing"

func TestGPUClass(t *testing.T) {
	for in, want := range map[string]string{
		"NVIDIA-GeForce-RTX-5090": "rtx-5090",
		"NVIDIA-H100-80GB-HBM3":   "h100-80gb-hbm3",
		"Tesla-V100-SXM2-32GB":    "v100-sxm2-32gb",
		"NVIDIA A800 (PCIe)":      "a800-pcie",
	} {
		if got := GPUClass(in); got != want {
			t.Errorf("GPUClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	if got := InstanceTypeName("rtx-5090", 8); got != "rtx-5090.x8" {
		t.Errorf("InstanceTypeName = %q", got)
	}
	if got := InstanceTypeName("a", 16); got != "a.x16" {
		t.Errorf("InstanceTypeName = %q", got)
	}
	if got := SharedInstanceTypeName("rtx-5090"); got != "rtx-5090.shared" {
		t.Errorf("SharedInstanceTypeName = %q", got)
	}
	if got := Namespace("project-", "demo"); got != "project-demo" {
		t.Errorf("Namespace = %q", got)
	}
	if got := PoolClusterQueue("project-demo", DefaultPoolName); got != "project-demo" {
		t.Errorf("PoolClusterQueue(default) = %q", got)
	}
	if got := PoolClusterQueue("project-demo", "llm-exclusive"); got != "project-demo-llm-exclusive" {
		t.Errorf("PoolClusterQueue(llm-exclusive) = %q", got)
	}
	if got := PoolResourceFlavor("llm-exclusive", "h100-80gb"); got != "pool-llm-exclusive-h100-80gb" {
		t.Errorf("PoolResourceFlavor = %q", got)
	}
	if got := MemberRoleBinding("Carol@Example.com"); got != "project-member-carol-example-com" {
		t.Errorf("MemberRoleBinding = %q", got)
	}
	if got := PublicDatasetConfigMap("imagenet-1k"); got != "dataset-imagenet-1k" {
		t.Errorf("PublicDatasetConfigMap = %q", got)
	}
}
