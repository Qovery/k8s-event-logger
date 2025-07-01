// main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynamicinformers "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ignoreNormal   = flag.Bool("ignore-normal", false, "ignore events of type 'Normal' to reduce noise")
	ignoreUpdate   = flag.Bool("ignore-update", true, "ignore Update events")
	metricsEnabled = flag.Bool("metrics-enabled", false, "enable metrics")
	resyncPeriod   = flag.Duration("resync-period", 0, "resync period for informers (0 disables)")
)

/* -------------------------------------------------------------------------- */
/*                                Prom metrics                                */
/* -------------------------------------------------------------------------- */

var (
	eventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_event_logger_q_k8s_events_total",
			Help: "Total number of Kubernetes events processed",
		},
		[]string{"type", "reason", "object_kind", "qovery.com/project-id", "qovery.com/environment-id", "qovery_com_service_id"},
	)
	//eventsWithoutService = promauto.NewCounterVec(
	//	prometheus.CounterOpts{
	//		Name: "k8s_event_logger_debug_total",
	//		Help: "Total number of Kubernetes events processed",
	//	},
	//	[]string{"type", "reason", "object_kind", "object_name", "namespace"},
	//)
)

/* -------------------------- Dynamic-informer cache ------------------------ */

type dynCache struct {
	lister cache.GenericLister
	stop   chan struct{}
}

var (
	dynClient dynamic.Interface
	dynMap    = map[string]*dynCache{} // key = GVR.String()
)

/* -------------------------------------------------------------------------- */

func main() {
	flag.Parse()

	logApp := log.New(os.Stderr, "", log.LstdFlags) // appli
	logEvt := log.New(os.Stdout, "", 0)             // events

	/* ----------------------- /metrics HTTP server ------------------------- */
	go func() {
		if *metricsEnabled {
			metricsPort := "3101"
			http.Handle("/metrics", promhttp.Handler())
			http.ListenAndServe(":"+metricsPort, nil)
		}
	}()

	/* ---------------------- Build kube clients ---------------------------- */
	cfgLoadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfgOverrides := &clientcmd.ConfigOverrides{}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		cfgLoadingRules, cfgOverrides).ClientConfig()
	if err != nil {
		logApp.Fatalf("kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logApp.Fatalf("typed client: %v", err)
	}
	dynClient, err = dynamic.NewForConfig(restCfg)
	if err != nil {
		logApp.Fatalf("dynamic client: %v", err)
	}

	/* -------------------- SharedInformerFactory (typed) ------------------- */
	factory := informers.NewSharedInformerFactory(clientset, *resyncPeriod)

	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	/* ---------------------- Watch Kubernetes events ----------------------- */
	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"events",
		corev1.NamespaceAll,
		metav1.Everything(),
	)
	_, ctrl := cache.NewInformer(
		watchlist,
		&corev1.Event{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				evt := obj.(*corev1.Event)
				if *ignoreNormal && evt.Type == corev1.EventTypeNormal {
					return
				}
				handleEvent(evt, logEvt)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if *ignoreUpdate {
					return
				}
				evt := newObj.(*corev1.Event)
				if *ignoreNormal && evt.Type == corev1.EventTypeNormal {
					return
				}
				handleEvent(evt, logEvt)
			},
		},
	)
	go ctrl.Run(stopCh)
	<-stopCh
}

/* -------------------------------------------------------------------------- */
func fetchObject(evt *corev1.Event) (runtime.Object, bool) {
	ns := evt.Namespace
	if ns == "" {
		ns = "default"
	}

	gvr := guessGVR(evt) // works for built-ins and CRDs
	dynCache := getDynLister(gvr)
	obj, err := dynCache.lister.
		ByNamespace(ns).
		Get(evt.InvolvedObject.Name)
	return obj, err == nil
}

func handleEvent(evt *corev1.Event, logger *log.Logger) {
	// 1) raw JSON to stdout (unchanged)
	raw, _ := json.Marshal(evt)
	logger.Println(string(raw))

	if *metricsEnabled == false {
		return
	}

	// 2) try to resolve the referenced object ONCE
	obj, _ := fetchObject(evt)
	projectId := labelOf(obj, "qovery.com/project-id", evt.InvolvedObject.Kind)
	envId := labelOf(obj, "qovery.com/environment-id", evt.InvolvedObject.Kind)
	serviceId := labelOf(obj, "qovery.com/service-id", evt.InvolvedObject.Kind)

	// 3) main counter
	ns := evt.Namespace
	if ns == "" {
		ns = "default"
	}
	eventsTotal.WithLabelValues(
		evt.Type,
		evt.Reason,
		evt.InvolvedObject.Kind,
		projectId,
		envId,
		serviceId,
	).Inc()

	//if serviceId == "" {
	//eventsWithoutService.WithLabelValues(
	//	evt.Type,
	//	evt.Reason,
	//	evt.InvolvedObject.Kind,
	//	evt.InvolvedObject.Name,
	//	evt.InvolvedObject.Namespace,
	//)
	//}
}

/* --------------------------- helper functions ----------------------------- */

func labelOf(obj runtime.Object, key string, kind string) string {
	if obj == nil {
		return ""
	}
	// handle "typed nil"
	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return ""
	}

	acc, err := meta.Accessor(obj)
	if err != nil || acc == nil {
		return ""
	}

	return acc.GetLabels()[key]
}

func guessGVR(evt *corev1.Event) schema.GroupVersionResource {
	gvk := schema.FromAPIVersionAndKind(evt.InvolvedObject.APIVersion, evt.InvolvedObject.Kind)
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	return gvr
}

func getDynLister(gvr schema.GroupVersionResource) *dynCache {
	key := gvr.String()
	if c, ok := dynMap[key]; ok {
		return c
	}

	f := dynamicinformers.NewDynamicSharedInformerFactory(dynClient, *resyncPeriod)
	inf := f.ForResource(gvr)

	stop := make(chan struct{})
	f.Start(stop)

	// wait cache sync (non-blocking backoff)
	go func() {
		wait.PollUntilContextTimeout(context.Background(), 200*time.Millisecond, 10*time.Second, true,
			func(ctx context.Context) (bool, error) {
				return cache.WaitForCacheSync(ctx.Done(), inf.Informer().HasSynced), nil
			})
	}()

	c := &dynCache{lister: inf.Lister(), stop: stop}
	dynMap[key] = c
	return c
}
