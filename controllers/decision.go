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

// buildDecision derives a SchedulingDecision from the current Pod (if it
// still exists) and the Events observed for it. The second return value is
// false when there isn't yet enough information to record a decision (the
// pod is still pending with no failure or Preempted Event), in which case
// the caller should wait for a later reconcile.
//
// candidateNodes (per-node filter results and Tier 2 scores) is
// intentionally left unpopulated here: the default scheduler's
// FailedScheduling Event carries a single aggregate message across all
// nodes, not a per-node breakdown, so reasonSummary is the highest-fidelity
// Tier 1 signal available. Real per-node data is a Tier 2 (scheduling
// framework hook) concern.
func buildDecision(
	pod *corev1.Pod, podFound bool, events []corev1.Event,
) (*schedulingv1alpha1.SchedulingDecision, bool) {
	// Preemption has no Pod-status equivalent, and can be the only signal
	// left once a victim Pod is terminated - so it's checked first and
	// independently of whether the Pod still exists.
	if preempted := latestEventWithReason(events, eventReasonPreempted); preempted != nil {
		return decisionFromPreemption(pod, podFound, preempted), true
	}

	if !podFound {
		// Without a live Pod, only a Preempted Event (handled above) carries
		// enough context (it retains the involved object's identity) to
		// reconstruct a decision. Scheduled/FailedScheduling need pod.Spec
		// for scheduler/volume attribution.
		return nil, false
	}

	if cond := podScheduledCondition(pod); cond != nil {
		switch cond.Status {
		case corev1.ConditionTrue:
			evt := latestEventWithReason(events, eventReasonScheduled)
			return decisionFromScheduled(pod, cond, evt), true
		case corev1.ConditionFalse:
			if cond.Reason == "Unschedulable" {
				evt := latestEventWithReason(events, eventReasonFailedScheduling)
				return decisionFromFailedScheduling(pod, cond, evt), true
			}
		case corev1.ConditionUnknown:
			// Fall through to the Event-only fallback below.
		}
	}

	// The PodScheduled condition hasn't synced to the cache yet (or wasn't
	// conclusive); fall back to whatever the Event informer already has.
	if evt := latestEventWithReason(events, eventReasonScheduled); evt != nil {
		return decisionFromScheduled(pod, nil, evt), true
	}
	if evt := latestEventWithReason(events, eventReasonFailedScheduling); evt != nil {
		return decisionFromFailedScheduling(pod, nil, evt), true
	}

	return nil, false
}

func decisionFromScheduled(
	pod *corev1.Pod, cond *corev1.PodCondition, event *corev1.Event,
) *schedulingv1alpha1.SchedulingDecision {
	ts := decisionTimestamp(cond, event)

	d := newDecision(pod.UID, pod.Name, pod.Namespace, schedulingv1alpha1.SchedulingOutcomeScheduled, ts)
	d.Spec.ChosenNode = pod.Spec.NodeName
	d.Spec.ReasonSummary = firstNonEmpty(conditionMessage(cond), eventMessage(event))
	d.Spec.SchedulerName = schedulerNameFor(event, pod, true)
	if event != nil {
		d.Spec.SourceRef.EventUID = event.UID
	}
	d.Spec.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	return d
}

func decisionFromFailedScheduling(
	pod *corev1.Pod, cond *corev1.PodCondition, event *corev1.Event,
) *schedulingv1alpha1.SchedulingDecision {
	ts := decisionTimestamp(cond, event)

	d := newDecision(pod.UID, pod.Name, pod.Namespace, schedulingv1alpha1.SchedulingOutcomeFailedScheduling, ts)
	d.Spec.ReasonSummary = firstNonEmpty(conditionMessage(cond), eventMessage(event))
	d.Spec.SchedulerName = schedulerNameFor(event, pod, true)
	if event != nil {
		d.Spec.SourceRef.EventUID = event.UID
	}
	d.Spec.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	return d
}

func decisionFromPreemption(
	pod *corev1.Pod, podFound bool, event *corev1.Event,
) *schedulingv1alpha1.SchedulingDecision {
	ts := metav1.NewTime(eventTimestamp(*event))

	var uid types.UID
	var name, namespace string
	if podFound {
		uid, name, namespace = pod.UID, pod.Name, pod.Namespace
	} else {
		uid, name, namespace = event.InvolvedObject.UID, event.InvolvedObject.Name, event.InvolvedObject.Namespace
	}

	d := newDecision(uid, name, namespace, schedulingv1alpha1.SchedulingOutcomePreempted, ts)
	d.Spec.ReasonSummary = event.Message
	d.Spec.SchedulerName = schedulerNameFor(event, pod, podFound)
	d.Spec.SourceRef.EventUID = event.UID
	if podFound {
		d.Spec.SchedulingLatencyMs = latencyMs(pod.CreationTimestamp.Time, ts.Time)
	}
	return d
}

func newDecision(
	podUID types.UID, podName, podNamespace string, outcome schedulingv1alpha1.SchedulingOutcome, ts metav1.Time,
) *schedulingv1alpha1.SchedulingDecision {
	return &schedulingv1alpha1.SchedulingDecision{
		ObjectMeta: metav1.ObjectMeta{
			Name:   decisionName(podUID),
			Labels: map[string]string{podUIDLabel: string(podUID)},
		},
		Spec: schedulingv1alpha1.SchedulingDecisionSpec{
			PodName:           podName,
			PodNamespace:      podNamespace,
			PodUID:            podUID,
			Outcome:           outcome,
			DecisionTimestamp: ts,
		},
	}
}

func decisionName(podUID types.UID) string {
	return fmt.Sprintf("sdec-%s", podUID)
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
