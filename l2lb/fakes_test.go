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
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/ipam/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

// fakeIPAM is an in-memory IPAMAPI recording every call.
type fakeIPAM struct {
	ips    map[string]*ipam.IP
	calls  []string
	nextIP string
	nextID int
}

func newFakeIPAM() *fakeIPAM {
	return &fakeIPAM{ips: map[string]*ipam.IP{}, nextIP: "172.30.192.10"}
}

// mutations returns the mutating calls made (reads filtered out).
func (f *fakeIPAM) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GetIP") && !strings.HasPrefix(c, "ListIPs") {
			out = append(out, c)
		}
	}
	return out
}

func notFoundErr(id string) error {
	return &scw.ResourceNotFoundError{Resource: "ip", ResourceID: id}
}

func ipNet(addr string) scw.IPNet {
	return scw.IPNet{IPNet: net.IPNet{IP: net.ParseIP(addr), Mask: net.CIDRMask(22, 32)}}
}

func (f *fakeIPAM) BookIP(req *ipam.BookIPRequest, _ ...scw.RequestOption) (*ipam.IP, error) {
	f.calls = append(f.calls, "BookIP")
	f.nextID++
	ip := &ipam.IP{
		ID:      fmt.Sprintf("ip-%d", f.nextID),
		Address: ipNet(f.nextIP),
		Source:  req.Source,
		Tags:    req.Tags,
	}
	if req.Resource != nil {
		ip.Resource = &ipam.Resource{Type: ipam.ResourceTypeCustom, MacAddress: scw.StringPtr(req.Resource.MacAddress)}
	}
	f.ips[ip.ID] = ip
	return ip, nil
}

func (f *fakeIPAM) GetIP(req *ipam.GetIPRequest, _ ...scw.RequestOption) (*ipam.IP, error) {
	f.calls = append(f.calls, "GetIP")
	ip, ok := f.ips[req.IPID]
	if !ok {
		return nil, notFoundErr(req.IPID)
	}
	return ip, nil
}

func (f *fakeIPAM) ListIPs(req *ipam.ListIPsRequest, _ ...scw.RequestOption) (*ipam.ListIPsResponse, error) {
	f.calls = append(f.calls, "ListIPs")
	resp := &ipam.ListIPsResponse{}
	for _, ip := range f.ips {
		if req.PrivateNetworkID != nil {
			if ip.Source == nil || ip.Source.PrivateNetworkID == nil || *ip.Source.PrivateNetworkID != *req.PrivateNetworkID {
				continue
			}
		}
		match := true
		for _, tag := range req.Tags {
			if !slices.Contains(ip.Tags, tag) {
				match = false
				break
			}
		}
		if match {
			resp.IPs = append(resp.IPs, ip)
		}
	}
	resp.TotalCount = uint64(len(resp.IPs))
	return resp, nil
}

func (f *fakeIPAM) AttachIP(req *ipam.AttachIPRequest, _ ...scw.RequestOption) (*ipam.IP, error) {
	f.calls = append(f.calls, "AttachIP:"+req.Resource.MacAddress)
	ip, ok := f.ips[req.IPID]
	if !ok {
		return nil, notFoundErr(req.IPID)
	}
	ip.Resource = &ipam.Resource{Type: ipam.ResourceTypeCustom, MacAddress: scw.StringPtr(req.Resource.MacAddress)}
	return ip, nil
}

func (f *fakeIPAM) DetachIP(req *ipam.DetachIPRequest, _ ...scw.RequestOption) (*ipam.IP, error) {
	f.calls = append(f.calls, "DetachIP:"+req.Resource.MacAddress)
	ip, ok := f.ips[req.IPID]
	if !ok {
		return nil, notFoundErr(req.IPID)
	}
	ip.Resource = nil
	return ip, nil
}

func (f *fakeIPAM) MoveIP(req *ipam.MoveIPRequest, _ ...scw.RequestOption) (*ipam.IP, error) {
	f.calls = append(f.calls, fmt.Sprintf("MoveIP:%s->%s", req.FromResource.MacAddress, req.ToResource.MacAddress))
	ip, ok := f.ips[req.IPID]
	if !ok {
		return nil, notFoundErr(req.IPID)
	}
	ip.Resource = &ipam.Resource{Type: ipam.ResourceTypeCustom, MacAddress: scw.StringPtr(req.ToResource.MacAddress)}
	return ip, nil
}

func (f *fakeIPAM) ReleaseIP(req *ipam.ReleaseIPRequest, _ ...scw.RequestOption) error {
	f.calls = append(f.calls, "ReleaseIP")
	if _, ok := f.ips[req.IPID]; !ok {
		return notFoundErr(req.IPID)
	}
	delete(f.ips, req.IPID)
	return nil
}

// fakeInstance is an in-memory InstanceAPI.
type fakeInstance struct {
	servers map[string]*instance.Server
}

func newFakeInstance() *fakeInstance {
	return &fakeInstance{servers: map[string]*instance.Server{}}
}

func (f *fakeInstance) GetServer(req *instance.GetServerRequest, _ ...scw.RequestOption) (*instance.GetServerResponse, error) {
	srv, ok := f.servers[req.ServerID]
	if !ok {
		return nil, &scw.ResourceNotFoundError{Resource: "server", ResourceID: req.ServerID}
	}
	return &instance.GetServerResponse{Server: srv}, nil
}
