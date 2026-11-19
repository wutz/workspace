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
	"maps"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/kf"
	"github.com/wutz/workspace/pkg/metrics"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
	"github.com/wutz/workspace/pkg/resourcepool"
)

// resourcePoolKey collapses all cluster-wide node/pool/pod triggers into one
// deterministic reconcile, guaranteeing conflict-free node assignment across pools.
var resourcePoolKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

// ResourcePoolReconciler manages physical node pool labels/taints, idle-node
// transfer guards, pool-scoped Kueue ResourceFlavors and per-pool availability.
type ResourcePoolReconciler struct {
	client.Client
	Config *platform.Loader
	// Kueue is true when Kueue APIs are installed and enabled.
	Kueue bool
	// PriorityClasses is true when Kueue WorkloadPriorityClass API is served.
	PriorityClasses bool
}

// +kubebuilder:rbac:groups=workspace.wutz.dev,resources=resourcepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspace.wutz.dev,resources=resourcepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.wutz.dev,resources=resourcepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspace.wutz.dev,resources=instancetypes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods;namespaces;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=resourceflavors;workloadpriorityclasses,verbs=get;list;watch;create;update;patch;delete

func (r *ResourcePoolReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	cfg, err := r.Config.Get(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureDefaultPool(ctx); err != nil {
		return ctrl.Result{}, err
	}

	pools := &v1.ResourcePoolList{}
	if err := r.List(ctx, pools); err != nil {
		return ctrl.Result{}, err
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods); err != nil {
		return ctrl.Result{}, err
	}
	its := &v1.InstanceTypeList{}
	if err := r.List(ctx, its); err != nil {
		return ctrl.Result{}, err
	}
	managedNS, err := r.managedProjectNamespaces(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	plan := resourcepool.Compute(pools.Items, nodes.Items, pods.Items, its.Items, managedNS, cfg.InstanceTypes)

	// Reconcile Node pool labels and shared-pool taints.
	for i := range nodes.Items {
		n := &nodes.Items[i]
		assign, ok := plan.Nodes[n.Name]
		if !ok {
			continue
		}
		if err := r.syncNode(ctx, n, assign); err != nil {
			return ctrl.Result{}, fmt.Errorf("sync node %s: %w", n.Name, err)
		}
	}

	// Reconcile pool-scoped Kueue ResourceFlavors and standard WorkloadPriorityClasses.
	if r.Kueue {
		for _, res := range plan.Pools {
			for _, f := range res.Flavors {
				if err := apply(ctx, r.Client, poolResourceFlavor(f)); err != nil {
					return ctrl.Result{}, fmt.Errorf("apply ResourceFlavor %s: %w", f.Name, err)
				}
			}
		}
		if r.PriorityClasses {
			for _, pc := range kf.StandardPriorityClasses {
				if err := apply(ctx, r.Client, workloadPriorityClass(pc)); err != nil {
					return ctrl.Result{}, fmt.Errorf("apply WorkloadPriorityClass %s: %w", pc.Name, err)
				}
			}
		}
	}

	// Patch status of each ResourcePool and refresh Prometheus metrics.
	metricStatuses := make(map[string]v1.ResourcePoolStatus, len(pools.Items))
	metricModes := make(map[string]v1.ResourcePoolMode, len(pools.Items))
	for i := range pools.Items {
		p := &pools.Items[i]
		res, ok := plan.Pools[p.Name]
		if !ok {
			continue
		}
		desired := res.Status
		desired.Conditions = slicesCloneConditions(p.Status.Conditions)
		if res.TransferBlocked {
			meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
				Type: v1.ConditionNodeTransferBlocked, Status: metav1.ConditionTrue,
				Reason: "ActiveWorkloads", Message: res.BlockedMessage, ObservedGeneration: p.Generation,
			})
		} else {
			meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
				Type: v1.ConditionNodeTransferBlocked, Status: metav1.ConditionFalse,
				Reason: "Clear", Message: "", ObservedGeneration: p.Generation,
			})
		}
		meta.SetStatusCondition(&desired.Conditions, metav1.Condition{
			Type: v1.ConditionPoolReady, Status: metav1.ConditionTrue,
			Reason: "Ready", Message: "", ObservedGeneration: p.Generation,
		})

		metricStatuses[p.Name] = desired
		metricModes[p.Name] = res.Mode

		if poolStatusChanged(p.Status, desired) {
			orig := p.DeepCopy()
			p.Status = desired
			if err := r.Status().Patch(ctx, p, client.MergeFrom(orig)); err != nil {
				return ctrl.Result{}, err
			}
			if res.TransferBlocked {
				log.Info("Blocked node transfer on ResourcePool", "pool", p.Name, "message", res.BlockedMessage)
			}
		}
	}
	metrics.ObservePools(metricStatuses, metricModes)
	return ctrl.Result{}, nil
}

func (r *ResourcePoolReconciler) ensureDefaultPool(ctx context.Context) error {
	cur := &v1.ResourcePool{}
	err := r.Get(ctx, types.NamespacedName{Name: naming.DefaultPoolName}, cur)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	def := &v1.ResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: naming.DefaultPoolName,
			Labels: map[string]string{
				naming.LabelManagedBy:   naming.ManagedBy,
				naming.LabelDefaultPool: "true",
			},
		},
		Spec: v1.ResourcePoolSpec{
			DisplayName: "Default Resource Pool",
			Mode:        v1.PoolModeExclusive,
			AccessControl: v1.ResourcePoolAccessControl{
				Visibility: v1.VisibilityPublic,
			},
		},
	}
	if err := r.Create(ctx, def); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	logf.FromContext(ctx).Info("Created built-in default ResourcePool", "name", def.Name)
	return nil
}

func (r *ResourcePoolReconciler) managedProjectNamespaces(ctx context.Context) (map[string]bool, error) {
	list := &corev1.NamespaceList{}
	if err := r.List(ctx, list, client.HasLabels{naming.LabelProject}); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = true
	}
	return out, nil
}

func (r *ResourcePoolReconciler) syncNode(ctx context.Context, n *corev1.Node, assign resourcepool.NodeAssignment) error {
	wantMode := resourcepool.ModeLabelValue(assign.Mode)
	labelChanged := n.Labels[naming.LabelPool] != assign.Pool || n.Labels[naming.LabelPoolMode] != wantMode

	hasTaint := false
	for _, t := range n.Spec.Taints {
		if t.Key == naming.TaintSharedPool && t.Effect == corev1.TaintEffectNoSchedule {
			hasTaint = true
			break
		}
	}
	taintChanged := hasTaint != assign.WantSharedTaint
	if !labelChanged && !taintChanged {
		return nil
	}

	orig := n.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[naming.LabelPool] = assign.Pool
	n.Labels[naming.LabelPoolMode] = wantMode

	if taintChanged {
		if assign.WantSharedTaint {
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
				Key:    naming.TaintSharedPool,
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			})
		} else {
			kept := make([]corev1.Taint, 0, len(n.Spec.Taints))
			for _, t := range n.Spec.Taints {
				if t.Key == naming.TaintSharedPool && t.Effect == corev1.TaintEffectNoSchedule {
					continue
				}
				kept = append(kept, t)
			}
			n.Spec.Taints = kept
		}
	}

	if err := r.Patch(ctx, n, client.MergeFrom(orig)); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Updated Node pool assignment", "node", n.Name, "pool", assign.Pool, "mode", wantMode)
	return nil
}

func poolResourceFlavor(f resourcepool.PoolFlavor) *unstructured.Unstructured {
	labels := map[string]any{
		naming.LabelManagedBy: naming.ManagedBy,
		naming.LabelPool:      f.Pool,
		naming.LabelPoolMode:  resourcepool.ModeLabelValue(f.Mode),
	}
	nodeLabels := map[string]any{
		naming.LabelPool: f.Pool,
	}
	if f.GPUClass != "" {
		labels[naming.LabelGPUClass] = f.GPUClass
	}
	if f.GPUProduct != "" {
		nodeLabels[naming.LabelGPUProduct] = f.GPUProduct
	}
	spec := map[string]any{
		"nodeLabels": nodeLabels,
	}
	if f.SharedTaint {
		spec["nodeTaints"] = []any{
			map[string]any{
				"key":    naming.TaintSharedPool,
				"value":  "true",
				"effect": string(corev1.TaintEffectNoSchedule),
			},
		}
		spec["tolerations"] = []any{
			map[string]any{
				"key":      naming.TaintSharedPool,
				"operator": string(corev1.TolerationOpEqual),
				"value":    "true",
				"effect":   string(corev1.TaintEffectNoSchedule),
			},
		}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: map[string]any{
			naming.KeyName:   f.Name,
			naming.KeyLabels: labels,
		},
		naming.KeySpec: spec,
	}}
	u.SetGroupVersionKind(kf.ResourceFlavorGVK)
	return u
}

func workloadPriorityClass(pc kf.PriorityClass) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		naming.KeyMetadata: map[string]any{
			naming.KeyName: pc.Name,
			naming.KeyLabels: map[string]any{
				naming.LabelManagedBy: naming.ManagedBy,
			},
		},
		"value":       int64(pc.Value),
		"description": pc.Description,
	}}
	u.SetGroupVersionKind(kf.WorkloadPriorityClassGVK)
	return u
}

func slicesCloneConditions(in []metav1.Condition) []metav1.Condition {
	if len(in) == 0 {
		return nil
	}
	out := make([]metav1.Condition, len(in))
	copy(out, in)
	return out
}

func poolStatusChanged(a, b v1.ResourcePoolStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.TotalNodes != b.TotalNodes ||
		a.ReadyNodes != b.ReadyNodes ||
		!reflect.DeepEqual(a.GPUs, b.GPUs) ||
		!reflect.DeepEqual(a.Launchable, b.Launchable) ||
		!reflect.DeepEqual(a.Nodes, b.Nodes) ||
		len(a.Conditions) != len(b.Conditions) {
		return true
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status || ac.Reason != bc.Reason ||
			ac.Message != bc.Message || ac.ObservedGeneration != bc.ObservedGeneration {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourcePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{resourcePoolKey}
	})
	nodeChanged := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
			return !maps.Equal(o.Labels, n.Labels) || o.Spec.Unschedulable != n.Spec.Unschedulable ||
				!equality.Semantic.DeepEqual(o.Spec.Taints, n.Spec.Taints) ||
				!equality.Semantic.DeepEqual(o.Status.Allocatable, n.Status.Allocatable) ||
				nodeReady(o) != nodeReady(n)
		},
	}
	podChanged := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return scheduled(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return scheduled(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.(*corev1.Pod), e.ObjectNew.(*corev1.Pod)
			return o.Spec.NodeName != n.Spec.NodeName || o.Status.Phase != n.Status.Phase
		},
	}
	configChanged := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		r.Config.Invalidate()
		return []reconcile.Request{resourcePoolKey}
	})
	isPlatformConfig := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == platform.ConfigMapName
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("resourcepool").
		Watches(&v1.ResourcePool{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{}, all, builder.WithPredicates(nodeChanged)).
		Watches(&corev1.Pod{}, all, builder.WithPredicates(podChanged)).
		Watches(&v1.InstanceType{}, all, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, configChanged, builder.WithPredicates(isPlatformConfig)).
		Complete(r)
}
