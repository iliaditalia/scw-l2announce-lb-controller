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

package l2lb

import (
	"context"
	"fmt"
	"slices"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// syncService reconciles one Service by "namespace/name" key:
// finalizer → IPAM IP → Cilium pool → lease holder → node MAC → attachment.
// Every step is idempotent; Scaleway is only mutated on an actual difference.
func (c *Controller) syncService(key string) (err error) {
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}
		metricReconciles.WithLabelValues(result).Inc()
	}()

	ctx := context.Background()

	obj, exists, err := c.serviceStore.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		// Cleanup already ran while the finalizer was still set.
		return nil
	}
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("could not cast object %s to v1.Service", key)
	}

	c.updateManagedServicesGauge()

	opted := optedIn(svc)
	if svc.DeletionTimestamp != nil || (!opted && hasFinalizer(svc)) {
		return c.cleanupService(ctx, svc)
	}
	if !opted {
		return nil
	}

	// 0. Finalizer first, so deletion can never race past cleanup.
	if !hasFinalizer(svc) {
		if err := patchService(ctx, c.clientSet, svc, func(s *v1.Service) {
			s.Finalizers = append(s.Finalizers, finalizerName)
		}); err != nil {
			return err
		}
	}

	// 1. IPAM IP.
	pnID := c.effectivePNID(svc)
	if pnID == "" {
		return fmt.Errorf("no private network ID: set -private-network-id or the %s annotation", annotationPNID)
	}
	ip, booked, err := c.ensureIP(svc, pnID)
	if err != nil {
		return err
	}
	if booked {
		if err := patchService(ctx, c.clientSet, svc, func(s *v1.Service) {
			if s.Annotations == nil {
				s.Annotations = map[string]string{}
			}
			s.Annotations[annotationIPID] = ip.ID
		}); err != nil {
			// The booked IP is tagged with the service UID, so a crash here
			// is recovered by the adoption path in ensureIP.
			return err
		}
		c.recorder.Eventf(svc, v1.EventTypeNormal, "IPAMIPReserved",
			"Reserved Scaleway IPAM IP %s (%s) on private network %s", ip.Address.IP, ip.ID, pnID)
	}

	// 2. Selector label + Cilium pool assigning the /32 to this service.
	if svc.Labels[poolLabelKey] != string(svc.UID) {
		if err := patchService(ctx, c.clientSet, svc, func(s *v1.Service) {
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			s.Labels[poolLabelKey] = string(svc.UID)
		}); err != nil {
			return err
		}
	}
	if err := c.ensurePool(ctx, svc, ip.Address.IP.String()+"/32"); err != nil {
		return err
	}

	// 3. L2-announcement lease. Absent or holderless: the VIP is not (yet)
	// announced — leave the current attachment alone, the lease event will
	// re-enqueue us.
	leaseObj, exists, err := c.leaseStore.GetByKey(leaseNamespace + "/" + leaseNameFor(svc))
	if err != nil {
		return err
	}
	if !exists {
		klog.V(2).Infof("service %s: lease %s not found yet, waiting for Cilium", key, leaseNameFor(svc))
		return nil
	}
	lease, ok := leaseObj.(*coordinationv1.Lease)
	if !ok {
		return fmt.Errorf("could not cast object to coordination/v1 Lease")
	}
	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if holder == "" {
		klog.V(2).Infof("service %s: lease %s has no holder, not touching the attachment", key, lease.Name)
		return nil
	}

	// 4. Lease holder node → Scaleway server → NIC MAC on the target PN.
	// Fetched on demand (rare) rather than kept in an informer cache.
	node, err := c.clientSet.CoreV1().Nodes().Get(ctx, holder, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting lease holder node %s (churn?), requeueing: %w", holder, err)
	}
	mac, err := c.nodeMACOnPN(node, pnID)
	if err != nil {
		return err
	}

	// 5. Point the IPAM attachment at the holder's MAC.
	return c.ensureAttachment(svc, ip, mac)
}

// cleanupService runs on deletion and on opt-out: delete the pool, release
// (controller-booked) or detach (user-provided) the IPAM IP, then drop the
// finalizer. On opt-out the controller's annotations/labels are stripped too.
func (c *Controller) cleanupService(ctx context.Context, svc *v1.Service) error {
	if err := c.deletePool(ctx, svc); err != nil {
		return err
	}
	external := svc.Annotations[annotationIPExternallyManaged] == "true"
	if ipID := svc.Annotations[annotationIPID]; ipID != "" {
		if err := c.releaseOrDetachIP(ipID, external); err != nil {
			return err
		}
	}
	metricDivergence.DeleteLabelValues(svc.Namespace, svc.Name)

	return patchService(ctx, c.clientSet, svc, func(s *v1.Service) {
		s.Finalizers = slices.DeleteFunc(s.Finalizers, func(f string) bool { return f == finalizerName })
		if s.DeletionTimestamp == nil { // opt-out, not deletion
			if !external { // external IP IDs are user config, keep them
				delete(s.Annotations, annotationIPID)
			}
			delete(s.Labels, poolLabelKey)
		}
	})
}

func hasFinalizer(svc *v1.Service) bool {
	return slices.Contains(svc.Finalizers, finalizerName)
}

func (c *Controller) updateManagedServicesGauge() {
	n := 0
	for _, o := range c.serviceStore.List() {
		if svc, ok := o.(*v1.Service); ok && optedIn(svc) {
			n++
		}
	}
	metricManagedServices.Set(float64(n))
}
