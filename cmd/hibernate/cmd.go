package main

import (
	goflag "flag"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	osclient "github.com/openshift/client-go/apps/clientset/versioned"
	"github.com/openshift/online-hibernation/pkg/cache"
	"github.com/openshift/online-hibernation/pkg/forcesleep"
	"github.com/openshift/online-hibernation/pkg/idling"
	"github.com/prometheus/client_golang/api/prometheus"
	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/resource"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	discocache "k8s.io/client-go/discovery/cached" // Saturday Night Fever

	"github.com/golang/glog"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	kclient "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
)

func createClients() (*restclient.Config, kclient.Interface, error) {
	return CreateClientsForConfig()
}

// CreateClientsForConfig creates and returns OpenShift and Kubernetes clients (as well as other useful
// client objects) for the given client config.
func CreateClientsForConfig() (*restclient.Config, kclient.Interface, error) {

	clientConfig, err := restclient.InClusterConfig()
	if err != nil {
		glog.V(1).Infof("Error creating in-cluster config: %s", err)
	}

	clientConfig.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: cache.Codecs}
	kc := kclient.NewForConfigOrDie(clientConfig)
	return clientConfig, kc, err
}

func setupPprof(mux *http.ServeMux) {
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
}

func main() {
	log.SetOutput(os.Stdout)
	var quota, period, sleepSyncPeriod, idleSyncPeriod, idleQueryPeriod, projectSleepPeriod time.Duration
	var workers, threshold int
	var cfgFile, termQuota, nontermQuota, prometheusURL, metricsBindAddr string
	var idleDryRun, sleepDryRun, collectRuntime, collectCache, enableProfiling bool

	flag.DurationVar(&quota, "quota", 16*time.Hour, "Maximum quota-hours allowed in period before force sleep")
	flag.DurationVar(&period, "period", 24*time.Hour, "Length of period in hours for quota consumption")
	flag.DurationVar(&sleepSyncPeriod, "sleep-sync-period", 60*time.Minute, "Interval to sync project status")
	flag.DurationVarP(&projectSleepPeriod, "sleep-duration", "z", 8*time.Hour, "Length of time to apply force-sleep to projects over quota.")
	flag.BoolVar(&sleepDryRun, "sleep-dry-run", true, "Log which projects will be put into force-sleep but do not restrict them.")

	flag.IntVarP(&workers, "workers", "w", 10, "Number of workers to process project sync")
	flag.StringVar(&cfgFile, "config", "", "load configuration from file")

	flag.StringVar(&termQuota, "terminating", "", "Memory quota for terminating pods")
	flag.StringVar(&nontermQuota, "nonterminating", "", "Memory quota for non-terminating pods")

	flag.StringVar(&metricsBindAddr, "metrics-bind-addr", ":8080", "The address on localhost serving metrics - http://localhost:port/metrics")
	flag.BoolVar(&enableProfiling, "enable-profiling", false, "Whether or not to enable pprof debug endpoints on the same address as the metrics endpoint")

	flag.BoolVar(&collectRuntime, "collect-runtime", true, "Enable runtime metrics")
	flag.BoolVar(&collectCache, "collect-cache", true, "Enable cache metrics")
	flag.StringVar(&prometheusURL, "prometheus-url", "https://prometheus.openshift-devops-monitor.svc.cluster.local", "Prometheus url")

	flag.DurationVar(&idleSyncPeriod, "idle-sync-period", 10*time.Minute, "Interval to sync project idle status")
	flag.DurationVar(&idleQueryPeriod, "idle-query-period", 30*time.Minute, "Period to compare network activity")
	flag.IntVar(&threshold, "idle-threshold", 5000, "Minimun network traffic received (bytes) to avoid auto-idling")
	flag.BoolVar(&idleDryRun, "idle-dry-run", true, "Log which projects will be auto-idled but do not idle them.")

	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	flag.Parse()

	tQuota, err := resource.ParseQuantity(termQuota)
	if err != nil {
		glog.V(0).Infof("Error with terminating quota: %s", err)
		os.Exit(1)
	}
	ntQuota, err := resource.ParseQuantity(nontermQuota)
	if err != nil {
		glog.V(0).Infof("Error with non-terminating quota: %s", err)
		os.Exit(1)
	}

	//Set up clients
	restConfig, kubeClient, err := createClients()
	osClient := osclient.NewForConfigOrDie(restConfig)
	var prometheusClient prometheus.Client

	// @DirectXMan12 should be credited here for helping with the promCfg
	// Steal the transport from the client config -- it should have the right
	// certs, token, auth info, etc for connecting to the Prometheus OAuth proxy
	promClientConfig := restConfig
	promClientConfig.CAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
	transport, err := restclient.TransportFor(promClientConfig)
	if err != nil {
		glog.Fatalf("Error creating prometheus client config: %s", err)
	}

	promCfg := prometheus.Config{
		Address: prometheusURL,
		// TODO: technically dubious
		Transport: transport.(prometheus.CancelableTransport),
	}

	prometheusClient, err = prometheus.New(promCfg)
	if err != nil {
		glog.Fatalf("Error creating Prometheus client: %s", err)
	}

	c := make(chan struct{})

	// TODO: switch to an actual retrying RESTMapper when someone gets around to writing it
	cachedDiscovery := discocache.NewMemCacheClient(kubeClient.Discovery())
	restMapper := discovery.NewDeferredDiscoveryRESTMapper(cachedDiscovery, apimeta.InterfacesForUnstructured)
	restMapper.Reset()

	//Cache is a shared object that both Sleeper and Idler will hold a reference to and interact with
	cache := cache.NewCache(osClient, kubeClient, restConfig, restMapper)
	cache.Run(c)

	sleeperConfig := &forcesleep.SleeperConfig{
		Quota:              quota,
		Period:             period,
		SleepSyncPeriod:    sleepSyncPeriod,
		SyncWorkers:        workers,
		ProjectSleepPeriod: projectSleepPeriod,
		TermQuota:          tQuota,
		NonTermQuota:       ntQuota,
		DryRun:             sleepDryRun,
		QuotaClient:        kubeClient.CoreV1(),
	}

	sleeper := forcesleep.NewSleeper(sleeperConfig, cache)

	// Spawn metrics server and pprof endpoints
	go func() {
		metricsConfig := forcesleep.MetricsConfig{
			CollectRuntime: collectRuntime,
			CollectCache:   collectCache,
		}
		metricsHandlerBuilder := forcesleep.MetricsServer{
			Config:     metricsConfig,
			Controller: sleeper,
		}
		metricsHandler, err := metricsHandlerBuilder.Handler()
		if err != nil {
			glog.Fatalf("error setting up Prometheus metrics: %s", err)
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsHandler)
		if enableProfiling {
			setupPprof(mux)
		}

		httpServer := &http.Server{
			Addr:    metricsBindAddr,
			Handler: mux,
		}

		glog.Fatal(httpServer.ListenAndServe())
	}()

	sleeper.Run(c)

	idlerConfig := &idling.IdlerConfig{
		PrometheusClient:   prometheusClient,
		IdleSyncPeriod:     idleSyncPeriod,
		IdleQueryPeriod:    idleQueryPeriod,
		SyncWorkers:        workers,
		Threshold:          threshold,
		IdleDryRun:         idleDryRun,
		ProjectSleepPeriod: projectSleepPeriod,
	}

	idler := idling.NewIdler(idlerConfig, cache)
	idler.Run(c)
	<-c
}
