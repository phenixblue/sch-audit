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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// eventInvolvedObjectIndex indexes core/v1 Events by the namespaced name of
// the Pod they refer to, so a Reconcile can cheaply look up the Events for
// its Pod without a namespace-wide List on every call.
const eventInvolvedObjectIndex = "involvedObject.nsName"

// podUIDLabel is set on every SchedulingDecision this controller creates and
// doubles as the idempotency key: a decision is created at most once per Pod
// UID, regardless of how many times the underlying Pod/Event objects are
// reconciled.
const podUIDLabel = "scheduling.purestorage.io/pod-uid"

// Event reason and involvedObject.kind values this controller looks for.
const (
	eventReasonScheduled        = "Scheduled"
	eventReasonFailedScheduling = "FailedScheduling"
	eventReasonPreempted        = "Preempted"
	podKind                     = "Pod"
)

// relevantEventReasons are the only core/v1 Event reasons this controller
// cares about; every other Event reason is ignored by the watch predicate
// and the index-backed List in listRelevantEvents.
var relevantEventReasons = map[string]struct{}{
	eventReasonScheduled:        {},
	eventReasonFailedScheduling: {},
	eventReasonPreempted:        {},
}

// SchedulingDecisionReconciler reconstructs SchedulingDecision records from
// Pod and Event informers (Tier 1). It reconciles Pods rather than
// SchedulingDecisions: a SchedulingDecision is a write-once log entry
// produced as a side effect, not a reconciled object.
type SchedulingDecisionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list

// Reconcile observes a single Pod's scheduling outcome and, the first time
// enough information is available to describe it, creates a corresponding
// SchedulingDecision. It is idempotent: once a decision exists for a Pod's
// UID, further reconciles of that Pod are no-ops.
func (r *SchedulingDecisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	events, err := r.listRelevantEvents(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing events for pod %s: %w", req.NamespacedName, err)
	}

	var pod corev1.Pod
	podFound := true
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting pod %s: %w", req.NamespacedName, err)
		}
		podFound = false
	}

	decision, ok := buildDecision(&pod, podFound, events)
	if !ok {
		// Not enough information yet (e.g. the pod hasn't been scheduled or
		// failed scheduling, and no relevant Event has arrived). A later
		// reconcile, triggered by the Pod's status changing or a relevant
		// Event arriving, will pick this back up.
		return ctrl.Result{}, nil
	}

	if podFound {
		volCtx, err := r.resolveVolumeContext(ctx, &pod)
		if err != nil {
			log.Error(err, "resolving volume context", "pod", req.NamespacedName)
		} else {
			decision.Spec.VolumeContext = volCtx
		}
	}

	if err := r.createIfAbsent(ctx, decision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// createIfAbsent creates decision unless a SchedulingDecision already exists
// for the same Pod UID. Decision names are deterministic
// (sdec-<pod-uid>), so this check-then-create is race-safe: a concurrent
// Create from another reconcile of the same Pod UID fails with
// AlreadyExists, which is treated as success.
func (r *SchedulingDecisionReconciler) createIfAbsent(
	ctx context.Context, decision *schedulingv1alpha1.SchedulingDecision,
) error {
	existing := &schedulingv1alpha1.SchedulingDecision{}
	err := r.Get(ctx, types.NamespacedName{Name: decision.Name}, existing)
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		// fall through to create
	default:
		return fmt.Errorf("checking for existing SchedulingDecision %s: %w", decision.Name, err)
	}

	if err := r.Create(ctx, decision); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating SchedulingDecision %s: %w", decision.Name, err)
	}
	return nil
}

// listRelevantEvents returns the Events for podKey whose reason this
// controller understands, using the involvedObject.nsName field index so
// the List is scoped to a single Pod rather than the whole namespace.
func (r *SchedulingDecisionReconciler) listRelevantEvents(
	ctx context.Context, podKey types.NamespacedName,
) ([]corev1.Event, error) {
	var list corev1.EventList
	if err := r.List(ctx, &list,
		client.InNamespace(podKey.Namespace),
		client.MatchingFields{eventInvolvedObjectIndex: podKey.Namespace + "/" + podKey.Name},
	); err != nil {
		return nil, err
	}

	relevant := make([]corev1.Event, 0, len(list.Items))
	for _, e := range list.Items {
		if _, ok := relevantEventReasons[e.Reason]; ok {
			relevant = append(relevant, e)
		}
	}
	return relevant, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SchedulingDecisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, eventInvolvedObjectIndex,
		func(obj client.Object) []string {
			event, ok := obj.(*corev1.Event)
			if !ok || event.InvolvedObject.Kind != podKind {
				return nil
			}
			return []string{event.InvolvedObject.Namespace + "/" + event.InvolvedObject.Name}
		},
	); err != nil {
		return fmt.Errorf("indexing event involvedObject: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Watches(
			&corev1.Event{},
			handler.EnqueueRequestsFromMapFunc(mapEventToPodRequest),
			builder.WithPredicates(relevantEventPredicate()),
		).
		Named("schedulingdecision").
		Complete(r)
}

// mapEventToPodRequest enqueues a reconcile for the Pod an Event refers to,
// so Preempted/FailedScheduling/Scheduled Events (which don't necessarily
// coincide with a Pod update the informer would otherwise catch) still
// trigger a reconcile.
func mapEventToPodRequest(_ context.Context, obj client.Object) []reconcile.Request {
	event, ok := obj.(*corev1.Event)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: event.InvolvedObject.Namespace,
			Name:      event.InvolvedObject.Name,
		},
	}}
}

// relevantEventPredicate restricts the Event watch to Pod-involving Events
// with a reason this controller acts on.
func relevantEventPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		event, ok := obj.(*corev1.Event)
		if !ok || event.InvolvedObject.Kind != podKind {
			return false
		}
		_, ok = relevantEventReasons[event.Reason]
		return ok
	})
}
