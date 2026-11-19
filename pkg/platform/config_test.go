package platform

import (
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Ingress.URL("x") != "https://x." {
		t.Errorf("port 443 must be omitted: %s", c.Ingress.URL("x"))
	}
	if c.DefaultQuota.GPUCount("any") != 8 {
		t.Errorf("defaults: %+v", c.DefaultQuota)
	}
	if c.InstanceTypes.Reserved.CPU.String() != "2" {
		t.Errorf("defaults: %+v", c.InstanceTypes)
	}
	if c.Network.SandboxNamespace != "agent-sandbox-system" || c.Registry.AdminSecret != "project-registry-admin" {
		t.Errorf("defaults: %+v", c.Network)
	}
}

func TestGatewayURL(t *testing.T) {
	for _, tc := range []struct {
		scheme string
		port   int
		want   string
	}{
		{"http", 8080, "http://web.example.com:8080"},
		{"http", 80, "http://web.example.com"},
		{"https", 443, "https://web.example.com"},
		{"https", 8443, "https://web.example.com:8443"},
	} {
		g := Gateway{Name: "gw", Scheme: tc.scheme, Port: tc.port}
		if got := g.URL("web.example.com"); got != tc.want {
			t.Errorf("URL(%s:%d) = %q, want %q", tc.scheme, tc.port, got, tc.want)
		}
	}
}

func TestExtendedConfigParsing(t *testing.T) {
	c, err := Parse([]byte(`
registry:
  domain: cr.example.com
  publicProjects: [base-images, public-models]
defaultQuota:
  registryStorage: 100Gi
network:
  allowedEgressNamespaces: [harbor, minio]
  allowedEgressCIDRs: [10.20.0.0/16, 10.96.0.0/12]
storage:
  sharedVolume:
    enabled: true
    defaultSize: 500Gi
    storageClassName: ceph-fs
  publicDatasets:
    - name: imagenet
      storageClassName: nfs-ro
      defaultMountPath: /datasets/imagenet
      allowedProjects: [demo]
  defaultTrashRetention: 72h
spot:
  defaultMaxQueueDuration: 6h
  defaultMaxRequeueCount: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Registry.PublicProjects) != 2 || c.Registry.PublicProjects[0] != "base-images" {
		t.Errorf("publicProjects = %+v", c.Registry.PublicProjects)
	}
	if c.DefaultQuota.RegistryStorage == nil || c.DefaultQuota.RegistryStorage.String() != "100Gi" {
		t.Errorf("default registryStorage = %v", c.DefaultQuota.RegistryStorage)
	}
	if len(c.Network.AllowedEgressNamespaces) != 2 || len(c.Network.AllowedEgressCIDRs) != 2 || c.Network.AllowedEgressCIDRs[0] != "10.20.0.0/16" {
		t.Errorf("network = %+v", c.Network)
	}
	if !c.Storage.SharedVolume.Enabled || c.Storage.SharedVolume.DefaultSize.String() != "500Gi" {
		t.Errorf("sharedVolume = %+v", c.Storage.SharedVolume)
	}
	if len(c.Storage.PublicDatasets) != 1 || c.Storage.PublicDatasets[0].DefaultMountPath != "/datasets/imagenet" {
		t.Errorf("publicDatasets = %+v", c.Storage.PublicDatasets)
	}
	if c.Spot.MaxRequeueCount() != 3 || c.Spot.DefaultMaxQueueDuration != "6h" {
		t.Errorf("spot = %+v", c.Spot)
	}
}
