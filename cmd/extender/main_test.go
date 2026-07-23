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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

func TestFilterHandlerPassthroughAndRecordsCandidates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "example-pod", Namespace: "default", UID: types.UID("pod-uid-1")},
	}
	args := extenderv1.ExtenderArgs{
		Pod:       pod,
		NodeNames: &[]string{"node-a", "node-b", "node-c"},
	}
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling ExtenderArgs: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/filter", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	filterHandler(c)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var result extenderv1.ExtenderFilterResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshaling ExtenderFilterResult: %v", err)
	}
	if result.NodeNames == nil || len(*result.NodeNames) != 3 {
		t.Fatalf("NodeNames = %v, want all 3 nodes returned unchanged", result.NodeNames)
	}
	if len(result.FailedNodes) != 0 {
		t.Errorf("FailedNodes = %v, want none - this extender must never reject a node", result.FailedNodes)
	}

	var events corev1.EventList
	if err := c.List(context.Background(), &events); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("got %d events, want exactly 1", len(events.Items))
	}
	event := events.Items[0]
	if event.Reason != schedulingv1alpha1.CandidateNodesEventReason {
		t.Errorf("Reason = %q, want %q", event.Reason, schedulingv1alpha1.CandidateNodesEventReason)
	}
	if event.Message != "node-a,node-b,node-c" {
		t.Errorf("Message = %q, want %q", event.Message, "node-a,node-b,node-c")
	}
	if event.InvolvedObject.UID != pod.UID || event.InvolvedObject.Name != pod.Name {
		t.Errorf("InvolvedObject = %+v, want a reference to %s/%s (%s)",
			event.InvolvedObject, pod.Namespace, pod.Name, pod.UID)
	}
	if event.ReportingController != reportingController {
		t.Errorf("ReportingController = %q, want %q", event.ReportingController, reportingController)
	}
}

func TestFilterHandlerWithFullNodeObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", UID: types.UID("u")}}
	args := extenderv1.ExtenderArgs{
		Pod: pod,
		Nodes: &corev1.NodeList{Items: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-x"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-y"}},
		}},
	}
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling ExtenderArgs: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/filter", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	filterHandler(c)(rec, req)

	var result extenderv1.ExtenderFilterResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshaling ExtenderFilterResult: %v", err)
	}
	if result.Nodes == nil || len(result.Nodes.Items) != 2 {
		t.Fatalf("Nodes = %v, want both nodes returned unchanged", result.Nodes)
	}

	var events corev1.EventList
	if err := c.List(context.Background(), &events); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(events.Items) != 1 || events.Items[0].Message != "node-x,node-y" {
		t.Fatalf("got events %+v, want one CandidateNodes event listing node-x,node-y", events.Items)
	}
}

func TestFilterHandlerRejectsInvalidBody(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	req := httptest.NewRequest(http.MethodPost, "/filter", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	filterHandler(c)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
