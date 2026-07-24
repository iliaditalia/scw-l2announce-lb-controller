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
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/ipam/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

const (
	testPN   = "pn-1"
	testUID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	macNode1 = "02:00:00:00:00:01"
	macNode2 = "02:00:00:00:00:02"
)

type fixture struct {
	c    *Controller
	ipam *fakeIPAM
	inst *fakeInstance
	kube *k8sfake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
}

func newFixture(t *testing.T, objs ...runtime.Object) *fixture {
	t.Helper()
	kube := k8sfake.NewSimpleClientset(objs...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{poolGVR: "CiliumLoadBalancerIPPoolList"})
	fIPAM := newFakeIPAM()
	fInst := newFakeInstance()

	c := New(kube, dyn, fIPAM, fInst, testPN, time.Minute)
	c.recorder = record.NewFakeRecorder(100)

	for _, o := range objs {
		var err error
		switch typed := o.(type) {
		case *v1.Service:
			err = c.serviceStore.Add(typed)
		case *coordinationv1.Lease:
			err = c.leaseStore.Add(typed)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{c: c, ipam: fIPAM, inst: fInst, kube: kube, dyn: dyn}
}

// sync runs syncService and refreshes the service indexer from the fake
// clientset afterwards, like a real informer would.
func (f *fixture) sync(t *testing.T, key string) error {
	t.Helper()
	err := f.c.syncService(key)
	ns, name, _ := cache.SplitMetaNamespaceKey(key)
	svc, getErr := f.kube.CoreV1().Services(ns).Get(context.Background(), name, metav1.GetOptions{})
	if getErr == nil {
		_ = f.c.serviceStore.Update(svc)
	}
	return err
}

func (f *fixture) service(t *testing.T) *v1.Service {
	t.Helper()
	svc, err := f.kube.CoreV1().Services("default").Get(context.Background(), "vip", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (f *fixture) addNode1AndNode2() {
	f.inst.servers["srv-1"] = &instance.Server{ID: "srv-1", PrivateNics: []*instance.PrivateNIC{
		{ID: "nic-1", PrivateNetworkID: testPN, MacAddress: macNode1},
		{ID: "nic-x", PrivateNetworkID: "pn-other", MacAddress: "02:00:00:00:00:ff"},
	}}
	f.inst.servers["srv-2"] = &instance.Server{ID: "srv-2", PrivateNics: []*instance.PrivateNIC{
		{ID: "nic-2", PrivateNetworkID: testPN, MacAddress: macNode2},
	}}
}

func testService(mods ...func(*v1.Service)) *v1.Service {
	lbClass := ciliumLBClass
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "vip",
			Namespace:   "default",
			UID:         types.UID(testUID),
			Annotations: map[string]string{annotationEnabled: "enabled"},
		},
		Spec: v1.ServiceSpec{
			Type:              v1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &lbClass,
		},
	}
	for _, m := range mods {
		m(svc)
	}
	return svc
}

func withFinalizer(svc *v1.Service) { svc.Finalizers = append(svc.Finalizers, finalizerName) }
func withIPID(id string) func(*v1.Service) {
	return func(s *v1.Service) { s.Annotations[annotationIPID] = id }
}
func withDeletion(svc *v1.Service) { now := metav1.Now(); svc.DeletionTimestamp = &now }
func withExternal(svc *v1.Service) { svc.Annotations[annotationIPExternallyManaged] = "true" }
func withoutOptIn(svc *v1.Service) { delete(svc.Annotations, annotationEnabled) }

func testNode(name, serverID string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.NodeSpec{ProviderID: "scaleway://instance/fr-par-2/" + serverID},
	}
}

func testLease(holder string) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "cilium-l2announce-default-vip", Namespace: leaseNamespace},
	}
	if holder != "" {
		lease.Spec.HolderIdentity = &holder
	}
	return lease
}

// seededIP registers an existing IPAM IP in the fake, optionally attached to
// a custom-resource MAC.
func (f *fixture) seededIP(id, addr, attachedMAC string, tags ...string) *ipam.IP {
	pn := testPN
	ip := &ipam.IP{ID: id, Address: ipNet(addr), Source: &ipam.Source{PrivateNetworkID: &pn}, Tags: tags}
	if attachedMAC != "" {
		ip.Resource = &ipam.Resource{Type: ipam.ResourceTypeCustom, MacAddress: scw.StringPtr(attachedMAC)}
	}
	f.ipam.ips[id] = ip
	return ip
}

func (f *fixture) getPool(t *testing.T) map[string]any {
	t.Helper()
	pool, err := f.dyn.Resource(poolGVR).Get(context.Background(), "scw-ipam-default-vip", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting pool: %v", err)
	}
	spec, _, _ := unstructured.NestedMap(pool.Object, "spec")
	return spec
}

func TestFreshOptIn(t *testing.T) {
	f := newFixture(t, testService())

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}

	if got := f.ipam.mutations(); !reflect.DeepEqual(got, []string{"BookIP"}) {
		t.Fatalf("expected only BookIP, got %v", got)
	}
	svc := f.service(t)
	if svc.Annotations[annotationIPID] != "ip-1" {
		t.Errorf("IP ID annotation not persisted: %v", svc.Annotations)
	}
	if !hasFinalizer(svc) {
		t.Error("finalizer not added")
	}
	if svc.Labels[poolLabelKey] != testUID {
		t.Errorf("pool label not stamped: %v", svc.Labels)
	}
	booked := f.ipam.ips["ip-1"]
	if !slices.Contains(booked.Tags, managedByTag) || !slices.Contains(booked.Tags, serviceUIDTagPrefix+testUID) {
		t.Errorf("booked IP missing ownership tags: %v", booked.Tags)
	}

	spec := f.getPool(t)
	blocks := spec["blocks"].([]any)
	if cidr := blocks[0].(map[string]any)["cidr"]; cidr != "172.30.192.10/32" {
		t.Errorf("pool cidr = %v, want 172.30.192.10/32", cidr)
	}
	selector := spec["serviceSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if selector[poolLabelKey] != testUID {
		t.Errorf("pool selector = %v", selector)
	}

	// Second pass with no lease: steady state, zero new mutations.
	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, []string{"BookIP"}) {
		t.Fatalf("second sync mutated: %v", got)
	}
}

func TestAttachOnLease(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testNode("node-1", "srv-1"),
		testLease("node-1"),
	)
	f.addNode1AndNode2()
	f.seededIP("ip-a", "172.30.192.10", "", managedByTag, serviceUIDTagPrefix+testUID)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, []string{"AttachIP:" + macNode1}) {
		t.Fatalf("expected attach to node-1 MAC, got %v", got)
	}
}

func TestMoveOnHolderChange(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testNode("node-1", "srv-1"),
		testNode("node-2", "srv-2"),
		testLease("node-2"),
	)
	f.addNode1AndNode2()
	f.seededIP("ip-a", "172.30.192.10", macNode1, managedByTag)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	want := []string{"MoveIP:" + macNode1 + "->" + macNode2}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestNoopWhenAttachmentCorrect(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a"), func(s *v1.Service) {
			s.Labels = map[string]string{poolLabelKey: testUID}
		}),
		testNode("node-1", "srv-1"),
		testLease("node-1"),
	)
	f.addNode1AndNode2()
	// Same MAC, different case: must still be a no-op.
	f.seededIP("ip-a", "172.30.192.10", "02:00:00:00:00:01", managedByTag)
	f.inst.servers["srv-1"].PrivateNics[0].MacAddress = "02:00:00:00:00:01"

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("expected no mutations, got %v", got)
	}
}

func TestEmptyHolderDoesNotThrash(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testLease(""), // failover in progress
	)
	f.seededIP("ip-a", "172.30.192.10", macNode1, managedByTag)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("expected no mutations with empty holder, got %v", got)
	}
}

func TestLeaseHolderNodeMissing(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testLease("node-gone"),
	)
	f.seededIP("ip-a", "172.30.192.10", macNode1, managedByTag)

	if err := f.sync(t, "default/vip"); err == nil {
		t.Fatal("expected error for missing node")
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("expected no mutations, got %v", got)
	}
}

func TestAmbiguousNICs(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testNode("node-1", "srv-1"),
		testLease("node-1"),
	)
	f.inst.servers["srv-1"] = &instance.Server{ID: "srv-1", PrivateNics: []*instance.PrivateNIC{
		{PrivateNetworkID: testPN, MacAddress: macNode1},
		{PrivateNetworkID: testPN, MacAddress: macNode2},
	}}
	f.seededIP("ip-a", "172.30.192.10", "", managedByTag)

	if err := f.sync(t, "default/vip"); err == nil {
		t.Fatal("expected error for ambiguous NICs")
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("expected no mutations, got %v", got)
	}
}

func TestAnnotatedIPMissingIsNotRebooked(t *testing.T) {
	f := newFixture(t, testService(withFinalizer, withIPID("ip-vanished")))

	if err := f.sync(t, "default/vip"); err == nil {
		t.Fatal("expected error for vanished annotated IP")
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("must never re-book an annotated IP, got %v", got)
	}
}

func TestAdoptionAfterCrash(t *testing.T) {
	// BookIP succeeded on a previous run, but the annotation patch never
	// landed: the IP is found again by its service-uid tag.
	f := newFixture(t, testService(withFinalizer))
	f.seededIP("ip-orphan", "172.30.192.10", "", managedByTag, serviceUIDTagPrefix+testUID)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("adoption must not mutate, got %v", got)
	}
	if got := f.service(t).Annotations[annotationIPID]; got != "ip-orphan" {
		t.Errorf("adopted IP not persisted, annotation = %q", got)
	}
}

func TestRefusesToStealForeignAttachment(t *testing.T) {
	f := newFixture(t,
		testService(withFinalizer, withIPID("ip-a")),
		testNode("node-1", "srv-1"),
		testLease("node-1"),
	)
	f.addNode1AndNode2()
	ip := f.seededIP("ip-a", "172.30.192.10", "")
	ip.Resource = &ipam.Resource{Type: ipam.ResourceTypeInstancePrivateNic, ID: "nic-foreign"}

	if err := f.sync(t, "default/vip"); err == nil {
		t.Fatal("expected error when IP is attached to a non-custom resource")
	}
	if got := f.ipam.mutations(); got != nil {
		t.Fatalf("expected no mutations, got %v", got)
	}
}

func TestDeletionReleasesManagedIP(t *testing.T) {
	f := newFixture(t, testService(withFinalizer, withIPID("ip-a"), withDeletion))
	f.seededIP("ip-a", "172.30.192.10", macNode1, managedByTag)
	// Pre-create the pool so deletion is observable.
	if err := f.c.ensurePool(context.Background(), testService(), "172.30.192.10/32"); err != nil {
		t.Fatal(err)
	}

	if err := f.c.syncService("default/vip"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DetachIP:" + macNode1, "ReleaseIP"}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	if _, err := f.dyn.Resource(poolGVR).Get(context.Background(), "scw-ipam-default-vip", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("pool not deleted: %v", err)
	}
	svc := f.service(t)
	if hasFinalizer(svc) {
		t.Error("finalizer not removed")
	}
}

func TestDeletionReleasesUnattachedManagedIP(t *testing.T) {
	f := newFixture(t, testService(withFinalizer, withIPID("ip-a"), withDeletion))
	f.seededIP("ip-a", "172.30.192.10", "", managedByTag) // not attached

	if err := f.c.syncService("default/vip"); err != nil {
		t.Fatal(err)
	}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, []string{"ReleaseIP"}) {
		t.Fatalf("expected ReleaseIP only, got %v", got)
	}
}

func TestDeletionDetachesUserProvidedIP(t *testing.T) {
	f := newFixture(t, testService(withFinalizer, withIPID("ip-user"), withDeletion))
	f.seededIP("ip-user", "172.30.192.99", macNode1) // no managed-by tag

	if err := f.c.syncService("default/vip"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DetachIP:" + macNode1}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v (detach only, never release), got %v", want, got)
	}
	if _, ok := f.ipam.ips["ip-user"]; !ok {
		t.Error("user-provided IP was released")
	}
}

func TestDeletionNeverReleasesExternalIP(t *testing.T) {
	// Adversarial case: the user-provided IP carries the managed-by tag
	// (e.g. it was once booked by the controller for another service).
	f := newFixture(t, testService(withFinalizer, withIPID("ip-ext"), withExternal, withDeletion))
	f.seededIP("ip-ext", "172.30.192.50", macNode1, managedByTag)

	if err := f.c.syncService("default/vip"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DetachIP:" + macNode1}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v (detach only, never release), got %v", want, got)
	}
	if _, ok := f.ipam.ips["ip-ext"]; !ok {
		t.Error("external IP was released")
	}
}

func TestExternalWithoutIPIDErrors(t *testing.T) {
	f := newFixture(t, testService(withExternal))

	if err := f.sync(t, "default/vip"); err == nil {
		t.Fatal("expected error for external annotation without IP ID")
	}
	if got := f.ipam.mutations(); len(got) != 0 {
		t.Fatalf("expected no mutations, got %v", got)
	}
}

func TestOptOutKeepsExternalAnnotations(t *testing.T) {
	f := newFixture(t, testService(withoutOptIn, withFinalizer, withIPID("ip-ext"), withExternal))
	f.seededIP("ip-ext", "172.30.192.50", macNode1)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	svc := f.service(t)
	if hasFinalizer(svc) {
		t.Error("finalizer not removed")
	}
	if svc.Annotations[annotationIPID] != "ip-ext" {
		t.Error("user's IP ID annotation was stripped on opt-out")
	}
	if svc.Annotations[annotationIPExternallyManaged] != "true" {
		t.Error("external annotation was stripped on opt-out")
	}
}

func TestOptOutCleansUp(t *testing.T) {
	f := newFixture(t, testService(withoutOptIn, withFinalizer, withIPID("ip-a"), func(s *v1.Service) {
		s.Labels = map[string]string{poolLabelKey: testUID}
	}))
	f.seededIP("ip-a", "172.30.192.10", macNode1, managedByTag)

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DetachIP:" + macNode1, "ReleaseIP"}
	if got := f.ipam.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	svc := f.service(t)
	if hasFinalizer(svc) {
		t.Error("finalizer not removed")
	}
	if _, ok := svc.Annotations[annotationIPID]; ok {
		t.Error("IP ID annotation not stripped on opt-out")
	}
	if _, ok := svc.Labels[poolLabelKey]; ok {
		t.Error("pool label not stripped on opt-out")
	}
}

func TestNotOptedInIsIgnored(t *testing.T) {
	f := newFixture(t, testService(withoutOptIn))

	if err := f.sync(t, "default/vip"); err != nil {
		t.Fatal(err)
	}
	if len(f.ipam.calls) != 0 {
		t.Fatalf("expected no IPAM calls at all, got %v", f.ipam.calls)
	}
}
