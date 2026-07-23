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

// Command dashboard serves a small read-only web UI over the
// SchedulingDecision CRD: stat cards, a node-placement heatmap by scheduler,
// a filterable recent-decisions table, and a latency-by-scheduler chart. It
// reads only from the CRD surface (no dependency on Loki/audit logs) and
// renders everything client-side from a single JSON endpoint.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	schedulingv1alpha1 "github.com/phenixblue/sch-audit/api/v1alpha1"
)

//go:embed static/index.html
var staticFS embed.FS

// +kubebuilder:rbac:groups=scheduling.purestorage.io,resources=schedulingdecisions,verbs=get;list;watch

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":8080", "Address the dashboard HTTP server listens on.")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(schedulingv1alpha1.AddToScheme(scheme))

	// A manager, not a one-shot client (contrast cmd/sweep): the dashboard
	// serves the same data repeatedly to however many browser tabs are
	// polling it, so a synced cache that's cheap to read on every request
	// beats hitting the API server on every request. No controller is
	// registered - the manager's cache lazily starts an informer for
	// SchedulingDecision the first time it's read, which is all a read-only
	// dashboard needs.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Fatalf("building manager: %v", err)
	}

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		log.Fatalf("reading embedded index.html: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/decisions", decisionsHandler(mgr.GetClient()))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("dashboard listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serving: %v", err)
		}
	}()

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("running manager: %v", err)
	}
}

// decisionsHandler serves the raw SchedulingDecision list as JSON. All
// aggregation (stat cards, heatmap, latency chart, table filtering) happens
// client-side in static/index.html - the CRD's own spec/status shape is
// already exactly what the UI needs, so there's no separate DTO to keep in
// sync with it.
func decisionsHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var list schedulingv1alpha1.SchedulingDecisionList
		if err := c.List(r.Context(), &list); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(list.Items); err != nil {
			log.Printf("encoding decisions response: %v", err)
		}
	}
}
