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
	"reflect"

	v1core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// poolGVR is the CiliumLoadBalancerIPPool resource; cluster-scoped, managed
// as unstructured to avoid importing Cilium (v2 is the storage version on
// Cilium >= 1.18).
var poolGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumloadbalancerippools"}

const (
	// poolLabelKey selects exactly one Service: the controller stamps it on
	// the Service (value = Service UID) and uses it in the pool's
	// serviceSelector.
	poolLabelKey = "ipam.k8s.iliad.it/pool"

	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "scw-l2announce-lb-controller"
)

func poolName(svc *v1core.Service) string {
	return fmt.Sprintf("scw-ipam-%s-%s", svc.Namespace, svc.Name)
}

// desiredPoolSpec is the spec assigning exactly the reserved /32 to exactly
// this service.
func desiredPoolSpec(svc *v1core.Service, cidr string) map[string]any {
	return map[string]any{
		"blocks": []any{
			map[string]any{"cidr": cidr},
		},
		"serviceSelector": map[string]any{
			"matchLabels": map[string]any{
				poolLabelKey: string(svc.UID),
			},
		},
	}
}

// ensurePool creates or updates the service's CiliumLoadBalancerIPPool.
// Update only happens when the spec actually differs.
func (c *Controller) ensurePool(ctx context.Context, svc *v1core.Service, cidr string) error {
	name := poolName(svc)
	spec := desiredPoolSpec(svc, cidr)

	existing, err := c.dynClient.Resource(poolGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		pool := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": poolGVR.Group + "/" + poolGVR.Version,
			"kind":       "CiliumLoadBalancerIPPool",
			"metadata": map[string]any{
				"name":   name,
				"labels": map[string]any{managedByLabelKey: managedByLabelValue},
			},
			"spec": spec,
		}}
		if _, err := c.dynClient.Resource(poolGVR).Create(ctx, pool, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating pool %s: %w", name, err)
		}
		klog.Infof("service %s/%s: created CiliumLoadBalancerIPPool %s (%s)", svc.Namespace, svc.Name, name, cidr)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting pool %s: %w", name, err)
	}

	currentSpec, _, err := unstructured.NestedMap(existing.Object, "spec")
	if err != nil {
		return fmt.Errorf("reading pool %s spec: %w", name, err)
	}
	// Compare only the fields we own; Cilium may default others (e.g. disabled).
	if reflect.DeepEqual(currentSpec["blocks"], spec["blocks"]) &&
		reflect.DeepEqual(currentSpec["serviceSelector"], spec["serviceSelector"]) {
		return nil
	}

	for k, v := range spec {
		currentSpec[k] = v
	}
	if err := unstructured.SetNestedMap(existing.Object, currentSpec, "spec"); err != nil {
		return fmt.Errorf("setting pool %s spec: %w", name, err)
	}
	if _, err := c.dynClient.Resource(poolGVR).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating pool %s: %w", name, err)
	}
	klog.Infof("service %s/%s: updated CiliumLoadBalancerIPPool %s (%s)", svc.Namespace, svc.Name, name, cidr)
	return nil
}

func (c *Controller) deletePool(ctx context.Context, svc *v1core.Service) error {
	err := c.dynClient.Resource(poolGVR).Delete(ctx, poolName(svc), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pool %s: %w", poolName(svc), err)
	}
	return nil
}
