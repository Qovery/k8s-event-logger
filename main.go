package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ignoreNormal = flag.Bool("ignore-normal", false, "ignore events of type 'Normal' to reduce noise")
	ignoreUpdate = flag.Bool("ignore-update", true, "ignore update of events")
	metricsPort  = flag.String("metrics-port", "8080", "port for prometheus metrics endpoint")
)

var (
	eventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "q_k8s_events_total",
			Help: "Total number of Kubernetes events processed",
		},
		[]string{"type", "reason", "namespace", "object_kind", "object_name"},
	)

	eventsProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "q_k8s_events_processed_total",
			Help: "Total number of events processed by the watcher",
		},
	)

	eventsIgnored = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "q_k8s_events_ignored_total",
			Help: "Total number of events ignored due to filters",
		},
	)
)

func main() {
	flag.Parse()

	loggerApplication := log.New(os.Stderr, "", log.LstdFlags)
	loggerEvent := log.New(os.Stdout, "", 0)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		loggerApplication.Printf("Starting metrics server on port %s", *metricsPort)
		if err := http.ListenAndServe(":"+*metricsPort, nil); err != nil {
			loggerApplication.Printf("Failed to start metrics server: %v", err)
		}
	}()

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
				logEvent(obj, loggerEvent)
				recordEventMetrics(obj.(*corev1.Event))
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				if *ignoreUpdate || (*ignoreNormal && newObj.(*corev1.Event).Type == corev1.EventTypeNormal) {
					return
				}
				logEvent(newObj, loggerEvent)
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

func recordEventMetrics(event *corev1.Event) {
	// Incrémenter le compteur total d'événements traités
	eventsProcessed.Inc()

	// Extraire les informations de l'objet concerné
	objectKind := event.InvolvedObject.Kind
	objectName := event.InvolvedObject.Name
	namespace := event.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Incrémenter le compteur avec les labels spécifiques
	eventsTotal.WithLabelValues(
		event.Type,
		event.Reason,
		namespace,
		objectKind,
		objectName,
	).Inc()
}
