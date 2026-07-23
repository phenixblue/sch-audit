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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// SchedulingOutcome is the terminal result of a scheduling attempt for a Pod.
// +kubebuilder:validation:Enum=Scheduled;FailedScheduling;Preempted
type SchedulingOutcome string

const (
	// SchedulingOutcomeScheduled means the pod was bound to a node.
	SchedulingOutcomeScheduled SchedulingOutcome = "Scheduled"
	// SchedulingOutcomeFailedScheduling means the scheduler could not find a node for the pod.
	SchedulingOutcomeFailedScheduling SchedulingOutcome = "FailedScheduling"
	// SchedulingOutcomePreempted means a lower-priority pod was evicted to make room for this pod.
	SchedulingOutcomePreempted SchedulingOutcome = "Preempted"
)

// CandidateNode captures the fate of a single node considered during a
// scheduling attempt. Score is only populated by Tier 2 (scheduling
// framework hook) capture; Tier 1 reconstruction from Events cannot see
// per-node scores for nodes that passed filtering.
type CandidateNode struct {
	// name is the node name.
	// +required
	Name string `json:"name"`

	// filterResult is the predicate-failure string reported for this node,
	// if the node was rejected during filtering.
	// +optional
	FilterResult string `json:"filterResult,omitempty"`

	// score is the node's score at decision time. Only populated by Tier 2
	// capture.
	// +optional
	Score *int64 `json:"score,omitempty"`
}

// VolumeContext resolves the volume plumbing behind a pod's placement:
// which PVC, StorageClass, and provisioner drove binding and topology
// constraints for this scheduling decision.
type VolumeContext struct {
	// pvcName is the name of the PersistentVolumeClaim that influenced
	// placement.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// storageClass is the StorageClass backing the PVC.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// driverType is the provisioner mapped to a human-readable driver label
	// (e.g. FADA, PX-CSI, vsphere-csi).
	// +optional
	DriverType string `json:"driverType,omitempty"`

	// bindingMode is the StorageClass's VolumeBindingMode (Immediate or
	// WaitForFirstConsumer).
	// +optional
	BindingMode string `json:"bindingMode,omitempty"`

	// topologyConstraint summarizes any topology restriction that shaped
	// node selection (e.g. an allowedTopologies term or a WaitForFirstConsumer
	// topology key).
	// +optional
	TopologyConstraint string `json:"topologyConstraint,omitempty"`
}

// SourceRef links a SchedulingDecision transition back to the Kubernetes
// objects it was reconstructed from, for cross-referencing with other
// observability data.
type SourceRef struct {
	// eventUID is the UID of the Kubernetes Event this transition was
	// derived from, if any.
	// +optional
	EventUID types.UID `json:"eventUID,omitempty"`

	// auditRequestID is the audit-id of the API request that performed the
	// bind, if it could be correlated.
	// +optional
	AuditRequestID string `json:"auditRequestID,omitempty"`
}

// SchedulingDecisionSpec identifies the pod a SchedulingDecision is about and
// the context that's stable for that pod's whole lifetime (the scheduler
// acting on it, its volume plumbing). It's set once when the first
// transition for the pod is observed and never updated afterward — anything
// that can change as scheduling is retried or re-observed (outcome, chosen
// node, reason) lives in status instead, since a scheduler can legitimately
// revise its own outcome for the same pod (e.g. a transient FailedScheduling
// while a PVC is still binding, followed by a Scheduled once it does).
type SchedulingDecisionSpec struct {
	// podName is the name of the pod this decision is about.
	// +required
	PodName string `json:"podName"`

	// podNamespace is the namespace of the pod this decision is about.
	// +required
	PodNamespace string `json:"podNamespace"`

	// podUID is the UID of the pod this decision is about. Used to derive
	// the SchedulingDecision's name, so a given pod maps to at most one
	// SchedulingDecision.
	// +required
	PodUID types.UID `json:"podUID"`

	// schedulerName is the scheduler acting on this pod (e.g.
	// default-scheduler, stork), taken from the reporting component of the
	// source Event.
	// +optional
	SchedulerName string `json:"schedulerName,omitempty"`

	// volumeContext describes the volume plumbing that influences
	// placement, if the pod references a PVC.
	// +optional
	VolumeContext *VolumeContext `json:"volumeContext,omitempty"`
}

// SchedulingTransition records a single observed scheduling outcome for a
// pod, in the order the reconciler observed it. A retry loop (e.g.
// FailedScheduling while a PVC binds, followed by Scheduled) or a preemption
// after an earlier successful schedule both show up as separate entries.
type SchedulingTransition struct {
	// outcome is the result of this scheduling attempt.
	// +required
	Outcome SchedulingOutcome `json:"outcome"`

	// chosenNode is the node the pod was bound to. Empty for
	// FailedScheduling outcomes.
	// +optional
	ChosenNode string `json:"chosenNode,omitempty"`

	// reasonSummary is a short human-readable explanation of the outcome,
	// taken from the source Event message (e.g. the predicate-failure
	// string for FailedScheduling).
	// +optional
	ReasonSummary string `json:"reasonSummary,omitempty"`

	// decisionTimestamp is when this transition was observed.
	// +required
	DecisionTimestamp metav1.Time `json:"decisionTimestamp"`

	// schedulingLatencyMs is the time in milliseconds between pod creation
	// and this transition.
	// +optional
	SchedulingLatencyMs int64 `json:"schedulingLatencyMs,omitempty"`

	// candidateNodes lists nodes considered during this scheduling attempt.
	// Populated with the rejected-node/reason pairs available from Tier 1
	// reconstruction; scores are Tier 2 only.
	// +optional
	// +listType=atomic
	CandidateNodes []CandidateNode `json:"candidateNodes,omitempty"`

	// sourceRef links this transition back to the Kubernetes objects it was
	// reconstructed from.
	// +optional
	SourceRef SourceRef `json:"sourceRef,omitempty"`
}

// SchedulingDecisionStatus reports the most recently observed scheduling
// outcome for the pod, mirrored from the last entry of transitions, plus the
// full transition history that produced it.
type SchedulingDecisionStatus struct {
	// outcome is the result of the most recently observed transition.
	// +optional
	Outcome SchedulingOutcome `json:"outcome,omitempty"`

	// chosenNode is the node the pod was bound to as of the most recently
	// observed transition. Empty when that transition isn't Scheduled.
	// +optional
	ChosenNode string `json:"chosenNode,omitempty"`

	// reasonSummary is the reasonSummary of the most recently observed
	// transition.
	// +optional
	ReasonSummary string `json:"reasonSummary,omitempty"`

	// decisionTimestamp is the decisionTimestamp of the most recently
	// observed transition.
	// +optional
	DecisionTimestamp metav1.Time `json:"decisionTimestamp,omitempty"`

	// schedulingLatencyMs is the schedulingLatencyMs of the most recently
	// observed transition.
	// +optional
	SchedulingLatencyMs int64 `json:"schedulingLatencyMs,omitempty"`

	// transitions is the ordered history of every scheduling outcome
	// observed for this pod, oldest first.
	// +optional
	// +listType=atomic
	Transitions []SchedulingTransition `json:"transitions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sdec
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="Scheduler",type=string,JSONPath=`.spec.schedulerName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.chosenNode`
// +kubebuilder:printcolumn:name="VolumeDriver",type=string,JSONPath=`.spec.volumeContext.driverType`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.outcome`
// +kubebuilder:printcolumn:name="LatencyMs",type=integer,JSONPath=`.status.schedulingLatencyMs`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SchedulingDecision is a durable, queryable record of Kubernetes scheduling
// activity for a single pod. It is cluster-scoped; spec identifies the pod
// and is set once, while status holds the latest observed outcome plus the
// full history of transitions that produced it.
type SchedulingDecision struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the pod this decision is about.
	// +required
	Spec SchedulingDecisionSpec `json:"spec"`

	// status reports the latest observed outcome and its full history.
	// +optional
	Status SchedulingDecisionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SchedulingDecisionList contains a list of SchedulingDecision
type SchedulingDecisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SchedulingDecision `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SchedulingDecision{}, &SchedulingDecisionList{})
		return nil
	})
}
