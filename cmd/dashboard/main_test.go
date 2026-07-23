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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

func TestDecisionsHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := schedulingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}

	decision := &schedulingv1alpha1.SchedulingDecision{
		ObjectMeta: metav1.ObjectMeta{Name: "sdec-test"},
		Spec: schedulingv1alpha1.SchedulingDecisionSpec{
			PodName:      "example-pod",
			PodNamespace: "default",
		},
		Status: schedulingv1alpha1.SchedulingDecisionStatus{
			Outcome: schedulingv1alpha1.SchedulingOutcomeScheduled,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(decision).Build()

	req := httptest.NewRequest(http.MethodGet, "/api/decisions", nil)
	rec := httptest.NewRecorder()
	decisionsHandler(c)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got []schedulingv1alpha1.SchedulingDecision
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}
	if len(got) != 1 || got[0].Name != "sdec-test" {
		t.Fatalf("got %+v, want a single sdec-test decision", got)
	}
	if got[0].Status.Outcome != schedulingv1alpha1.SchedulingOutcomeScheduled {
		t.Errorf("Status.Outcome = %q, want Scheduled", got[0].Status.Outcome)
	}
}

func TestEmbeddedIndexHTML(t *testing.T) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded index.html: %v", err)
	}
	html := string(data)
	for _, want := range []string{"<title>sch-audit</title>", "api/decisions", "id=\"decisions-body\""} {
		if !strings.Contains(html, want) {
			t.Errorf("embedded index.html missing expected content %q", want)
		}
	}
}
