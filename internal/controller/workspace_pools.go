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

package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/instancetype"
	"github.com/wutz/workspace/pkg/kf"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
	"github.com/wutz/workspace/pkg/resourcepool"
)

type poolAccessError struct {
	reason string
	msg    string
}

func (e *poolAccessError) Error() string { return e.msg }

// queueReconcileResult captures the provisioned queues and authorized pools
// for RBAC, ResourceQuota and metrics reporting.
type queueReconcileResult struct {
	defaultCQ       string
	defaultLQ       string
	clusterQueues   []string
	queues          []v1.QueueStatus
	authorizedPools []string
	gpuQuotasByPool map[string]map[string]int64
}

func (r *ProjectReconciler) listPools(ctx context.Context) (map[string]*v1.ResourcePool, []string, error) {
	list := &v1.ResourcePoolList{}
	if err := r.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return map[string]*v1.ResourcePool{}, nil, nil
		}
		return nil, nil, err
	}
	byName := make(map[string]*v1.ResourcePool, len(list.Items))
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		rp := list.Items[i].DeepCopy()
		byName[rp.Name] = rp
		names = append(names, rp.Name)
	}
	slices.Sort(names)
	return byName, names, nil
}

// resolveAuthorizedPools returns sorted ResourcePool names this project is
// permitted to discover and bind to (all Public pools plus explicitly allowed
// Restricted pools referenced in poolQuotas or allowing this project).
func resolveAuthorizedPools(p *v1.Project, pools map[string]*v1.ResourcePool, poolNames []string) []string {
	out := make([]string, 0, len(poolNames))
	for _, name := range poolNames {
		if resourcepool.ProjectAllowed(pools[name], p.Name) {
			out = append(out, name)
		}
	}
	return out
}

// validatePoolQuotas verifies that every pool referenced in p.Spec.PoolQuotas
// exists and allows this Project.
func validatePoolQuotas(p *v1.Project, pools map[string]*v1.ResourcePool) error {
	for _, pq := range p.Spec.PoolQuotas {
		rp, ok := pools[pq.Pool]
		if !ok {
			return &poolAccessError{
				reason: "PoolNotFound",
				msg:    fmt.Sprintf("resource pool %s does not exist", pq.Pool),
			}
		}
		if !resourcepool.ProjectAllowed(rp, p.Name) {
			return &poolAccessError{
				reason: "PoolAccessDenied",
				msg:    fmt.Sprintf("project %s is not allowed to use restricted resource pool %s", p.Name, pq.Pool),
			}
		}
	}
	return nil
}

func classMap(classes []instancetype.Class) map[string]instancetype.Class {
	m := make(map[string]instancetype.Class, len(classes))
	for _, c := range classes {
		m[c.Name] = c
	}
	return m
}

func workloadNames(ws []v1.WorkloadKind) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, string(w))
	}
	return strings.Join(parts, ",")
}

func poolFlavorEntry(name string, milliCPU, memory, gpus int64, borrow, lend *int32) map[string]any {
	gpuRes := map[string]any{
		naming.KeyName:  string(instancetype.ResourceGPU),
		keyNominalQuota: fmt.Sprint(gpus),
	}
	if borrow != nil {
		gpuRes[keyBorrowingLimit] = fmt.Sprint(*borrow)
	}
	if lend != nil {
		gpuRes["lendingLimit"] = fmt.Sprint(*lend)
	}
	return map[string]any{
		naming.KeyName: name,
		keyResources: []any{
			map[string]any{naming.KeyName: keyCPU, keyNominalQuota: resource.NewMilliQuantity(milliCPU, resource.DecimalSI).String()},
			map[string]any{naming.KeyName: keyMemory, keyNominalQuota: resource.NewQuantity(memory, resource.BinarySI).String()},
			gpuRes,
		},
	}
}

func spotFlavorEntry(name string, milliCPU, memory, borrowGPUs int64) map[string]any {
	return map[string]any{
		naming.KeyName: name,
		keyResources: []any{
			map[string]any{
				naming.KeyName:    keyCPU,
				keyNominalQuota:   "0",
				keyBorrowingLimit: resource.NewMilliQuantity(milliCPU, resource.DecimalSI).String(),
			},
			map[string]any{
				naming.KeyName:    keyMemory,
				keyNominalQuota:   "0",
				keyBorrowingLimit: resource.NewQuantity(memory, resource.BinarySI).String(),
			},
			map[string]any{
				naming.KeyName:    string(instancetype.ResourceGPU),
				keyNominalQuota:   "0",
				keyBorrowingLimit: fmt.Sprint(borrowGPUs),
			},
		},
	}
}

func buildLocalQueue(
	p *v1.Project, ns, name, cqName, pool string, mode v1.ResourcePoolMode, annotations map[string]string,
) *unstructured.Unstructured {
	labels := toAny(projectLabels(p))
	if pool != "" {
		labels[naming.LabelPool] = pool
	}
	if mode != "" {
		labels[naming.LabelPoolMode] = resourcepool.ModeLabelValue(mode)
	}
	metaObj := map[string]any{
		naming.KeyName:      name,
		naming.KeyNamespace: ns,
		naming.KeyLabels:    labels,
	}
	if len(annotations) > 0 {
		metaObj["annotations"] = toAny(annotations)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: metaObj,
		naming.KeySpec:     map[string]any{"clusterQueue": cqName},
	}}
	u.SetGroupVersionKind(kf.LocalQueueGVK)
	return u
}

func buildExclusivePoolClusterQueue(
	p *v1.Project, cfg *platform.Config, ns string, pq v1.PoolQuota, rp *v1.ResourcePool,
	classes []instancetype.Class, includeCPUHeadroom bool,
) (*unstructured.Unstructured, map[string]int64) {
	cqName := naming.PoolClusterQueue(ns, pq.Pool)
	byClass := classMap(classes)
	gpuCounts := map[string]int64{}
	var flavors []any

	// Use GPU classes explicitly declared in pq.GPUs; if empty, derive from pool status GPUs.
	type entry struct {
		class string
		count int64
	}
	var entries []entry
	if len(pq.GPUs) > 0 {
		for _, g := range pq.GPUs {
			entries = append(entries, entry{class: g.Class, count: int64(g.Count)})
		}
	} else if rp != nil {
		for _, g := range rp.Status.GPUs {
			entries = append(entries, entry{class: g.Class, count: int64(cfg.DefaultQuota.GPUCount(g.Class))})
		}
	}
	for _, e := range entries {
		gpuCounts[e.class] = e.count
		c := byClass[e.class]
		flavors = append(flavors, poolFlavorEntry(
			naming.PoolResourceFlavor(pq.Pool, e.class),
			e.count*c.MilliCPU, e.count*c.Memory, e.count,
			pq.BorrowingLimit, pq.LendingLimit,
		))
	}
	if includeCPUHeadroom || len(flavors) == 0 {
		headroom := cfg.DefaultQuota.Headroom
		flavors = append(flavors, flavor(naming.ResourceFlavor(cpuFlavor), headroom.CPU.MilliValue(), headroom.Memory.Value(), 0))
	}

	cohort := naming.PoolCohort(pq.Pool)
	if rp != nil && rp.Spec.SpotPolicy != nil && rp.Spec.SpotPolicy.LendIdleToSpot {
		cohort = naming.SpotCohort
	}

	labels := toAny(projectLabels(p))
	labels[naming.LabelPool] = pq.Pool
	labels[naming.LabelPoolMode] = "exclusive"

	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: map[string]any{
			naming.KeyName:     cqName,
			naming.KeyLabels:   labels,
			keyOwnerReferences: []any{ownerRef(p)},
		},
		naming.KeySpec: map[string]any{
			keyCohortName:        cohort,
			keyNamespaceSelector: map[string]any{keyMatchLabels: map[string]any{corev1.LabelMetadataName: ns}},
			keyQueueingStrategy:  queueingBestEffort,
			"preemption": map[string]any{
				"reclaimWithinCohort": "Any",
				"withinClusterQueue":  "LowerPriority",
			},
			keyResourceGroups: []any{map[string]any{
				keyCoveredResources: []any{keyCPU, keyMemory, string(instancetype.ResourceGPU)},
				keyFlavors:          flavors,
			}},
		},
	}}
	u.SetGroupVersionKind(kf.ClusterQueueGVK)
	return u, gpuCounts
}

func buildSharedPoolClusterQueue(p *v1.Project, ns string, pq v1.PoolQuota) *unstructured.Unstructured {
	cqName := naming.PoolClusterQueue(ns, pq.Pool)
	maxSandboxes := int32(4)
	if pq.MaxSandboxes != nil {
		maxSandboxes = *pq.MaxSandboxes
	}
	labels := toAny(projectLabels(p))
	labels[naming.LabelPool] = pq.Pool
	labels[naming.LabelPoolMode] = naming.PoolModeShared

	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: map[string]any{
			naming.KeyName:     cqName,
			naming.KeyLabels:   labels,
			keyOwnerReferences: []any{ownerRef(p)},
		},
		naming.KeySpec: map[string]any{
			keyCohortName:        naming.PoolCohort(pq.Pool),
			keyNamespaceSelector: map[string]any{keyMatchLabels: map[string]any{corev1.LabelMetadataName: ns}},
			keyQueueingStrategy:  queueingBestEffort,
			keyResourceGroups: []any{map[string]any{
				keyCoveredResources: []any{naming.ResourceSharedSlot},
				keyFlavors: []any{map[string]any{
					naming.KeyName: naming.PoolSharedFlavor(pq.Pool),
					keyResources: []any{map[string]any{
						naming.KeyName:  naming.ResourceSharedSlot,
						keyNominalQuota: strconv.Itoa(int(maxSandboxes)),
					}},
				}},
			}},
		},
	}}
	u.SetGroupVersionKind(kf.ClusterQueueGVK)
	return u
}

func buildSpotClusterQueue(
	p *v1.Project, cfg *platform.Config, ns string,
	lendingPools []*v1.ResourcePool, classes []instancetype.Class,
) (*unstructured.Unstructured, map[string]int64) {
	byClass := classMap(classes)
	spotLimits := map[string]int64{}
	for _, g := range p.Spec.Spot.GPUs {
		spotLimits[g.Class] = int64(g.Count)
	}

	var flavors []any
	recorded := map[string]int64{}
	for _, rp := range lendingPools {
		poolClasses := make([]string, 0, len(rp.Status.GPUs))
		for _, g := range rp.Status.GPUs {
			poolClasses = append(poolClasses, g.Class)
		}
		if len(poolClasses) == 0 {
			for cls := range spotLimits {
				poolClasses = append(poolClasses, cls)
			}
			slices.Sort(poolClasses)
		}
		for _, cls := range poolClasses {
			limit, ok := spotLimits[cls]
			if !ok {
				if len(spotLimits) > 0 {
					continue
				}
				limit = int64(cfg.DefaultQuota.GPUCount(cls))
			}
			if limit <= 0 {
				continue
			}
			c := byClass[cls]
			flavors = append(flavors, spotFlavorEntry(
				naming.PoolResourceFlavor(rp.Name, cls),
				limit*c.MilliCPU, limit*c.Memory, limit,
			))
			recorded[cls] = max(recorded[cls], limit)
		}
	}
	if len(flavors) == 0 {
		// Fall back to default GPU flavors with zero nominal quota so Spot queue remains valid.
		for _, c := range classes {
			limit := spotLimits[c.Name]
			if limit <= 0 && len(spotLimits) == 0 {
				limit = int64(cfg.DefaultQuota.GPUCount(c.Name))
			}
			if limit <= 0 {
				continue
			}
			flavors = append(flavors, spotFlavorEntry(
				naming.ResourceFlavor(c.Name),
				limit*c.MilliCPU, limit*c.Memory, limit,
			))
			recorded[c.Name] = limit
		}
	}
	if len(flavors) == 0 {
		headroom := cfg.DefaultQuota.Headroom
		flavors = append(flavors, spotFlavorEntry(
			naming.ResourceFlavor(cpuFlavor), headroom.CPU.MilliValue(), headroom.Memory.Value(), 0,
		))
	}

	labels := toAny(projectLabels(p))
	labels[naming.LabelPoolMode] = "spot"

	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: map[string]any{
			naming.KeyName:     naming.SpotClusterQueue(ns),
			naming.KeyLabels:   labels,
			keyOwnerReferences: []any{ownerRef(p)},
		},
		naming.KeySpec: map[string]any{
			keyCohortName:        naming.SpotCohort,
			keyNamespaceSelector: map[string]any{keyMatchLabels: map[string]any{corev1.LabelMetadataName: ns}},
			keyQueueingStrategy:  queueingBestEffort,
			"preemption": map[string]any{
				"reclaimWithinCohort": "Never",
				"withinClusterQueue":  "Never",
			},
			keyResourceGroups: []any{map[string]any{
				keyCoveredResources: []any{keyCPU, keyMemory, string(instancetype.ResourceGPU)},
				keyFlavors:          flavors,
			}},
		},
	}}
	u.SetGroupVersionKind(kf.ClusterQueueGVK)
	return u, recorded
}

func poolAnnotations(rp *v1.ResourcePool) map[string]string {
	ann := map[string]string{
		naming.AnnotationAllowedWorkloads: workloadNames(resourcepool.EffectiveAllowedWorkloads(rp)),
	}
	if rp != nil && rp.Spec.Rules != nil && rp.Spec.Rules.IdleShutdown != nil && rp.Spec.Rules.IdleShutdown.Enabled {
		ann[naming.AnnotationIdleShutdownEnabled] = valueTrue
	}
	return ann
}

func spotQueueAnnotations(p *v1.Project, cfg *platform.Config) map[string]string {
	maxWait := cfg.Spot.DefaultMaxQueueDuration
	if p.Spec.Spot != nil && p.Spec.Spot.MaxQueueDuration != nil {
		maxWait = p.Spec.Spot.MaxQueueDuration.Duration.String()
	}
	maxRequeue := cfg.Spot.MaxRequeueCount()
	if p.Spec.Spot != nil && p.Spec.Spot.MaxRequeueCount != nil {
		maxRequeue = *p.Spec.Spot.MaxRequeueCount
	}
	return map[string]string{
		naming.AnnotationSpotMaxQueueDuration: maxWait,
		naming.AnnotationSpotMaxRequeueCount:  strconv.Itoa(int(maxRequeue)),
		naming.AnnotationAllowedWorkloads:     workloadNames([]v1.WorkloadKind{v1.WorkloadSandbox, v1.WorkloadTrainJob, v1.WorkloadInference}),
	}
}

func (r *ProjectReconciler) reconcileQueues(
	ctx context.Context, p *v1.Project, cfg *platform.Config, ns string,
	classes []instancetype.Class, pools map[string]*v1.ResourcePool, poolNames []string,
) (queueReconcileResult, error) {
	res := queueReconcileResult{
		authorizedPools: resolveAuthorizedPools(p, pools, poolNames),
		gpuQuotasByPool: map[string]map[string]int64{},
	}
	if err := validatePoolQuotas(p, pools); err != nil {
		return res, err
	}

	// Compute metric GPU map even when Kueue is disabled.
	if len(p.Spec.PoolQuotas) == 0 {
		byClass := map[string]int64{}
		for _, c := range classes {
			byClass[c.Name] = gpuQuota(p, cfg, c.Name)
		}
		res.gpuQuotasByPool[naming.DefaultPoolName] = byClass
	} else {
		for _, pq := range p.Spec.PoolQuotas {
			byClass := map[string]int64{}
			for _, g := range pq.GPUs {
				byClass[g.Class] = int64(g.Count)
			}
			res.gpuQuotasByPool[pq.Pool] = byClass
		}
	}

	if !r.Kueue {
		return res, nil
	}

	if err := apply(ctx, r.Client, cpuResourceFlavor()); err != nil {
		return res, fmt.Errorf("apply CPU ResourceFlavor: %w", err)
	}

	wantLQs := map[string]bool{}
	wantCQs := map[string]bool{}

	if len(p.Spec.PoolQuotas) == 0 {
		// Backward-compatible path: single default ClusterQueue and LocalQueue.
		cqName := naming.ClusterQueue(ns)
		defPool := pools[naming.DefaultPoolName]
		if err := apply(ctx, r.Client, clusterQueue(p, cfg, ns, classes)); err != nil {
			return res, fmt.Errorf("apply ClusterQueue: %w", err)
		}
		lq := buildLocalQueue(p, ns, naming.LocalQueueName, cqName, naming.DefaultPoolName, v1.PoolModeExclusive, poolAnnotations(defPool))
		if err := apply(ctx, r.Client, lq); err != nil {
			return res, fmt.Errorf("apply LocalQueue: %w", err)
		}
		wantCQs[cqName] = true
		wantLQs[naming.LocalQueueName] = true
		res.defaultCQ = cqName
		res.defaultLQ = naming.LocalQueueName
		res.clusterQueues = append(res.clusterQueues, cqName)
		res.queues = append(res.queues, v1.QueueStatus{
			Name:             naming.LocalQueueName,
			Pool:             naming.DefaultPoolName,
			Mode:             v1.PoolModeExclusive,
			ClusterQueue:     cqName,
			AllowedWorkloads: resourcepool.EffectiveAllowedWorkloads(defPool),
		})
	} else if err := r.applyPoolQueues(ctx, p, cfg, ns, classes, pools, &res, wantLQs, wantCQs); err != nil {
		return res, err
	}

	// Provision preemptible Spot queue when enabled.
	if p.Spec.Spot != nil && p.Spec.Spot.Enabled {
		var lending []*v1.ResourcePool
		for _, name := range poolNames {
			rp := pools[name]
			if resourcepool.EffectiveMode(rp) == v1.PoolModeExclusive &&
				rp.Spec.SpotPolicy != nil && rp.Spec.SpotPolicy.LendIdleToSpot &&
				resourcepool.ProjectAllowed(rp, p.Name) {
				lending = append(lending, rp)
			}
		}
		spotCQ, spotCounts := buildSpotClusterQueue(p, cfg, ns, lending, classes)
		spotCQName := naming.SpotClusterQueue(ns)
		if err := apply(ctx, r.Client, spotCQ); err != nil {
			return res, fmt.Errorf("apply spot ClusterQueue: %w", err)
		}
		spotLQ := buildLocalQueue(p, ns, naming.SpotQueueName, spotCQName, "", v1.PoolModeExclusive, spotQueueAnnotations(p, cfg))
		if err := apply(ctx, r.Client, spotLQ); err != nil {
			return res, fmt.Errorf("apply spot LocalQueue: %w", err)
		}
		wantCQs[spotCQName] = true
		wantLQs[naming.SpotQueueName] = true
		res.clusterQueues = append(res.clusterQueues, spotCQName)
		res.gpuQuotasByPool[naming.SpotQueueName] = spotCounts
		res.queues = append(res.queues, v1.QueueStatus{
			Name:             naming.SpotQueueName,
			ClusterQueue:     spotCQName,
			Spot:             true,
			AllowedWorkloads: []v1.WorkloadKind{v1.WorkloadSandbox, v1.WorkloadTrainJob, v1.WorkloadInference},
		})
	}

	if err := r.pruneStaleQueues(ctx, p, ns, wantLQs, wantCQs); err != nil {
		return res, err
	}
	slices.Sort(res.clusterQueues)
	return res, nil
}

// applyPoolQueues creates one ClusterQueue and LocalQueue per ResourcePool the
// Project is bound to through spec.poolQuotas.
func (r *ProjectReconciler) applyPoolQueues(
	ctx context.Context, p *v1.Project, cfg *platform.Config, ns string,
	classes []instancetype.Class, pools map[string]*v1.ResourcePool,
	res *queueReconcileResult, wantLQs, wantCQs map[string]bool,
) error {
	hasDefaultPool := false
	for _, pq := range p.Spec.PoolQuotas {
		if pq.Pool == naming.DefaultPoolName {
			hasDefaultPool = true
			break
		}
	}
	for i, pq := range p.Spec.PoolQuotas {
		rp := pools[pq.Pool]
		mode := resourcepool.EffectiveMode(rp)
		cqName := naming.PoolClusterQueue(ns, pq.Pool)
		lqName := pq.Pool

		if mode == v1.PoolModeShared {
			cq := buildSharedPoolClusterQueue(p, ns, pq)
			if err := apply(ctx, r.Client, cq); err != nil {
				return fmt.Errorf("apply shared ClusterQueue %s: %w", cqName, err)
			}
		} else {
			includeCPU := pq.Pool == naming.DefaultPoolName || (!hasDefaultPool && i == 0)
			cq, counts := buildExclusivePoolClusterQueue(p, cfg, ns, pq, rp, classes, includeCPU)
			if err := apply(ctx, r.Client, cq); err != nil {
				return fmt.Errorf("apply ClusterQueue %s: %w", cqName, err)
			}
			res.gpuQuotasByPool[pq.Pool] = counts
		}

		lq := buildLocalQueue(p, ns, lqName, cqName, pq.Pool, mode, poolAnnotations(rp))
		if err := apply(ctx, r.Client, lq); err != nil {
			return fmt.Errorf("apply LocalQueue %s: %w", lqName, err)
		}
		wantCQs[cqName] = true
		wantLQs[lqName] = true
		res.clusterQueues = append(res.clusterQueues, cqName)
		res.queues = append(res.queues, v1.QueueStatus{
			Name:             lqName,
			Pool:             pq.Pool,
			Mode:             mode,
			ClusterQueue:     cqName,
			AllowedWorkloads: resourcepool.EffectiveAllowedWorkloads(rp),
		})
		if res.defaultCQ == "" || pq.Pool == naming.DefaultPoolName {
			res.defaultCQ = cqName
			res.defaultLQ = lqName
		}
	}
	return nil
}

func (r *ProjectReconciler) pruneStaleQueues(
	ctx context.Context, p *v1.Project, ns string, wantLQs, wantCQs map[string]bool,
) error {
	sel := client.MatchingLabels{naming.LabelProject: p.Name}
	lqs := &unstructured.UnstructuredList{}
	lqs.SetGroupVersionKind(kf.LocalQueueGVK.GroupVersion().WithKind(kf.LocalQueueGVK.Kind + "List"))
	if err := r.List(ctx, lqs, client.InNamespace(ns), sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range lqs.Items {
		if !wantLQs[lqs.Items[i].GetName()] {
			if err := r.Delete(ctx, &lqs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	cqs := &unstructured.UnstructuredList{}
	cqs.SetGroupVersionKind(kf.ClusterQueueGVK.GroupVersion().WithKind(kf.ClusterQueueGVK.Kind + "List"))
	if err := r.List(ctx, cqs, sel); err != nil && !meta.IsNoMatchError(err) {
		return err
	}
	for i := range cqs.Items {
		if !wantCQs[cqs.Items[i].GetName()] {
			if err := r.Delete(ctx, &cqs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *ProjectReconciler) deleteAllProjectClusterQueues(ctx context.Context, p *v1.Project, ns string) error {
	cqs := &unstructured.UnstructuredList{}
	cqs.SetGroupVersionKind(kf.ClusterQueueGVK.GroupVersion().WithKind(kf.ClusterQueueGVK.Kind + "List"))
	if err := r.List(ctx, cqs, client.MatchingLabels{naming.LabelProject: p.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	for i := range cqs.Items {
		if err := r.Delete(ctx, &cqs.Items[i]); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return err
		}
	}
	// Also delete the default ClusterQueue by name in case it was not yet indexed in cache.
	cq := &unstructured.Unstructured{}
	cq.SetGroupVersionKind(kf.ClusterQueueGVK)
	cq.SetName(naming.ClusterQueue(ns))
	if err := r.Delete(ctx, cq); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return err
	}
	return nil
}
