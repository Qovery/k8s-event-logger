package main

import (
	"context"
	"encoding/json"
	"flag"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicinformers "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var (
	ignoreNormal   = flag.Bool("ignore-normal", false, "ignore events of type 'Normal'")
	ignoreUpdate   = flag.Bool("ignore-update", true, "ignore Update events")
	metricsEnabled = flag.Bool("metrics-enabled", false, "enable /metrics endpoint")
	resyncPeriod   = flag.Duration("resync-period", 0, "informer resync period (0 disables)")
)

var (
	eventsTotal *prometheus.CounterVec
)

func initMetrics() {
	if !*metricsEnabled {
		return
	}
	eventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_event_logger_events_total",
			Help: "Total Kubernetes events processed, enriched with Qovery labels",
		},
		[]string{"type", "reason", "kind",
			"qovery_project_id", "qovery_environment_id", "qovery_service_id"},
	)
	prometheus.MustRegister(eventsTotal)

	go func() {
		log.Printf("[metrics] exposing /metrics on :8080")
		_ = http.ListenAndServe(":8080", promhttp.Handler())
	}()
}

func incMetrics(evt *corev1.Event, projectID, envID, serviceID string) {
	if eventsTotal == nil {
		return
	}
	eventsTotal.WithLabelValues(
		evt.Type, evt.Reason, evt.InvolvedObject.Kind,
		projectID, envID, serviceID,
	).Inc()
}

type Enricher struct {
	dynClient    dynamic.Interface
	resyncPeriod time.Duration

	mu     sync.RWMutex
	lister map[string]cache.GenericLister
	stopCh map[string]chan struct{}
}

func newEnricher(dyn dynamic.Interface, resync time.Duration) *Enricher {
	return &Enricher{
		dynClient:    dyn,
		resyncPeriod: resync,
		lister:       map[string]cache.GenericLister{},
		stopCh:       map[string]chan struct{}{},
	}
}

func (e *Enricher) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.stopCh {
		close(ch)
	}
}

func (e *Enricher) Labels(evt *corev1.Event, appLogger *log.Logger) (string, string, string) {
	obj := e.fetch(evt, appLogger)
	return labelOf(obj, "qovery.com/project-id"),
		labelOf(obj, "qovery.com/environment-id"),
		labelOf(obj, "qovery.com/service-id")
}

func (e *Enricher) fetch(evt *corev1.Event, appLogger *log.Logger) runtime.Object {
	gvr := guessGVR(evt)
	printLog := evt.InvolvedObject.Kind == "Pod"

	if printLog {
		appLogger.Printf("=== FETCH DEBUG ===")
		appLogger.Printf("Event: %s/%s/%s", evt.Namespace, evt.InvolvedObject.Kind, evt.InvolvedObject.Name)
		appLogger.Printf("APIVersion: %s, Kind: %s", evt.InvolvedObject.APIVersion, evt.InvolvedObject.Kind)
		appLogger.Printf("Computed GVR: %s", gvr.String())
	}

	l := e.getLister(gvr)
	if l == nil {
		if printLog {
			appLogger.Printf("ERROR: getLister returned nil for GVR: %s", gvr.String())
		}
		return nil
	}

	if printLog {
		appLogger.Printf("Lister obtained successfully")
	}

	// Première tentative : ressource cluster-scoped
	if obj, err := l.Get(evt.InvolvedObject.Name); err == nil {
		if printLog {
			appLogger.Printf("SUCCESS: Found via cluster-scoped lister")
		}
		return obj
	} else {
		if printLog {
			appLogger.Printf("Cluster-scoped lookup failed: %v", err)
		}
	}

	// Deuxième tentative : ressource namespacée
	if evt.Namespace != "" {
		namespacedLister := l.ByNamespace(evt.Namespace)
		if printLog {
			appLogger.Printf("Trying namespaced lookup in namespace: %s", evt.Namespace)
		}

		if obj, err := namespacedLister.Get(evt.InvolvedObject.Name); err == nil {
			if printLog {
				appLogger.Printf("SUCCESS: Found via namespaced lister")
			}
			return obj
		} else {
			if printLog {
				appLogger.Printf("Namespaced lookup failed: %v", err)
			}
		}
	} else {
		if printLog {
			appLogger.Printf("No namespace available for namespaced lookup")
		}
	}

	if printLog {
		appLogger.Printf("FAILURE: Object not found in cache")
		appLogger.Printf("=== END FETCH DEBUG ===")
	}
	return nil
}

// Version améliorée de guessGVR avec plus de debug
func guessGVR(evt *corev1.Event) schema.GroupVersionResource {
	gvk := schema.FromAPIVersionAndKind(evt.InvolvedObject.APIVersion, evt.InvolvedObject.Kind)

	gvr, _ := meta.UnsafeGuessKindToResource(gvk)

	// Correction pour les ressources core/v1
	if gvr.Group == "" && gvr.Version == "" {
		gvr.Version = "v1"
	}

	// Pour Pod spécifiquement, on s'assure d'avoir le bon GVR
	if evt.InvolvedObject.Kind == "Pod" && gvr.Resource == "" {
		gvr = schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "pods",
		}
	}

	return gvr
}

// Version améliorée de getLister avec debug
func (e *Enricher) getLister(gvr schema.GroupVersionResource) cache.GenericLister {
	key := gvr.String()

	e.mu.RLock()
	l, ok := e.lister[key]
	e.mu.RUnlock()
	if ok {
		return l
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Double-check après avoir pris le lock
	if l, ok = e.lister[key]; ok {
		return l
	}

	log.Printf("Creating new informer for GVR: %s", gvr.String())

	f := dynamicinformers.NewDynamicSharedInformerFactory(e.dynClient, e.resyncPeriod)
	inf := f.ForResource(gvr)

	stop := make(chan struct{})
	f.Start(stop)

	log.Printf("Waiting for cache sync for GVR: %s", gvr.String())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // Augmenté à 30s
	defer cancel()

	if !cache.WaitForCacheSync(ctx.Done(), inf.Informer().HasSynced) {
		log.Printf("Cache sync failed for GVR: %s", gvr.String())
		close(stop)
		return nil
	}

	log.Printf("Cache sync successful for GVR: %s", gvr.String())
	e.lister[key] = inf.Lister()
	e.stopCh[key] = stop
	return inf.Lister()
}

func labelOf(obj runtime.Object, key string) string {
	if obj == nil {
		return ""
	}
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

func main() {
	flag.Parse()

	loggerApplication := log.New(os.Stderr, "", log.LstdFlags)
	loggerEvent := log.New(os.Stdout, "", 0)

	// Using First sample from https://pkg.go.dev/k8s.io/client-go/tools/clientcmd to automatically deal with environment variables and default file paths

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// if you want to change the loading rules (which files in which order), you can do so here

	configOverrides := &clientcmd.ConfigOverrides{}
	// if you want to change override values or bind them to flags, there are methods to help you

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		loggerApplication.Panicln(err.Error())
	}

	// Note that this *should* automatically sanitize sensitive fields
	loggerApplication.Println("Using configuration:", config.String())

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		loggerApplication.Panicln(err.Error())
	}

	initMetrics()

	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		loggerApplication.Fatalf("kubeconfig: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		loggerApplication.Fatalf("dynamic client: %v", err)
	}

	enr := newEnricher(dynClient, *resyncPeriod)
	defer enr.Close()

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
			AddFunc: func(obj interface{}) {
				if *ignoreNormal && obj.(*corev1.Event).Type == corev1.EventTypeNormal {
					return
				}
				onEvent(obj.(*corev1.Event), loggerEvent, enr, loggerApplication)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if *ignoreUpdate || (*ignoreNormal && newObj.(*corev1.Event).Type == corev1.EventTypeNormal) {
					return
				}
				onEvent(newObj.(*corev1.Event), loggerEvent, enr, loggerApplication)
			},
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)
	select {}
}

func onEvent(evt *corev1.Event, logger *log.Logger, enr *Enricher, appLogger *log.Logger) {
	j, _ := json.Marshal(evt)
	logger.Printf("%s\n", string(j))

	if *metricsEnabled {
		pid, eid, sid := enr.Labels(evt, appLogger)
		incMetrics(evt, pid, eid, sid)
	}
}
