/*
Copyright 2026 Iliad

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
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/ipam/v1"
	"github.com/scaleway/scaleway-sdk-go/logger"
	"github.com/scaleway/scaleway-sdk-go/scw"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"github.com/iliaditalia/scw-l2announce-lb-controller/l2lb"
)

func main() {
	var (
		kubeconfig     string
		pnID           string
		resyncPeriod   time.Duration
		metricsAddr    string
		leaderElect    bool
		leaderElectNS  string
		versionAndExit bool
	)

	klog.InitFlags(nil)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Empty means in-cluster configuration.")
	flag.StringVar(&pnID, "private-network-id", os.Getenv("PN_ID"), "Scaleway private network ID VIPs are reserved from (falls back to the PN_ID environment variable).")
	flag.DurationVar(&resyncPeriod, "resync-period", 10*time.Minute, "Period of the full drift resync.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Listen address of the /metrics and /healthz endpoints.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election (run with >1 replica).")
	flag.StringVar(&leaderElectNS, "leader-election-namespace", os.Getenv("POD_NAMESPACE"), "Namespace of the leader-election lease (falls back to the POD_NAMESPACE environment variable).")
	flag.BoolVar(&versionAndExit, "version", false, "Print version and exit.")
	flag.Parse()

	if versionAndExit {
		_, _ = os.Stdout.WriteString(l2lb.Version() + "\n")
		return
	}
	if pnID == "" {
		klog.Fatal("-private-network-id (or the PN_ID environment variable) is required")
	}

	logger.SetLogger(l2lb.Logger{})

	scwClient, err := scw.NewClient(scw.WithEnv(), scw.WithUserAgent(l2lb.UserAgent()))
	if err != nil {
		klog.Fatalf("could not create Scaleway client: %v", err)
	}
	if _, set := scwClient.GetDefaultRegion(); !set {
		klog.Fatal("SCW_DEFAULT_REGION is required (IPAM is regional)")
	}
	if _, set := scwClient.GetDefaultProjectID(); !set {
		klog.Fatal("SCW_DEFAULT_PROJECT_ID is required (to book IPAM IPs)")
	}

	// Empty flags fall back to in-cluster config, then default kubeconfig rules.
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatalf("could not build kubernetes client configuration: %v", err)
	}
	clientSet := kubernetes.NewForConfigOrDie(config)

	controller := l2lb.New(clientSet, ipam.NewAPI(scwClient), instance.NewAPI(scwClient), pnID, resyncPeriod)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, func() { klog.Info("shutdown signal received") })

	// Metrics/healthz are served before (and regardless of) leader election
	// so standby replicas are probeable.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() {
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			klog.Fatalf("metrics server failed: %v", err)
		}
	}()

	klog.Infof("starting scw-l2announce-lb-controller %s (private network %s)", l2lb.Version(), pnID)

	if !leaderElect {
		controller.Run(ctx.Done())
		return
	}

	if leaderElectNS == "" {
		klog.Fatal("-leader-election-namespace (or the POD_NAMESPACE environment variable) is required with -leader-elect")
	}
	hostname, err := os.Hostname()
	if err != nil {
		klog.Fatalf("could not get hostname: %v", err)
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      "scw-l2announce-lb-controller",
				Namespace: leaderElectNS,
			},
			Client:     clientSet.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: hostname},
		},
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				controller.Run(ctx.Done())
			},
			OnStoppedLeading: func() {
				// Die and let the Deployment restart us; no graceful demotion.
				klog.Fatalf("leader election lost")
			},
		},
	})
}
