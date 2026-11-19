// Package metrics registers Prometheus gauges for ResourcePool capacity,
// launchable availability and Project quotas.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "github.com/wutz/workspace/api/v1alpha1"
)

// Label names shared by the pool gauges.
const (
	labelPool     = "pool"
	labelGPUClass = "gpu_class"
	labelMode     = "mode"
	labelProject  = "project"
)

var (
	poolLabels       = []string{labelPool, labelGPUClass, labelMode}
	launchableLabels = []string{labelPool, "instance_type"}
	quotaLabels      = []string{labelProject, labelPool, labelGPUClass}
	projectLabels    = []string{labelProject}
)

var (
	poolGPUTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_resourcepool_gpu_total",
		Help: "Total schedulable GPUs in a ResourcePool by GPU class and mode.",
	}, poolLabels)

	poolGPUAllocated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_resourcepool_gpu_allocated",
		Help: "Allocated GPUs in a ResourcePool by GPU class and mode.",
	}, poolLabels)

	poolGPUFree = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_resourcepool_gpu_free",
		Help: "Free schedulable GPUs in a ResourcePool by GPU class and mode.",
	}, poolLabels)

	poolLaunchable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_resourcepool_launchable_instances",
		Help: "Fragmentation-aware launchable instance count in a ResourcePool.",
	}, launchableLabels)

	projectGPUQuota = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_project_gpu_quota",
		Help: "Configured GPU quota for a Project by ResourcePool and GPU class.",
	}, quotaLabels)

	projectReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_project_ready",
		Help: "Whether a Project is Ready (1) or not (0).",
	}, projectLabels)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		poolGPUTotal,
		poolGPUAllocated,
		poolGPUFree,
		poolLaunchable,
		projectGPUQuota,
		projectReady,
	)
}

// ObservePools refreshes all ResourcePool capacity and launchable gauges.
func ObservePools(pools map[string]v1.ResourcePoolStatus, modes map[string]v1.ResourcePoolMode) {
	poolGPUTotal.Reset()
	poolGPUAllocated.Reset()
	poolGPUFree.Reset()
	poolLaunchable.Reset()
	for name, st := range pools {
		mode := string(modes[name])
		for _, g := range st.GPUs {
			poolGPUTotal.WithLabelValues(name, g.Class, mode).Set(float64(g.Total))
			poolGPUAllocated.WithLabelValues(name, g.Class, mode).Set(float64(g.Allocated))
			poolGPUFree.WithLabelValues(name, g.Class, mode).Set(float64(g.Free))
		}
		for _, l := range st.Launchable {
			poolLaunchable.WithLabelValues(name, l.InstanceType).Set(float64(l.Count))
		}
	}
}

// ObserveProject records readiness and per-pool GPU quotas for one Project.
func ObserveProject(project string, ready bool, quotas map[string]map[string]int64) {
	v := 0.0
	if ready {
		v = 1.0
	}
	projectReady.WithLabelValues(project).Set(v)
	projectGPUQuota.DeletePartialMatch(prometheus.Labels{labelProject: project})
	for pool, byClass := range quotas {
		for cls, count := range byClass {
			projectGPUQuota.WithLabelValues(project, pool, cls).Set(float64(count))
		}
	}
}

// DeleteProject removes metrics for a deleted Project.
func DeleteProject(project string) {
	projectReady.DeleteLabelValues(project)
	projectGPUQuota.DeletePartialMatch(prometheus.Labels{labelProject: project})
}
