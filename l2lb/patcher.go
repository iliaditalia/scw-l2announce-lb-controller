/*
Copyright 2018 Scaleway
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
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
)

// patchService applies mutate to a deep copy of svc and sends the resulting
// two-way strategic merge patch. No-op if mutate changes nothing. Pass
// "status" as subresource when mutate touches svc.Status.
// Adapted from the Scaleway cloud-controller-manager (scaleway/patcher.go).
func patchService(ctx context.Context, kclient clientset.Interface, svc *v1.Service, mutate func(*v1.Service), subresources ...string) error {
	final := svc.DeepCopy()
	mutate(final)

	originJSON, err := json.Marshal(svc)
	if err != nil {
		return fmt.Errorf("failed to serialize original service: %w", err)
	}
	updatedJSON, err := json.Marshal(final)
	if err != nil {
		return fmt.Errorf("failed to serialize updated service: %w", err)
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(originJSON, updatedJSON, v1.Service{})
	if err != nil {
		return fmt.Errorf("failed to create 2-way merge patch: %w", err)
	}
	if len(patch) == 0 || string(patch) == "{}" {
		return nil
	}

	_, err = kclient.CoreV1().Services(svc.Namespace).Patch(ctx, svc.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, subresources...)
	if err != nil {
		return fmt.Errorf("failed to patch service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}
