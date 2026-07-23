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
	"context"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		labels  map[string]string
		want    bool
		wantErr bool
	}{
		{
			name:    "missing label",
			labels:  map[string]string{},
			wantErr: true,
		},
		{
			name:    "unparsable label",
			labels:  map[string]string{schedulingv1alpha1.ExpiresAtLabel: "not-a-number"},
			wantErr: true,
		},
		{
			name:   "in the past",
			labels: map[string]string{schedulingv1alpha1.ExpiresAtLabel: strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)},
			want:   true,
		},
		{
			name:   "exactly now",
			labels: map[string]string{schedulingv1alpha1.ExpiresAtLabel: strconv.FormatInt(now.Unix(), 10)},
			want:   true,
		},
		{
			name:   "in the future",
			labels: map[string]string{schedulingv1alpha1.ExpiresAtLabel: strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isExpired(tt.labels, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("isExpired() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("isExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSweep(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)

	expired := decisionWithExpiry(t, "sdec-expired", now.Add(-time.Hour))
	notExpired := decisionWithExpiry(t, "sdec-not-expired", now.Add(time.Hour))
	unlabeled := &schedulingv1alpha1.SchedulingDecision{
		ObjectMeta: metav1.ObjectMeta{Name: "sdec-unlabeled"},
	}

	scheme := runtime.NewScheme()
	if err := schedulingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(expired, notExpired, unlabeled).
		Build()

	// No Delete in this scenario can fail (all objects exist, fake client
	// has no injected errors), so sweep's os.Exit(1)-on-failure path can't
	// trigger here - safe to call the real function directly.
	if err := sweep(context.Background(), c, now); err != nil {
		t.Fatalf("sweep() returned error: %v", err)
	}

	var remaining schedulingv1alpha1.SchedulingDecisionList
	if err := c.List(context.Background(), &remaining); err != nil {
		t.Fatalf("listing after sweep: %v", err)
	}

	names := make(map[string]bool, len(remaining.Items))
	for _, d := range remaining.Items {
		names[d.Name] = true
	}

	if names["sdec-expired"] {
		t.Error("expired decision should have been deleted")
	}
	if !names["sdec-not-expired"] {
		t.Error("not-yet-expired decision should have been kept")
	}
	if !names["sdec-unlabeled"] {
		t.Error("unlabeled decision should have been kept (skipped, not deleted)")
	}
}

func decisionWithExpiry(t *testing.T, name string, expiresAt time.Time) *schedulingv1alpha1.SchedulingDecision {
	t.Helper()
	return &schedulingv1alpha1.SchedulingDecision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				schedulingv1alpha1.ExpiresAtLabel: strconv.FormatInt(expiresAt.Unix(), 10),
			},
		},
		Spec: schedulingv1alpha1.SchedulingDecisionSpec{
			PodName:      name,
			PodNamespace: "default",
			PodUID:       types.UID(name),
		},
	}
}
