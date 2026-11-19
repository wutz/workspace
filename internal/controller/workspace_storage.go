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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/wutz/workspace/api/v1alpha1"
	"github.com/wutz/workspace/pkg/naming"
	"github.com/wutz/workspace/pkg/platform"
)

type datasetAccessError struct {
	reason string
	msg    string
}

func (e *datasetAccessError) Error() string { return e.msg }

func trashRetentionValue(p *v1.Project, cfg *platform.Config) string {
	if p.Spec.TrashRetention != nil && p.Spec.TrashRetention.Duration > 0 {
		return p.Spec.TrashRetention.Duration.String()
	}
	return cfg.Storage.DefaultTrashRetention
}

// reconcileSharedVolume creates the project's default RWX shared PVC when enabled
// by either the Project spec or the platform default.
func (r *ProjectReconciler) reconcileSharedVolume(
	ctx context.Context, p *v1.Project, cfg *platform.Config, ns string,
) (string, error) {
	enabled := cfg.Storage.SharedVolume.Enabled
	if p.Spec.SharedVolume != nil && p.Spec.SharedVolume.Enabled != nil {
		enabled = *p.Spec.SharedVolume.Enabled
	}
	if !enabled {
		return "", nil
	}

	size := cfg.Storage.SharedVolume.DefaultSize
	if p.Spec.SharedVolume != nil && p.Spec.SharedVolume.Size != nil {
		size = *p.Spec.SharedVolume.Size
	}
	sc := cfg.Storage.SharedVolume.StorageClassName
	if p.Spec.SharedVolume != nil && p.Spec.SharedVolume.StorageClassName != "" {
		sc = p.Spec.SharedVolume.StorageClassName
	}

	cur := &corev1.PersistentVolumeClaim{}
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: naming.SharedVolumeName}, cur)
	if err == nil {
		// Existing PVC: keep immutable spec fields untouched; only allow size expansion.
		curSize := cur.Spec.Resources.Requests[corev1.ResourceStorage]
		if size.Cmp(curSize) > 0 {
			orig := cur.DeepCopy()
			cur.Spec.Resources.Requests[corev1.ResourceStorage] = size
			if err := r.Patch(ctx, cur, client.MergeFrom(orig)); err != nil {
				return "", fmt.Errorf("expand shared PVC: %w", err)
			}
		}
		return naming.SharedVolumeName, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	labels := projectLabels(p)
	labels[naming.LabelSharedVolume] = valueTrue
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.SharedVolumeName,
			Namespace: ns,
			Labels:    labels,
			Annotations: map[string]string{
				naming.AnnotationTrashRetention: trashRetentionValue(p, cfg),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create shared PVC: %w", err)
	}
	return naming.SharedVolumeName, nil
}

// reconcilePublicDatasets validates access and projects platform-level read-only
// dataset/model descriptors as ConfigMaps in the project namespace.
func (r *ProjectReconciler) reconcilePublicDatasets(
	ctx context.Context, p *v1.Project, cfg *platform.Config, ns string,
) ([]string, error) {
	catalog := make(map[string]platform.PublicDatasetConfig, len(cfg.Storage.PublicDatasets))
	for _, ds := range cfg.Storage.PublicDatasets {
		catalog[ds.Name] = ds
	}

	wantCMs := map[string]bool{}
	projected := make([]string, 0, len(p.Spec.PublicDatasets))
	for _, ref := range p.Spec.PublicDatasets {
		ds, ok := catalog[ref.Name]
		if !ok {
			return nil, &datasetAccessError{
				reason: "DatasetNotFound",
				msg:    fmt.Sprintf("public dataset %s is not defined in platform config", ref.Name),
			}
		}
		if len(ds.AllowedProjects) > 0 && !slices.Contains(ds.AllowedProjects, p.Name) {
			return nil, &datasetAccessError{
				reason: "DatasetAccessDenied",
				msg:    fmt.Sprintf("project %s is not allowed to mount public dataset %s", p.Name, ref.Name),
			}
		}
		mountPath := ref.MountPath
		if mountPath == "" {
			mountPath = ds.DefaultMountPath
		}
		cmName := naming.PublicDatasetConfigMap(ref.Name)
		wantCMs[cmName] = true
		labels := projectLabels(p)
		labels[naming.LabelPublicDataset] = ref.Name
		cm := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
				Labels:    labels,
			},
			Data: map[string]string{
				"name":             ds.Name,
				"description":      ds.Description,
				"storageClassName": ds.StorageClassName,
				"volumeHandle":     ds.VolumeHandle,
				"path":             ds.Path,
				"mountPath":        mountPath,
				"readOnly":         valueTrue,
			},
		}
		if err := apply(ctx, r.Client, cm); err != nil {
			return nil, fmt.Errorf("apply public dataset ConfigMap %s: %w", cmName, err)
		}
		projected = append(projected, ref.Name)
	}

	// Prune previously projected datasets that are no longer requested.
	existing := &corev1.ConfigMapList{}
	if err := r.APIReader.List(ctx, existing, client.InNamespace(ns), client.MatchingLabels{
		naming.LabelProject: p.Name,
	}); err != nil {
		return nil, err
	}
	for i := range existing.Items {
		cm := &existing.Items[i]
		if cm.Labels[naming.LabelPublicDataset] == "" {
			continue
		}
		if !wantCMs[cm.Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, cm)); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(projected)
	return projected, nil
}
