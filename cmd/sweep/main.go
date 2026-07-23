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

// Command sweep deletes SchedulingDecision records whose retention window
// has elapsed (see api/v1alpha1.ExpiresAtLabel). It's a one-shot CLI, not a
// controller: it lists, deletes what's expired, and exits. It's meant to run
// as a Kubernetes CronJob (config/manager/sweep_cronjob.yaml) using the same
// image and ServiceAccount as the sch-audit controller manager.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=get;list;delete

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(schedulingv1alpha1.AddToScheme(scheme))

	// A direct (uncached) client is deliberate here: this is a one-shot
	// List-then-Delete pass, not a long-running controller, so there's no
	// benefit to a manager/informer cache and every extra moving part is
	// something that could go stale or leak.
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("building client: %v", err)
	}

	if err := sweep(context.Background(), c, time.Now()); err != nil {
		log.Fatal(err)
	}
}

func sweep(ctx context.Context, c client.Client, now time.Time) error {
	var list schedulingv1alpha1.SchedulingDecisionList
	if err := c.List(ctx, &list); err != nil {
		return fmt.Errorf("listing SchedulingDecisions: %w", err)
	}

	var deleted, skipped, failed int
	for i := range list.Items {
		decision := &list.Items[i]

		expired, err := isExpired(decision.Labels, now)
		if err != nil {
			log.Printf("skipping %s: %v", decision.Name, err)
			skipped++
			continue
		}
		if !expired {
			continue
		}

		if err := c.Delete(ctx, decision); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("deleting %s: %v", decision.Name, err)
			failed++
			continue
		}
		deleted++
	}

	log.Printf("sweep complete: %d total, %d deleted, %d skipped (missing/invalid %s label), %d failed",
		len(list.Items), deleted, skipped, schedulingv1alpha1.ExpiresAtLabel, failed)
	if failed > 0 {
		os.Exit(1)
	}
	return nil
}

// isExpired reports whether labels carries an ExpiresAtLabel timestamp
// that's already passed. A record missing the label, or carrying an
// unparsable value, is reported as an error rather than silently swept or
// silently kept forever - either a decision predates retention being added,
// or something other than this project created it, and either way it's
// worth an operator noticing rather than guessing.
func isExpired(labels map[string]string, now time.Time) (bool, error) {
	value, ok := labels[schedulingv1alpha1.ExpiresAtLabel]
	if !ok {
		return false, fmt.Errorf("missing %s label", schedulingv1alpha1.ExpiresAtLabel)
	}

	expiresAt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false, fmt.Errorf("parsing %s label %q: %w", schedulingv1alpha1.ExpiresAtLabel, value, err)
	}

	return !now.Before(time.Unix(expiresAt, 0)), nil
}
