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

// SourceRef links a SchedulingDecision back to the Kubernetes objects it was
// reconstructed from, for cross-referencing with other observability data.
type SourceRef struct {
	// eventUID is the UID of the Kubernetes Event this decision was derived
	// from, if any.
	// +optional
	EventUID types.UID `json:"eventUID,omitempty"`

	// auditRequestID is the audit-id of the API request that performed the
	// bind, if it could be correlated.
	// +optional
	AuditRequestID string `json:"auditRequestID,omitempty"`
}

// SchedulingDecisionSpec records a single scheduling decision. A
// SchedulingDecision is an immutable log entry, not a reconciled object: it
// is written once when the decision is observed and never updated
// afterward.
type SchedulingDecisionSpec struct {
	// podName is the name of the pod that was scheduled.
	// +required
	PodName string `json:"podName"`

	// podNamespace is the namespace of the pod that was scheduled.
	// +required
	PodNamespace string `json:"podNamespace"`

	// podUID is the UID of the pod that was scheduled. Used as the
	// idempotency key so a given pod bind produces at most one
	// SchedulingDecision.
	// +required
	PodUID types.UID `json:"podUID"`

	// schedulerName is the scheduler that made this decision (e.g.
	// default-scheduler, stork), taken from the reporting component of the
	// source Event.
	// +optional
	SchedulerName string `json:"schedulerName,omitempty"`

	// chosenNode is the node the pod was bound to. Empty for
	// FailedScheduling outcomes.
	// +optional
	ChosenNode string `json:"chosenNode,omitempty"`

	// outcome is the terminal result of the scheduling attempt.
	// +required
	Outcome SchedulingOutcome `json:"outcome"`

	// reasonSummary is a short human-readable explanation of the outcome,
	// taken from the source Event message (e.g. the predicate-failure
	// string for FailedScheduling).
	// +optional
	ReasonSummary string `json:"reasonSummary,omitempty"`

	// decisionTimestamp is when the scheduling decision was made.
	// +required
	DecisionTimestamp metav1.Time `json:"decisionTimestamp"`

	// schedulingLatencyMs is the time in milliseconds between pod creation
	// and this decision.
	// +optional
	SchedulingLatencyMs int64 `json:"schedulingLatencyMs,omitempty"`

	// candidateNodes lists nodes considered during scheduling. Populated
	// with the rejected-node/reason pairs available from Tier 1
	// reconstruction; scores are Tier 2 only.
	// +optional
	// +listType=atomic
	CandidateNodes []CandidateNode `json:"candidateNodes,omitempty"`

	// volumeContext describes the volume plumbing that influenced
	// placement, if the pod referenced a PVC.
	// +optional
	VolumeContext *VolumeContext `json:"volumeContext,omitempty"`

	// sourceRef links this decision back to the Kubernetes objects it was
	// reconstructed from.
	// +optional
	SourceRef SourceRef `json:"sourceRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sdec
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="Scheduler",type=string,JSONPath=`.spec.schedulerName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.chosenNode`
// +kubebuilder:printcolumn:name="VolumeDriver",type=string,JSONPath=`.spec.volumeContext.driverType`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.spec.outcome`
// +kubebuilder:printcolumn:name="LatencyMs",type=integer,JSONPath=`.spec.schedulingLatencyMs`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SchedulingDecision is a durable, queryable record of a single Kubernetes
// scheduling decision. It is cluster-scoped and treated as an immutable log
// entry: once created, it is not reconciled or updated in place.
type SchedulingDecision struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec records the scheduling decision.
	// +required
	Spec SchedulingDecisionSpec `json:"spec"`
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
