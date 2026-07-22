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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// SchedulingDecisionReconciler reconciles a SchedulingDecision object
type SchedulingDecisionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=update;patch;delete

// Reconcile is a scaffold placeholder: it logs that a SchedulingDecision was
// observed. The Tier 1 reconciler (Pod/Event watches, volume-context
// resolution, idempotent SchedulingDecision creation) lands in a follow-up
// milestone; this stub only proves the watch/manager wiring works end to end.
func (r *SchedulingDecisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("observed SchedulingDecision", "name", req.Name)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SchedulingDecisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&schedulingv1alpha1.SchedulingDecision{}).
		Named("schedulingdecision").
		Complete(r)
}
