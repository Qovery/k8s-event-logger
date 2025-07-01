// main.go — v2 “minimal-diff”
package main

import (
	"context"
	"encoding/json"
	"flag"
	"k8s.io/apimachinery/pkg/fields"
	"log"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ignoreNormal   = flag.Bool("ignore-normal", false, "ignore events of type 'Normal'")
	ignoreUpdate   = flag.Bool("ignore-update", true, "ignore Update events")
	metricsEnabled = flag.Bool("metrics-enabled", false, "enable Prometheus /metrics endpoint")
)

/* -------------------- Prometheus -------------------- */

var eventsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "k8s_event_logger_events_total",
		Help: "Total Kubernetes events processed, enriched with Qovery labels",
	},
	[]string{"type", "reason", "kind",
		"qovery_project_id", "qovery_environment_id", "qovery_service_id"},
)

/* --------- Helper: fetch object & Qovery labels --------- */

func fetchObjectLabels(ctx context.Context, dyn dynamic.Interface, evt *corev1.Event) (proj, env, svc string) {
	// Build the GVR from APIVersion/Kind
	gvk := schema.FromAPIVersionAndKind(evt.InvolvedObject.APIVersion, evt.InvolvedObject.Kind)
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	if gvr.Group == "" && gvr.Version == "" {
		gvr.Version = "v1" // core group default
	}

	// Empty namespace == cluster-scoped
	ns := evt.InvolvedObject.Namespace
	res := dyn.Resource(gvr).Namespace(ns)

	obj, err := res.Get(ctx, evt.InvolvedObject.Name, metav1.GetOptions{})
	if err != nil {
		return // keep empty labels on error
	}

	acc, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	labels := acc.GetLabels()
	return labels["qovery.com/project-id"],
		labels["qovery.com/environment-id"],
		labels["qovery.com/service-id"]
}

/* ------------------------- main ------------------------- */

func main() {
	flag.Parse()

	appLog := log.New(os.Stderr, "", log.LstdFlags)
	evtLog := log.New(os.Stdout, "", 0)

	/* Build clients */
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		appLog.Fatalf("kubeconfig: %v", err)
	}
	appLog.Println("Using configuration:", restCfg.String())

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		appLog.Fatalf("typed client: %v", err)
	}
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		appLog.Fatalf("dynamic client: %v", err)
	}

	/* Expose /metrics if requested */
	if *metricsEnabled {
		prometheus.MustRegister(eventsTotal)
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			appLog.Printf("serving /metrics on :8080")
			_ = http.ListenAndServe(":8080", nil)
		}()
	}

	/* Event informer (unchanged from v1) */
	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"events",
		corev1.NamespaceAll,
		fields.Everything(),
	)
	_, controller := cache.NewInformer(
		watchlist,
		&corev1.Event{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) { handle(obj, evtLog, dynClient) },
			UpdateFunc: func(oldObj, newObj interface{}) {
				if *ignoreUpdate {
					return
				}
				handle(newObj, evtLog, dynClient)
			},
		},
	)

	stop := make(chan struct{})
	go controller.Run(stop)
	<-stop
}

/* -------------- single-event processing -------------- */

func handle(o interface{}, out *log.Logger, dyn dynamic.Interface) {
	evt := o.(*corev1.Event)

	if *ignoreNormal && evt.Type == corev1.EventTypeNormal {
		return
	}

	// 1) raw JSON log
	raw, _ := json.Marshal(evt)
	out.Println(string(raw))

	// 2) Prometheus
	if !*metricsEnabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pid, eid, sid := fetchObjectLabels(ctx, dyn, evt)
	eventsTotal.WithLabelValues(
		evt.Type, evt.Reason, evt.InvolvedObject.Kind, pid, eid, sid,
	).Inc()
}

/* ---------------- extra utility ---------------- */

func safeIsNil(i interface{}) bool {
	if i == nil {
		return true
	}
	v := reflect.ValueOf(i)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

/*
Usage examples:

go run main.go                               # logs events as JSON
go run main.go -metrics-enabled              # + /metrics endpoint
go run main.go -ignore-normal=false          # include “Normal” events
*/
