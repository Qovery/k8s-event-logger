package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery/cached/memory"

	"github.com/hashicorp/golang-lru/v2"
)

var (
	ignoreNormal   = flag.Bool("ignore-normal", false, "ignore events of type 'Normal' to reduce noise")
	ignoreUpdate   = flag.Bool("ignore-update", true, "ignore update of events")
	metricsEnabled = flag.Bool("metrics-enabled", false, "expose Prometheus metrics on :8080/metrics")
)

var (
	eventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_event_logger_q_k8s_events_total",
			Help: "Total number of Kubernetes events processed",
		},
		[]string{"type", "reason", "object_kind", "qovery_project_id", "qovery_environment_id", "qovery_service_id"},
	)
	eventsHit = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_event_logger_q_cache_event_total",
			Help: "",
		},
		[]string{"cache_type"},
	)
)

func main() {
	flag.Parse()

	loggerApplication := log.New(os.Stderr, "", log.LstdFlags)
	loggerEvent := log.New(os.Stdout, "", 0)

	if *metricsEnabled {
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			loggerApplication.Printf("Prometheus endpoint listening on :8080/metrics")
			if err := http.ListenAndServe(":8080", nil); err != nil {
				loggerApplication.Fatalf("metrics server: %v", err)
			}
		}()
	}

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

	fetcher, err := NewObjectLabelFetcher(config, 10_000, 30*time.Minute)
	if err != nil {
		loggerApplication.Fatalf("failed to build label fetcher: %v", err)
	}

	// Note that this *should* automatically sanitize sensitive fields
	loggerApplication.Println("Using configuration:", config.String())

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		loggerApplication.Panicln(err.Error())
	}

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
				evt := obj.(*corev1.Event)
				if *ignoreNormal && evt.Type == corev1.EventTypeNormal {
					return
				}
				logEvent(obj, loggerEvent)
				recordMetric(evt, fetcher)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				evt := newObj.(*corev1.Event)
				if *ignoreUpdate || (*ignoreNormal && evt.Type == corev1.EventTypeNormal) {
					return
				}
				logEvent(newObj, loggerEvent)
				recordMetric(evt, fetcher)
			},
		},
	)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)
	select {}
}

func logEvent(obj interface{}, logger *log.Logger) {
	j, _ := json.Marshal(obj)
	logger.Printf("%s\n", string(j))
}

func recordMetric(evt *corev1.Event, fetcher *ObjectLabelFetcher) {
	if *metricsEnabled {
		qoveryProjectId := ""
		qoveryEnvId := ""
		qoveryServiceId := ""

		labels, err := fetcher.LabelsForEvent(context.Background(), evt)
		if err == nil {
			qoveryProjectId = labels["qovery.com/project-id"]
			qoveryEnvId = labels["qovery.com/environment-id"]
			qoveryServiceId = labels["qovery.com/service-id"]
		}

		eventsTotal.
			WithLabelValues(evt.Type, evt.Reason, evt.InvolvedObject.Kind, qoveryProjectId, qoveryEnvId, qoveryServiceId).
			Inc()
	}
}

type cacheKey string

func keyFromEvent(evt *corev1.Event) cacheKey {
	return cacheKey(evt.InvolvedObject.UID)
}

type ObjectLabelFetcher struct {
	dynClient dynamic.Interface
	mapper    meta.RESTMapper
	cache     labelCache[cacheKey, map[string]string]
}

type labelCache[K comparable, V any] interface {
	Get(K) (V, bool)
	Add(K, V) bool
}

func NewObjectLabelFetcher(
	cfg *rest.Config,
	maxEntries int,
	ttl time.Duration,
) (*ObjectLabelFetcher, error) {
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	// ----- creating the LRU cache (with or without TTL) -----
	var c labelCache[cacheKey, map[string]string]
	if ttl > 0 {
		c = expirable.NewLRU[cacheKey, map[string]string](maxEntries, nil, ttl)
	} else {
		lruCache, err := lru.New[cacheKey, map[string]string](maxEntries)
		if err != nil {
			return nil, err
		}
		c = lruCache
	}

	return &ObjectLabelFetcher{
		dynClient: dynClient,
		mapper:    mapper,
		cache:     c,
	}, nil
}

func (f *ObjectLabelFetcher) LabelsForEvent(ctx context.Context, evt *corev1.Event) (map[string]string, error) {
	key := keyFromEvent(evt)

	// Fast-path: cache hit
	if lbls, ok := f.cache.Get(key); ok {
		eventsHit.WithLabelValues("hit").Inc()
		return lbls, nil
	}
	eventsHit.WithLabelValues("miss").Inc()

	// Resolve the GroupVersionKind from the Event.
	gvk := schema.FromAPIVersionAndKind(evt.InvolvedObject.APIVersion, evt.InvolvedObject.Kind)

	// Translate GVK ➜ GroupVersionResource via the RESTMapper.
	mapping, err := f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	// Pick the correct dynamic ResourceInterface (namespaced or cluster-wide).
	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		dr = f.dynClient.Resource(mapping.Resource).Namespace(evt.InvolvedObject.Namespace)
	} else {
		dr = f.dynClient.Resource(mapping.Resource)
	}

	// Retrieve the actual object (no need to unmarshal into a typed struct).
	obj, err := dr.Get(ctx, evt.InvolvedObject.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Use the meta.Accessor helper to read generic metadata, including labels.
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	labels := accessor.GetLabels()

	// Store in cache (eviction handled automatically)
	f.cache.Add(key, labels)

	return labels, nil
}
