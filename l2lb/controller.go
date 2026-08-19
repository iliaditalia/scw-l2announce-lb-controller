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

// Package l2lb keeps a Scaleway IPAM IP's custom-resource MAC attachment in
// sync with the Cilium L2-announcement lease holder of a LoadBalancer
// service, so the Cilium-announced VIP is routable through the Scaleway VPC
// gateway (VPC peering / interlink), not just on the local L2 segment.
package l2lb

import (
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// ciliumLBClass is the loadBalancerClass handled by the Cilium L2 announcer.
	ciliumLBClass = "io.cilium/l2-announcer"

	// annotationEnabled opts a Service in ("enabled").
	annotationEnabled = "k8s.iliad.it/scw-ipam"
	// annotationIPID holds the IPAM IP ID: user-provided, or persisted by the
	// controller after booking one.
	annotationIPID = "k8s.iliad.it/scw-ipam-ip-id"
	// annotationPNID optionally overrides the controller-wide private network ID.
	annotationPNID = "k8s.iliad.it/scw-ipam-pn-id"
	// annotationIPExternallyManaged ("true", user-set only, never written by the
	// controller) marks the annotated IP as externally managed: it is only
	// ever detached on cleanup, never released, regardless of its tags.
	annotationIPExternallyManaged = "k8s.iliad.it/scw-ipam-ip-externally-managed"

	// finalizerName guards Scaleway-side cleanup on Service deletion.
	finalizerName = "k8s.iliad.it/scw-ipam-cleanup"

	// Cilium creates L2-announcement leases in kube-system, named after the
	// service: cilium-l2announce-<namespace>-<name>.
	leaseNamespace = metav1.NamespaceSystem
	leasePrefix    = "cilium-l2announce-"

	componentName = "scw-l2announce-lb-controller"
)

const (
	exponentialBaseDelay  = time.Second * 1
	exponentialMaxDelay   = time.Minute * 10
	exponentialMaxRetries = 30
)

// Controller watches LoadBalancer Services opted in via annotation and
// reconciles their Scaleway IPAM IP, externalIPs VIP and MAC attachment.
type Controller struct {
	clientSet   clientset.Interface
	ipamAPI     IPAMAPI
	instanceAPI InstanceAPI
	recorder    record.EventRecorder
	pnID        string

	serviceStore      cache.Store
	serviceController cache.Controller
	leaseStore        cache.Store
	leaseController   cache.Controller
	queue             workqueue.TypedRateLimitingInterface[string]
}

// New builds a Controller. pnID is the default Scaleway private network the
// VIPs are reserved from; resyncPeriod is the full drift-resync interval.
func New(clientSet clientset.Interface, ipamAPI IPAMAPI, instanceAPI InstanceAPI, pnID string, resyncPeriod time.Duration) *Controller {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientSet.CoreV1().Events("")})

	c := &Controller{
		clientSet:   clientSet,
		ipamAPI:     ipamAPI,
		instanceAPI: instanceAPI,
		recorder:    broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: componentName}),
		pnID:        pnID,
		queue:       workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[string](exponentialBaseDelay, exponentialMaxDelay)),
	}

	// Services: the resync period doubles as the periodic drift check.
	c.serviceStore, c.serviceController = cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: cache.NewListWatchFromClient(clientSet.CoreV1().RESTClient(), "services", v1.NamespaceAll, fields.Everything()),
		ObjectType:    &v1.Service{},
		ResyncPeriod:  resyncPeriod,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueService,
			UpdateFunc: func(_, newObj any) { c.enqueueService(newObj) },
			DeleteFunc: c.enqueueService,
		},
	})

	// Leases: failover fast path. Only holder changes are relevant — leases
	// renew every few seconds and must not thrash the queue.
	c.leaseStore, c.leaseController = cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: cache.NewListWatchFromClient(clientSet.CoordinationV1().RESTClient(), "leases", leaseNamespace, fields.Everything()),
		ObjectType:    &coordinationv1.Lease{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueServiceForLease,
			UpdateFunc: func(oldObj, newObj any) {
				if leaseHolder(oldObj) != leaseHolder(newObj) {
					c.enqueueServiceForLease(newObj)
				}
			},
			DeleteFunc: c.enqueueServiceForLease,
		},
	})

	return c
}

// enqueueService queues a Service if the controller has (or may have) work to
// do on it: opted in, or still carrying our finalizer/annotations (opt-out
// and deletion cleanup).
func (c *Controller) enqueueService(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	svc, ok := obj.(*v1.Service)
	if !ok {
		return
	}
	if !c.managed(svc) {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// enqueueServiceForLease maps a Cilium L2-announcement lease event back to
// its Service. The lease name is not parsed (both namespace and name may
// contain hyphens): instead the cached Services are scanned for the one whose
// computed lease name matches.
func (c *Controller) enqueueServiceForLease(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok {
		return
	}
	if !strings.HasPrefix(lease.Name, leasePrefix) {
		return
	}
	for _, o := range c.serviceStore.List() {
		svc, ok := o.(*v1.Service)
		if !ok {
			continue
		}
		if leaseNameFor(svc) == lease.Name {
			c.enqueueService(svc)
			return
		}
	}
}

func leaseNameFor(svc *v1.Service) string {
	return leasePrefix + svc.Namespace + "-" + svc.Name
}

func leaseHolder(obj any) string {
	lease, ok := obj.(*coordinationv1.Lease)
	if !ok || lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// managed reports whether the Service is opted in or has controller-owned
// state left to clean up.
func (c *Controller) managed(svc *v1.Service) bool {
	return optedIn(svc) || svc.Annotations[annotationIPID] != "" || hasFinalizer(svc)
}

// optedIn reports whether the Service asks for IPAM VIP management.
func optedIn(svc *v1.Service) bool {
	return svc.Spec.Type == v1.ServiceTypeLoadBalancer &&
		svc.Spec.LoadBalancerClass != nil &&
		*svc.Spec.LoadBalancerClass == ciliumLBClass &&
		svc.Annotations[annotationEnabled] == "enabled"
}

// effectivePNID returns the per-service private network override, or the
// controller-wide default.
func (c *Controller) effectivePNID(svc *v1.Service) string {
	if pn := svc.Annotations[annotationPNID]; pn != "" {
		return pn
	}
	return c.pnID
}

// Run starts the informers and the single reconcile worker, blocking until
// stopCh is closed. A single worker serializes Scaleway mutations.
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer c.queue.ShutDown()

	go c.serviceController.Run(stopCh)
	go c.leaseController.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.serviceController.HasSynced, c.leaseController.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	klog.Info("caches synced, starting reconcile worker")
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncService(key)
	if err != nil {
		klog.Errorf("error syncing service %s: %v", key, err)
		if c.queue.NumRequeues(key) < exponentialMaxRetries {
			c.queue.AddRateLimited(key)
			return true
		}
	}
	c.queue.Forget(key)
	return true
}
