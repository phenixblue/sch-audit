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

package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// provisionerDriverLabels maps a StorageClass provisioner to the
// human-readable driver label used in the dashboard and printer columns.
// Provisioners not in this table are reported as-is.
var provisionerDriverLabels = map[string]string{
	"csi.purestorage.com":    "FADA",
	"pxd.portworx.com":       "PX-CSI",
	"csi.vsphere.vmware.com": "vsphere-csi",
	"kubernetes.io/aws-ebs":  "aws-ebs",
	"ebs.csi.aws.com":        "aws-ebs-csi",
	"disk.csi.azure.com":     "azure-disk-csi",
	"pd.csi.storage.gke.io":  "gce-pd-csi",
}

// resolveVolumeContext walks the Pod's volumes for the first one backed by
// a PersistentVolumeClaim, then follows PVC -> StorageClass -> provisioner
// to build the volume context. Pods with no PVC-backed volume, or whose PVC
// isn't found, return a nil context rather than an error.
//
// Only the first PVC-backed volume is considered: this covers the common
// case (a single data volume, e.g. one PVC per KubeVirt VM) without trying
// to model multi-PVC pods, which the design doc doesn't call for.
func (r *SchedulingDecisionReconciler) resolveVolumeContext(
	ctx context.Context, pod *corev1.Pod,
) (*schedulingv1alpha1.VolumeContext, error) {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}

		var pvc corev1.PersistentVolumeClaim
		pvcKey := types.NamespacedName{Namespace: pod.Namespace, Name: vol.PersistentVolumeClaim.ClaimName}
		if err := r.Get(ctx, pvcKey, &pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting PVC %s: %w", pvcKey, err)
		}

		return r.volumeContextFromPVC(ctx, &pvc)
	}
	return nil, nil
}

func (r *SchedulingDecisionReconciler) volumeContextFromPVC(
	ctx context.Context, pvc *corev1.PersistentVolumeClaim,
) (*schedulingv1alpha1.VolumeContext, error) {
	volCtx := &schedulingv1alpha1.VolumeContext{PVCName: pvc.Name}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return volCtx, nil
	}
	volCtx.StorageClass = *pvc.Spec.StorageClassName

	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: volCtx.StorageClass}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return volCtx, nil
		}
		return nil, fmt.Errorf("getting StorageClass %s: %w", volCtx.StorageClass, err)
	}

	volCtx.DriverType = driverLabelForProvisioner(sc.Provisioner)
	if sc.VolumeBindingMode != nil {
		volCtx.BindingMode = string(*sc.VolumeBindingMode)
	}
	volCtx.TopologyConstraint = summarizeTopology(sc.AllowedTopologies)
	return volCtx, nil
}

func driverLabelForProvisioner(provisioner string) string {
	if label, ok := provisionerDriverLabels[provisioner]; ok {
		return label
	}
	return provisioner
}

func summarizeTopology(terms []corev1.TopologySelectorTerm) string {
	var parts []string
	for _, term := range terms {
		for _, expr := range term.MatchLabelExpressions {
			parts = append(parts, fmt.Sprintf("%s in (%s)", expr.Key, strings.Join(expr.Values, ",")))
		}
	}
	return strings.Join(parts, "; ")
}
