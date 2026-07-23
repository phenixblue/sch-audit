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

// Command extender is an optional Kubernetes scheduler Extender
// (https://kubernetes.io/docs/reference/config-api/kube-scheduler-config.v1/)
// that observes, but never affects, scheduling: it implements only the
// Filter verb, and always returns every node it was given as still
// eligible. Its purpose is purely to see the post-Filter candidate list for
// a pod - something Tier 1's Event-based reconstruction can't recover, and
// full per-node Score visibility, i.e. Tier 2, isn't achievable without a
// scheduling-framework plugin compiled into a scheduler binary this project
// doesn't control (see docs/tier2-investigation.md). It's not wired into
// any scheduler by default: registering it means adding an entry to that
// scheduler's KubeSchedulerConfiguration extenders list, which is out of
// scope for this binary to do on its own.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

// reportingController identifies this component as the source of the
// Events it creates, distinct from the scheduler(s) it observes.
const reportingController = "sch-audit-extender"

// +kubebuilder:rbac:groups="",resources=events,verbs=create

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":8099", "Address the extender HTTP server listens on.")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	// A direct (uncached) client: this component only ever creates Events,
	// one per Filter call, so there's nothing to benefit from a cache.
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("building client: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/filter", filterHandler(c))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("extender listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serving: %v", err)
	}
}

// filterHandler implements the scheduler Extender Filter verb as a pure
// observer: it records the candidate node list as an Event on the pod, then
// returns every node it was given as still eligible, unchanged. It never
// removes a node, so registering this extender cannot change which node a
// pod lands on or whether it schedules at all.
func filterHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var args extenderv1.ExtenderArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, fmt.Sprintf("decoding ExtenderArgs: %v", err), http.StatusBadRequest)
			return
		}

		names := candidateNodeNames(args)
		if args.Pod != nil && len(names) > 0 {
			if err := recordCandidateNodes(r.Context(), c, args.Pod, names); err != nil {
				// Never fail the scheduling attempt over an observability
				// write: log and continue returning the passthrough result.
				log.Printf("recording candidate nodes for pod %s/%s: %v", args.Pod.Namespace, args.Pod.Name, err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(passthroughResult(args)); err != nil {
			log.Printf("encoding ExtenderFilterResult: %v", err)
		}
	}
}

// candidateNodeNames extracts the candidate node names from args, which
// carries them as either Nodes (full objects) or NodeNames (just names)
// depending on how this extender is registered (its
// KubeSchedulerConfiguration entry's nodeCacheCapable setting).
func candidateNodeNames(args extenderv1.ExtenderArgs) []string {
	if args.NodeNames != nil {
		return *args.NodeNames
	}
	if args.Nodes != nil {
		names := make([]string, 0, len(args.Nodes.Items))
		for _, n := range args.Nodes.Items {
			names = append(names, n.Name)
		}
		return names
	}
	return nil
}

// passthroughResult returns every node from args as still eligible, in
// whichever of Nodes/NodeNames form it arrived in, with no FailedNodes -
// this extender never rejects a node.
func passthroughResult(args extenderv1.ExtenderArgs) extenderv1.ExtenderFilterResult {
	return extenderv1.ExtenderFilterResult{
		Nodes:     args.Nodes,
		NodeNames: args.NodeNames,
	}
}

// recordCandidateNodes creates an Event on pod carrying the candidate node
// names, for the SchedulingDecision reconciler to pick up (it already
// watches Events on Pods via the same involvedObject index used for
// Scheduled/FailedScheduling/Preempted). GenerateName, not a deterministic
// name: a pod can go through more than one scheduling attempt (e.g. a retry
// loop), and each is worth its own record, same as the outcome Events
// already do.
func recordCandidateNodes(ctx context.Context, c client.Client, pod *corev1.Pod, names []string) error {
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sch-audit-candidates-",
			Namespace:    pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       pod.UID,
		},
		Reason:              schedulingv1alpha1.CandidateNodesEventReason,
		Message:             strings.Join(names, ","),
		Type:                corev1.EventTypeNormal,
		ReportingController: reportingController,
		FirstTimestamp:      now,
		LastTimestamp:       now,
	}
	return c.Create(ctx, event)
}
