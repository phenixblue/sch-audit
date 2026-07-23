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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// podIdentity is the subset of a SchedulingDecision's spec that identifies
// the pod it's about. It's derived once, from whichever transition is
// observed first for a pod, and never recomputed afterward.
type podIdentity struct {
	UID           types.UID
	Name          string
	Namespace     string
	SchedulerName string
}

// deriveTransition inspects the current Pod and its Events to determine the
// pod's identity and the latest SchedulingTransition they imply. ok is false
// when there isn't yet enough information to describe a transition (the pod
// is still pending with no failure or Preempted Event); a later reconcile,
// triggered by the Pod's status changing or a relevant Event arriving, picks
// this back up.
//
// candidateNodes (per-node filter results and Tier 2 scores) is
// intentionally left unpopulated here: the default scheduler's
// FailedScheduling Event carries a single aggregate message across all
// nodes, not a per-node breakdown, so reasonSummary is the highest-fidelity
// Tier 1 signal available. Real per-node data is a Tier 2 (scheduling
// framework hook) concern.
func deriveTransition(
	pod *corev1.Pod, podFound bool, events []corev1.Event,
) (podIdentity, *schedulingv1alpha1.SchedulingTransition, bool) {
	// Preemption has no Pod-status equivalent, and can be the only signal
	// left once a victim Pod is terminated - so it's checked first and
	// independently of whether the Pod still exists.
	if preempted := latestEventWithReason(events, eventReasonPreempted); preempted != nil {
		return identityAndTransitionFromPreemption(pod, podFound, preempted)
	}

	if !podFound {
		// Without a live Pod, only a Preempted Event (handled above) carries
		// enough context (it retains the involved object's identity) to
		// reconstruct a transition. Scheduled/FailedScheduling need pod.Spec
		// for scheduler/volume attribution.
		return podIdentity{}, nil, false
	}

	if cond := podScheduledCondition(pod); cond != nil {
		switch cond.Status {
		case corev1.ConditionTrue:
			evt := latestEventWithReason(events, eventReasonScheduled)
			return identityFromPod(pod, evt), transitionFromScheduled(pod, cond, evt), true
		case corev1.ConditionFalse:
			if cond.Reason == "Unschedulable" {
				evt := latestEventWithReason(events, eventReasonFailedScheduling)
				return identityFromPod(pod, evt), transitionFromFailedScheduling(pod, cond, evt), true
			}
		case corev1.ConditionUnknown:
			// Fall through to the Event-only fallback below.
		}
	}

	// The PodScheduled condition hasn't synced to the cache yet (or wasn't
	// conclusive); fall back to whatever the Event informer already has.
	if evt := latestEventWithReason(events, eventReasonScheduled); evt != nil {
		return identityFromPod(pod, evt), transitionFromScheduled(pod, nil, evt), true
	}
	if evt := latestEventWithReason(events, eventReasonFailedScheduling); evt != nil {
		return identityFromPod(pod, evt), transitionFromFailedScheduling(pod, nil, evt), true
	}

	return podIdentity{}, nil, false
}

func identityFromPod(pod *corev1.Pod, event *corev1.Event) podIdentity {
	return podIdentity{
		UID:           pod.UID,
		Name:          pod.Name,
		Namespace:     pod.Namespace,
		SchedulerName: schedulerNameFor(event, pod, true),
	}
}

func identityAndTransitionFromPreemption(
	pod *corev1.Pod, podFound bool, event *corev1.Event,
) (podIdentity, *schedulingv1alpha1.SchedulingTransition, bool) {
	ts := metav1.NewTime(eventTimestamp(*event))

	identity := podIdentity{SchedulerName: schedulerNameFor(event, pod, podFound)}
	if podFound {
		identity.UID, identity.Name, identity.Namespace = pod.UID, pod.Name, pod.Namespace
	} else {
		identity.UID = event.InvolvedObject.UID
		identity.Name = event.InvolvedObject.Name
		identity.Namespace = event.InvolvedObject.Namespace
	}

	t := newTransition(schedulingv1alpha1.SchedulingOutcomePreempted, ts)
	t.ReasonSummary = event.Message
	t.SourceRef.EventUID = event.UID
	if podFound {
		t.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	}
	return identity, t, true
}

func transitionFromScheduled(
	pod *corev1.Pod, cond *corev1.PodCondition, event *corev1.Event,
) *schedulingv1alpha1.SchedulingTransition {
	ts := decisionTimestamp(cond, event)

	t := newTransition(schedulingv1alpha1.SchedulingOutcomeScheduled, ts)
	t.ChosenNode = pod.Spec.NodeName
	t.ReasonSummary = firstNonEmpty(conditionMessage(cond), eventMessage(event))
	if event != nil {
		t.SourceRef.EventUID = event.UID
	}
	t.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	return t
}

func transitionFromFailedScheduling(
	pod *corev1.Pod, cond *corev1.PodCondition, event *corev1.Event,
) *schedulingv1alpha1.SchedulingTransition {
	ts := decisionTimestamp(cond, event)

	t := newTransition(schedulingv1alpha1.SchedulingOutcomeFailedScheduling, ts)
	t.ReasonSummary = firstNonEmpty(conditionMessage(cond), eventMessage(event))
	if event != nil {
		t.SourceRef.EventUID = event.UID
	}
	t.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	return t
}

func newTransition(
	outcome schedulingv1alpha1.SchedulingOutcome, ts metav1.Time,
) *schedulingv1alpha1.SchedulingTransition {
	return &schedulingv1alpha1.SchedulingTransition{
		Outcome:           outcome,
		DecisionTimestamp: ts,
	}
}

func decisionName(podUID types.UID) string {
	return fmt.Sprintf("sdec-%s", podUID)
}

// applyTransition appends transition to status's history and mirrors it
// into status's "latest observed outcome" fields.
func applyTransition(
	status *schedulingv1alpha1.SchedulingDecisionStatus, transition *schedulingv1alpha1.SchedulingTransition,
) {
	status.Transitions = append(status.Transitions, *transition)
	status.Outcome = transition.Outcome
	status.ChosenNode = transition.ChosenNode
	status.ReasonSummary = transition.ReasonSummary
	status.DecisionTimestamp = transition.DecisionTimestamp
	status.SchedulingLatencyMs = transition.SchedulingLatencyMs
}

// transitionsEqual reports whether two transitions represent the same
// observed outcome, so a reconcile that re-derives an already-recorded
// transition (e.g. triggered by an unrelated pod update) is a no-op instead
// of appending a duplicate.
func transitionsEqual(a, b schedulingv1alpha1.SchedulingTransition) bool {
	return a.Outcome == b.Outcome &&
		a.ChosenNode == b.ChosenNode &&
		a.ReasonSummary == b.ReasonSummary &&
		a.DecisionTimestamp.Equal(&b.DecisionTimestamp)
}

// schedulerNameFor prefers the reporting scheduler recorded on the Event
// (the actual component that acted), falling back to the Pod's requested
// scheduler name, which is always present (defaulting to
// "default-scheduler") but doesn't distinguish an extender-style scheduler
// from the component that ultimately reports on its behalf.
func schedulerNameFor(event *corev1.Event, pod *corev1.Pod, podFound bool) string {
	if event != nil {
		if event.ReportingController != "" {
			return event.ReportingController
		}
		if event.Source.Component != "" {
			return event.Source.Component
		}
	}
	if podFound && pod.Spec.SchedulerName != "" {
		return pod.Spec.SchedulerName
	}
	return ""
}

func podScheduledCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodScheduled {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func latestEventWithReason(events []corev1.Event, reason string) *corev1.Event {
	var latest *corev1.Event
	for i := range events {
		if events[i].Reason != reason {
			continue
		}
		if latest == nil || eventTimestamp(events[i]).After(eventTimestamp(*latest)) {
			latest = &events[i]
		}
	}
	return latest
}

func eventTimestamp(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// decisionTimestamp prefers the PodScheduled condition's transition time -
// it's the moment the outcome actually became true on the Pod - falling
// back to the Event's timestamp when the condition isn't available yet.
func decisionTimestamp(cond *corev1.PodCondition, event *corev1.Event) metav1.Time {
	if cond != nil && !cond.LastTransitionTime.IsZero() {
		return cond.LastTransitionTime
	}
	if event != nil {
		return metav1.NewTime(eventTimestamp(*event))
	}
	return metav1.Now()
}

func conditionMessage(cond *corev1.PodCondition) string {
	if cond == nil {
		return ""
	}
	return cond.Message
}

func eventMessage(event *corev1.Event) string {
	if event == nil {
		return ""
	}
	return event.Message
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func latencyMs(start, end time.Time) int64 {
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}
