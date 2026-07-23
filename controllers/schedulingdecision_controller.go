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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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

// DefaultRetentionWindow is used when a SchedulingDecisionReconciler's
// RetentionWindow is left zero-valued (e.g. by tests, or any other caller
// that doesn't care about retention), and is also the flag default wired up
// in cmd/manager.
const DefaultRetentionWindow = 72 * time.Hour

// Event reason and involvedObject.kind values this controller looks for.
const (
	eventReasonScheduled        = "Scheduled"
	eventReasonFailedScheduling = "FailedScheduling"
	eventReasonPreempted        = "Preempted"
	podKind                     = "Pod"
)

// eventReasonCandidateNodes is created by the optional cmd/extender
// observer (see api/v1alpha1.CandidateNodesEventReason's doc comment); its
// message is a comma-separated candidate node name list.
const eventReasonCandidateNodes = schedulingv1alpha1.CandidateNodesEventReason

// relevantEventReasons are the only core/v1 Event reasons this controller
// cares about; every other Event reason is ignored by the watch predicate
// and the index-backed List in listRelevantEvents.
var relevantEventReasons = map[string]struct{}{
	eventReasonScheduled:        {},
	eventReasonFailedScheduling: {},
	eventReasonPreempted:        {},
	eventReasonCandidateNodes:   {},
}

// SchedulingDecisionReconciler reconstructs SchedulingDecision records from
// Pod and Event informers (Tier 1). It reconciles Pods rather than
// SchedulingDecisions: a SchedulingDecision is a write-once log entry
// produced as a side effect, not a reconciled object.
type SchedulingDecisionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RetentionWindow is how long a SchedulingDecision is kept before it's
	// eligible for deletion by the retention sweep (cmd/sweep). Zero means
	// DefaultRetentionWindow.
	RetentionWindow time.Duration
}

// retentionWindow returns r.RetentionWindow, falling back to
// DefaultRetentionWindow when it hasn't been set.
func (r *SchedulingDecisionReconciler) retentionWindow() time.Duration {
	if r.RetentionWindow > 0 {
		return r.RetentionWindow
	}
	return DefaultRetentionWindow
}

// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list

// Reconcile observes a single Pod's scheduling outcome and records it as a
// transition on the corresponding SchedulingDecision, creating the decision
// the first time enough information is available to describe one. A
// scheduler's belief about a pod's outcome isn't necessarily terminal the
// first time it's observed (e.g. a transient FailedScheduling while a PVC
// is still binding, followed by a Scheduled once it does), so later
// reconciles that observe a new outcome append to the decision's status
// history instead of leaving the first-ever observation stuck in place.
func (r *SchedulingDecisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	identity, transition, ok := deriveTransition(&pod, podFound, events)
	if !ok {
		// Not enough information yet (e.g. the pod hasn't been scheduled or
		// failed scheduling, and no relevant Event has arrived). A later
		// reconcile, triggered by the Pod's status changing or a relevant
		// Event arriving, will pick this back up.
		return ctrl.Result{}, nil
	}

	if err := r.recordTransition(ctx, identity, transition, podFound, &pod); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// recordTransition makes sure a SchedulingDecision exists for identity.UID,
// then applies transition to its status. Split this way because the two
// steps have different idempotency needs: creation only has to happen once
// per pod, ever, while the status write has to be safe to retry against
// whatever the object's current state actually is.
func (r *SchedulingDecisionReconciler) recordTransition(
	ctx context.Context, identity podIdentity, transition *schedulingv1alpha1.SchedulingTransition,
	podFound bool, pod *corev1.Pod,
) error {
	name := decisionName(identity.UID)
	if err := r.ensureDecisionCreated(ctx, name, identity, podFound, pod); err != nil {
		return err
	}
	return r.appendTransitionWithRetry(ctx, name, transition)
}

// ensureDecisionCreated creates a SchedulingDecision named name with
// identity's stable fields as spec, if one doesn't already exist. Decision
// names are deterministic (sdec-<pod-uid>), so losing a Create race to
// another reconcile of the same Pod UID is expected and treated as success
// (AlreadyExists) - the actual status write always happens afterward, in
// appendTransitionWithRetry, regardless of which path got the decision
// created.
func (r *SchedulingDecisionReconciler) ensureDecisionCreated(
	ctx context.Context, name string, identity podIdentity, podFound bool, pod *corev1.Pod,
) error {
	var existing schedulingv1alpha1.SchedulingDecision
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting SchedulingDecision %s: %w", name, err)
	}

	log := logf.FromContext(ctx)
	expiresAt := time.Now().Add(r.retentionWindow()).Unix()
	decision := &schedulingv1alpha1.SchedulingDecision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				podUIDLabel:                       string(identity.UID),
				schedulingv1alpha1.ExpiresAtLabel: strconv.FormatInt(expiresAt, 10),
			},
		},
		Spec: schedulingv1alpha1.SchedulingDecisionSpec{
			PodName:       identity.Name,
			PodNamespace:  identity.Namespace,
			PodUID:        identity.UID,
			SchedulerName: identity.SchedulerName,
		},
	}

	if podFound {
		volCtx, err := r.resolveVolumeContext(ctx, pod)
		if err != nil {
			log.Error(err, "resolving volume context", "pod", types.NamespacedName{
				Namespace: identity.Namespace, Name: identity.Name,
			})
		} else {
			decision.Spec.VolumeContext = volCtx
		}
	}

	if err := r.Create(ctx, decision); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating SchedulingDecision %s: %w", name, err)
	}
	return nil
}

// appendTransitionWithRetry appends transition to the named decision's
// status history and updates its "latest observed outcome" fields, unless
// transition is identical to the most recently recorded one (a no-op, so
// re-deriving an already-recorded outcome - e.g. from an unrelated Pod
// update - doesn't grow the history forever).
//
// It re-fetches the decision fresh on every attempt rather than reusing a
// caller-supplied copy, and retries on a Conflict: the manager's client
// reads through a local cache, which can still be a step behind a write
// this same reconciler just made - either ensureDecisionCreated's Create
// (the Get below can come back NotFound for an object that was already
// created, if the cache hasn't caught up yet) or this function's own
// previous attempt (Update can come back Conflict against a resourceVersion
// that's already stale). Retrying against a fresh read on either error is
// what makes both cases safe instead of failing the reconcile outright.
func (r *SchedulingDecisionReconciler) appendTransitionWithRetry(
	ctx context.Context, name string, transition *schedulingv1alpha1.SchedulingTransition,
) error {
	retriable := func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsNotFound(err)
	}
	err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
		decision := &schedulingv1alpha1.SchedulingDecision{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, decision); err != nil {
			return err
		}

		if n := len(decision.Status.Transitions); n > 0 && transitionsEqual(decision.Status.Transitions[n-1], *transition) {
			return nil
		}

		applyTransition(&decision.Status, transition)
		return r.Status().Update(ctx, decision)
	})
	if err != nil {
		return fmt.Errorf("updating status for SchedulingDecision %s: %w", name, err)
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
