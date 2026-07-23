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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

var _ = Describe("SchedulingDecisionReconciler", func() {
	const namespace = "default"

	// uniqueName avoids collisions between tests sharing one long-lived
	// envtest environment and manager.
	uniqueName := func(prefix string) string {
		return prefix + "-" + rand.String(8)
	}

	newPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
			},
		}
	}

	// bindPod sets spec.nodeName via the pods/binding subresource, the only
	// way the apiserver allows that field to transition from empty (a plain
	// Update is rejected as an immutable-field change), then refreshes pod
	// so later Status().Update calls use a current resourceVersion. The
	// refresh polls rather than doing a single Get: k8sClient reads through
	// the manager's cache, which can still be a step behind the binding
	// write for a moment, and a Get that lands in that window would hand
	// back a resourceVersion that's already stale, making the caller's next
	// Status().Update conflict.
	bindPod := func(pod *corev1.Pod, nodeName string) {
		binding := &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			Target:     corev1.ObjectReference{Kind: "Node", Name: nodeName},
		}
		Expect(k8sClient.SubResource("binding").Create(ctx, pod, binding)).To(Succeed())

		Eventually(func() (string, error) {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				return "", err
			}
			return pod.Spec.NodeName, nil
		}, 5*time.Second, 50*time.Millisecond).Should(Equal(nodeName))
	}

	setPodScheduledCondition := func(pod *corev1.Pod, status corev1.ConditionStatus, reason, message string) {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// fetchDecision waits for both the SchedulingDecision to exist and its
	// status to carry an outcome, not just existence: the controller writes
	// status via a separate call after creating the object, so there's a
	// legitimate (if usually brief) window where the object exists but
	// status is still empty.
	fetchDecision := func(podUID types.UID) *schedulingv1alpha1.SchedulingDecision {
		decision := &schedulingv1alpha1.SchedulingDecision{}
		Eventually(func() (schedulingv1alpha1.SchedulingOutcome, error) {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: decisionName(podUID)}, decision); err != nil {
				return "", err
			}
			return decision.Status.Outcome, nil
		}, 5*time.Second, 100*time.Millisecond).ShouldNot(BeEmpty())
		return decision
	}

	It("records a Scheduled decision once the PodScheduled condition is true, exactly once", func() {
		pod := newPod(uniqueName("scheduled-pod"))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

		bindPod(pod, "node-1")
		setPodScheduledCondition(pod, corev1.ConditionTrue, "", "")

		decision := fetchDecision(pod.UID)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })

		Expect(decision.Status.Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomeScheduled))
		Expect(decision.Status.ChosenNode).To(Equal("node-1"))
		Expect(decision.Spec.PodName).To(Equal(pod.Name))
		Expect(decision.Spec.PodNamespace).To(Equal(namespace))
		Expect(decision.Spec.PodUID).To(Equal(pod.UID))
		Expect(decision.Spec.SchedulerName).To(Equal("default-scheduler")) // pod.Spec.SchedulerName fallback
		Expect(decision.Labels[podUIDLabel]).To(Equal(string(pod.UID)))
		Expect(decision.Status.Transitions).To(HaveLen(1))

		// Retention sweep (cmd/sweep) keys off this label: it should be set
		// to roughly now+DefaultRetentionWindow, not the zero value or some
		// unrelated timestamp.
		expiresAt, err := strconv.ParseInt(decision.Labels[schedulingv1alpha1.ExpiresAtLabel], 10, 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Unix(expiresAt, 0)).To(BeTemporally(
			"~", time.Now().Add(DefaultRetentionWindow), 10*time.Second,
		))

		// Idempotency: forcing another reconcile (via an unrelated update)
		// must not produce a second decision for the same pod UID, nor
		// append a duplicate transition to the one that exists.
		pod.Labels = map[string]string{"touch": rand.String(4)}
		Expect(k8sClient.Update(ctx, pod)).To(Succeed())

		Consistently(func() (int, error) {
			var list schedulingv1alpha1.SchedulingDecisionList
			if err := k8sClient.List(ctx, &list, client.MatchingLabels{podUIDLabel: string(pod.UID)}); err != nil {
				return 0, err
			}
			return len(list.Items), nil
		}, 2*time.Second, 200*time.Millisecond).Should(Equal(1))

		Consistently(func() (int, error) {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(decision), decision); err != nil {
				return 0, err
			}
			return len(decision.Status.Transitions), nil
		}, 2*time.Second, 200*time.Millisecond).Should(Equal(1))
	})

	It("records a FailedScheduling decision with the predicate-failure message", func() {
		pod := newPod(uniqueName("unschedulable-pod"))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

		const failureMessage = "0/3 nodes are available: 3 Insufficient cpu."
		setPodScheduledCondition(pod, corev1.ConditionFalse, "Unschedulable", failureMessage)

		decision := fetchDecision(pod.UID)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })

		Expect(decision.Status.Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomeFailedScheduling))
		Expect(decision.Status.ChosenNode).To(BeEmpty())
		Expect(decision.Status.ReasonSummary).To(Equal(failureMessage))
	})

	It("supersedes a transient FailedScheduling once the pod actually gets Scheduled", func() {
		// Reproduces the retry-loop a scheduler goes through against a PVC
		// with an Immediate VolumeBindingMode: it reports FailedScheduling
		// while the PVC is still being provisioned, then Scheduled once the
		// PVC binds and it retries. The first observation must not be the
		// permanent record.
		pod := newPod(uniqueName("retry-pod"))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

		const transientMessage = "0/6 nodes are available: pod has unbound immediate PersistentVolumeClaims."
		setPodScheduledCondition(pod, corev1.ConditionFalse, "Unschedulable", transientMessage)

		decision := fetchDecision(pod.UID)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })
		Expect(decision.Status.Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomeFailedScheduling))
		Expect(decision.Status.Transitions).To(HaveLen(1))

		bindPod(pod, "node-1")
		setPodScheduledCondition(pod, corev1.ConditionTrue, "", "")

		Eventually(func() (schedulingv1alpha1.SchedulingOutcome, error) {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(decision), decision); err != nil {
				return "", err
			}
			return decision.Status.Outcome, nil
		}, 5*time.Second, 100*time.Millisecond).Should(Equal(schedulingv1alpha1.SchedulingOutcomeScheduled))

		Expect(decision.Status.ChosenNode).To(Equal("node-1"))
		Expect(decision.Status.Transitions).To(HaveLen(2))
		Expect(decision.Status.Transitions[0].Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomeFailedScheduling))
		Expect(decision.Status.Transitions[1].Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomeScheduled))
	})

	It("records a Preempted decision from an Event when the pod still exists", func() {
		pod := newPod(uniqueName("preempted-pod"))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

		event := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("preempted-pod-evt"), Namespace: namespace},
			InvolvedObject: corev1.ObjectReference{
				Kind:      podKind,
				Namespace: pod.Namespace,
				Name:      pod.Name,
				UID:       pod.UID,
			},
			Reason:              eventReasonPreempted,
			Message:             "Preempted by higher-priority-pod on node-2",
			LastTimestamp:       metav1.Now(),
			ReportingController: "default-scheduler",
			Type:                corev1.EventTypeNormal,
		}
		Expect(k8sClient.Create(ctx, event)).To(Succeed())

		decision := fetchDecision(pod.UID)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })

		Expect(decision.Status.Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomePreempted))
		Expect(decision.Status.ReasonSummary).To(Equal(event.Message))
		Expect(decision.Spec.SchedulerName).To(Equal("default-scheduler"))
		Expect(decision.Status.Transitions).To(HaveLen(1))
		Expect(decision.Status.Transitions[0].SourceRef.EventUID).To(Equal(event.UID))
	})

	It("records a Preempted decision from an Event even after the pod is gone", func() {
		pod := newPod(uniqueName("preempted-gone-pod"))
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		podUID := pod.UID

		event := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("preempted-gone-pod-evt"), Namespace: namespace},
			InvolvedObject: corev1.ObjectReference{
				Kind:      podKind,
				Namespace: pod.Namespace,
				Name:      pod.Name,
				UID:       podUID,
			},
			Reason:        eventReasonPreempted,
			Message:       "Preempted by higher-priority-pod on node-3",
			LastTimestamp: metav1.Now(),
			Type:          corev1.EventTypeNormal,
		}
		Expect(k8sClient.Create(ctx, event)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

		decision := fetchDecision(podUID)
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })

		Expect(decision.Status.Outcome).To(Equal(schedulingv1alpha1.SchedulingOutcomePreempted))
		Expect(decision.Spec.PodName).To(Equal(pod.Name))
		Expect(decision.Spec.PodNamespace).To(Equal(namespace))
		Expect(decision.Spec.PodUID).To(Equal(podUID))
	})

	// Milestone 5 (validate against a real cluster) confirmed default-scheduler
	// + STORK + PX-CSI (Portworx) end to end against a live OpenShift cluster.
	// FADA and vsphere-csi aren't reachable from this environment (no FlashArray
	// or vSphere cluster available), so this DescribeTable exercises all three
	// plan-named drivers through the same real reconciler/envtest pipeline
	// instead - the only thing that varies per driver is the StorageClass's
	// provisioner string, so one parameterized flow covers them all without
	// re-proving the PVC/pod/bind/condition scaffolding three times over.
	DescribeTable("resolves volume context for a StorageClass's provisioner",
		func(provisioner, expectedDriver string) {
			scName := uniqueName("sc")
			bindingMode := storagev1.VolumeBindingWaitForFirstConsumer
			sc := &storagev1.StorageClass{
				ObjectMeta:        metav1.ObjectMeta{Name: scName},
				Provisioner:       provisioner,
				VolumeBindingMode: &bindingMode,
			}
			Expect(k8sClient.Create(ctx, sc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })

			pvcName := uniqueName("pvc")
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: namespace},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &scName,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pvc) })

			pod := newPod(uniqueName("volume-pod"))
			pod.Spec.Volumes = []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			}}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

			bindPod(pod, "node-1")
			setPodScheduledCondition(pod, corev1.ConditionTrue, "", "")

			decision := fetchDecision(pod.UID)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, decision) })

			Expect(decision.Spec.VolumeContext).NotTo(BeNil())
			Expect(decision.Spec.VolumeContext.PVCName).To(Equal(pvcName))
			Expect(decision.Spec.VolumeContext.StorageClass).To(Equal(scName))
			Expect(decision.Spec.VolumeContext.DriverType).To(Equal(expectedDriver))
			Expect(decision.Spec.VolumeContext.BindingMode).To(Equal(string(storagev1.VolumeBindingWaitForFirstConsumer)))
		},
		Entry("FADA (Pure Storage FlashArray CSI)", "csi.purestorage.com", "FADA"),
		Entry("PX-CSI (Portworx)", "pxd.portworx.com", "PX-CSI"),
		Entry("vsphere-csi", "csi.vsphere.vmware.com", "vsphere-csi"),
	)
})
