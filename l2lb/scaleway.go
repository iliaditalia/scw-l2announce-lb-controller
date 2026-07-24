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
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/ipam/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	v1core "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// managedByTag marks IPAM IPs booked by this controller: only those are
	// released on cleanup, user-provided IPs are merely detached.
	managedByTag = "managed-by=scw-l2announce-lb-controller"
	// serviceUIDTagPrefix ties a booked IP to its Service, enabling adoption
	// when the controller crashed between BookIP and the annotation patch.
	serviceUIDTagPrefix = "service-uid="
	// serviceNameTagPrefix is informational (console readability).
	serviceNameTagPrefix = "service="

	providerPrefix = "scaleway://instance/"
)

// IPAMAPI is the subset of the Scaleway IPAM API used by the controller.
type IPAMAPI interface {
	BookIP(req *ipam.BookIPRequest, opts ...scw.RequestOption) (*ipam.IP, error)
	GetIP(req *ipam.GetIPRequest, opts ...scw.RequestOption) (*ipam.IP, error)
	ListIPs(req *ipam.ListIPsRequest, opts ...scw.RequestOption) (*ipam.ListIPsResponse, error)
	AttachIP(req *ipam.AttachIPRequest, opts ...scw.RequestOption) (*ipam.IP, error)
	DetachIP(req *ipam.DetachIPRequest, opts ...scw.RequestOption) (*ipam.IP, error)
	MoveIP(req *ipam.MoveIPRequest, opts ...scw.RequestOption) (*ipam.IP, error)
	ReleaseIP(req *ipam.ReleaseIPRequest, opts ...scw.RequestOption) error
}

// InstanceAPI is the subset of the Scaleway Instance API used by the controller.
type InstanceAPI interface {
	GetServer(req *instance.GetServerRequest, opts ...scw.RequestOption) (*instance.GetServerResponse, error)
}

func isNotFound(err error) bool {
	notFound := &scw.ResourceNotFoundError{}
	return errors.As(err, &notFound)
}

// serverInfoFromProviderID parses a Kapsule node providerID of the form
// scaleway://instance/<zone>/<instance-uuid>. Other products (baremetal, ...)
// are rejected.
func serverInfoFromProviderID(providerID string) (zone scw.Zone, serverID string, err error) {
	rest, ok := strings.CutPrefix(providerID, providerPrefix)
	if !ok {
		return "", "", fmt.Errorf("provider ID %q is not of the form %s<zone>/<uuid>", providerID, providerPrefix)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("provider ID %q is not of the form %s<zone>/<uuid>", providerID, providerPrefix)
	}
	zone, err = scw.ParseZone(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("provider ID %q: %w", providerID, err)
	}
	return zone, parts[1], nil
}

// nodeMACOnPN resolves the MAC of the node's private NIC attached to the
// given private network — the NIC Cilium announces the VIP on.
func (c *Controller) nodeMACOnPN(node *v1core.Node, pnID string) (string, error) {
	if node.Spec.ProviderID == "" {
		return "", fmt.Errorf("node %s has no providerID", node.Name)
	}
	zone, serverID, err := serverInfoFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return "", err
	}
	resp, err := c.instanceAPI.GetServer(&instance.GetServerRequest{Zone: zone, ServerID: serverID})
	if err != nil {
		return "", fmt.Errorf("getting server %s for node %s: %w", serverID, node.Name, err)
	}

	var macs []string
	for _, nic := range resp.Server.PrivateNics {
		if nic.PrivateNetworkID == pnID {
			macs = append(macs, nic.MacAddress)
		}
	}
	switch len(macs) {
	case 1:
		return macs[0], nil
	case 0:
		return "", fmt.Errorf("node %s (server %s) has no private NIC on private network %s", node.Name, serverID, pnID)
	default:
		return "", fmt.Errorf("node %s (server %s) has %d private NICs on private network %s, cannot pick one", node.Name, serverID, len(macs), pnID)
	}
}

// ensureIP returns the IPAM IP for the service, booking one if needed. The
// booked IP's ID is persisted as an annotation by the caller. Booked IPs are
// tagged so they can be adopted after a crash and released on cleanup.
func (c *Controller) ensureIP(svc *v1core.Service, pnID string) (ip *ipam.IP, booked bool, err error) {
	if ipID := svc.Annotations[annotationIPID]; ipID != "" {
		ip, err := c.ipamAPI.GetIP(&ipam.GetIPRequest{IPID: ipID})
		if err != nil {
			if isNotFound(err) {
				// Never silently re-book: the ID may be a user-provided IP.
				return nil, false, fmt.Errorf("IPAM IP %s referenced by annotation %s not found", ipID, annotationIPID)
			}
			return nil, false, err
		}
		return ip, false, nil
	}

	uidTag := serviceUIDTagPrefix + string(svc.UID)

	// Adoption: a previous run may have booked an IP but crashed before
	// persisting the annotation.
	list, err := c.ipamAPI.ListIPs(&ipam.ListIPsRequest{
		PrivateNetworkID: &pnID,
		Tags:             []string{uidTag},
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing IPAM IPs for adoption: %w", err)
	}
	if len(list.IPs) == 1 {
		return list.IPs[0], true, nil
	}
	if len(list.IPs) > 1 {
		return nil, false, fmt.Errorf("found %d IPAM IPs tagged %s, expected at most one — manual cleanup required", len(list.IPs), uidTag)
	}

	ip, err = c.ipamAPI.BookIP(&ipam.BookIPRequest{
		Source: &ipam.Source{PrivateNetworkID: &pnID},
		Tags: []string{
			managedByTag,
			uidTag,
			serviceNameTagPrefix + svc.Namespace + "/" + svc.Name,
		},
	})
	if err != nil {
		return nil, false, fmt.Errorf("booking IPAM IP on private network %s: %w", pnID, err)
	}
	metricMutations.WithLabelValues("book").Inc()
	return ip, true, nil
}

// ensureAttachment points the IPAM IP's custom-resource attachment at mac.
// Mutates only on an actual difference.
func (c *Controller) ensureAttachment(svc *v1core.Service, ip *ipam.IP, mac string) error {
	switch {
	case ip.Resource == nil:
		metricDivergence.WithLabelValues(svc.Namespace, svc.Name).Set(1)
		if _, err := c.ipamAPI.AttachIP(&ipam.AttachIPRequest{
			IPID:     ip.ID,
			Resource: &ipam.CustomResource{MacAddress: mac},
		}); err != nil {
			return fmt.Errorf("attaching IPAM IP %s to MAC %s: %w", ip.ID, mac, err)
		}
		metricMutations.WithLabelValues("attach").Inc()
		klog.Infof("service %s/%s: attached IPAM IP %s (%s) to MAC %s", svc.Namespace, svc.Name, ip.ID, ip.Address.IP, mac)

	case ip.Resource.Type == ipam.ResourceTypeCustom:
		current := ""
		if ip.Resource.MacAddress != nil {
			current = *ip.Resource.MacAddress
		}
		if strings.EqualFold(current, mac) {
			metricDivergence.WithLabelValues(svc.Namespace, svc.Name).Set(0)
			return nil
		}
		metricDivergence.WithLabelValues(svc.Namespace, svc.Name).Set(1)
		if current == "" {
			return fmt.Errorf("IPAM IP %s has a custom-resource attachment without a MAC, cannot move it", ip.ID)
		}
		if _, err := c.ipamAPI.MoveIP(&ipam.MoveIPRequest{
			IPID:         ip.ID,
			FromResource: &ipam.CustomResource{MacAddress: current},
			ToResource:   &ipam.CustomResource{MacAddress: mac},
		}); err != nil {
			return fmt.Errorf("moving IPAM IP %s from MAC %s to MAC %s: %w", ip.ID, current, mac, err)
		}
		metricMutations.WithLabelValues("move").Inc()
		klog.Infof("service %s/%s: moved IPAM IP %s (%s) from MAC %s to MAC %s", svc.Namespace, svc.Name, ip.ID, ip.Address.IP, current, mac)

	default:
		c.recorder.Eventf(svc, v1core.EventTypeWarning, "IPAMIPAttachedElsewhere",
			"IPAM IP %s is attached to a %s resource (%s), refusing to steal it", ip.ID, ip.Resource.Type, ip.Resource.ID)
		return fmt.Errorf("IPAM IP %s is attached to a %s resource (%s), refusing to steal it", ip.ID, ip.Resource.Type, ip.Resource.ID)
	}

	metricDivergence.WithLabelValues(svc.Namespace, svc.Name).Set(0)
	return nil
}

// releaseOrDetachIP is the cleanup counterpart of ensureIP/ensureAttachment:
// controller-booked IPs (managed-by tag) are released; user-provided IPs are
// only detached from their custom-resource MAC.
func (c *Controller) releaseOrDetachIP(ipID string) error {
	ip, err := c.ipamAPI.GetIP(&ipam.GetIPRequest{IPID: ipID})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}

	if slices.Contains(ip.Tags, managedByTag) {
		// Scaleway refuses to release an IP still attached to a resource.
		if err := c.detachCustom(ip); err != nil {
			return err
		}
		if err := c.ipamAPI.ReleaseIP(&ipam.ReleaseIPRequest{IPID: ip.ID}); err != nil && !isNotFound(err) {
			return fmt.Errorf("releasing IPAM IP %s: %w", ip.ID, err)
		}
		metricMutations.WithLabelValues("release").Inc()
		klog.Infof("released IPAM IP %s (%s)", ip.ID, ip.Address.IP)
		return nil
	}

	return c.detachCustom(ip)
}

// detachCustom detaches ip from its custom-resource MAC, if any.
func (c *Controller) detachCustom(ip *ipam.IP) error {
	if ip.Resource == nil || ip.Resource.Type != ipam.ResourceTypeCustom || ip.Resource.MacAddress == nil {
		return nil
	}
	mac := *ip.Resource.MacAddress
	if _, err := c.ipamAPI.DetachIP(&ipam.DetachIPRequest{
		IPID:     ip.ID,
		Resource: &ipam.CustomResource{MacAddress: mac},
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("detaching IPAM IP %s: %w", ip.ID, err)
	}
	metricMutations.WithLabelValues("detach").Inc()
	klog.Infof("detached IPAM IP %s (%s) from MAC %s", ip.ID, ip.Address.IP, mac)
	return nil
}
