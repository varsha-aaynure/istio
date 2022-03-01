// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model_test

import (
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/memory"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pilot/pkg/xds"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/test/scopes"
)

func TestGatewayHostnames(t *testing.T) {
	origMinGatewayTTL := model.MinGatewayTTL
	model.MinGatewayTTL = 3 * time.Second
	t.Cleanup(func() {
		model.MinGatewayTTL = origMinGatewayTTL
	})

	gwHost := "test.gw.istio.io"
	dnsServer := newFakeDNSServer(":10053", 1, sets.NewSet(gwHost))
	model.NetworkGatewayTestDNSServers = []string{"localhost:10053"}
	t.Cleanup(func() {
		if err := dnsServer.Shutdown(); err != nil {
			t.Logf("failed shutting down fake dns server")
		}
	})

	meshNetworks := mesh.NewFixedNetworksWatcher(nil)
	xdsUpdater := &xds.FakeXdsUpdater{Events: make(chan xds.FakeXdsEvent, 10)}
	env := &model.Environment{NetworksWatcher: meshNetworks, ServiceDiscovery: memory.NewServiceDiscovery()}
	if err := env.InitNetworksManager(xdsUpdater); err != nil {
		t.Fatal(err)
	}

	t.Run("initial resolution", func(t *testing.T) {
		meshNetworks.SetNetworks(&meshconfig.MeshNetworks{Networks: map[string]*meshconfig.Network{
			"nw0": {Gateways: []*meshconfig.Network_IstioNetworkGateway{{
				Gw: &meshconfig.Network_IstioNetworkGateway_Address{
					Address: gwHost,
				},
				Port: 15443,
			}}},
		}})
		xdsUpdater.WaitDurationOrFail(t, model.MinGatewayTTL+5*time.Second, "xds")
		gws := env.NetworkManager.AllGateways()
		if !reflect.DeepEqual(gws, []model.NetworkGateway{{Network: "nw0", Addr: "10.0.0.0", Port: 15443}}) {
			t.Fatalf("did not get expected gws: %v", gws)
		}
	})
	t.Run("re-resolve after TTL", func(t *testing.T) {
		if testing.Short() {
			t.Skip()
		}
		// wait for TTL + 5 to get an XDS update
		xdsUpdater.WaitDurationOrFail(t, model.MinGatewayTTL+5*time.Second, "xds")
		// after the update, we should see the next gateway (10.0.0.1)
		gws := env.NetworkManager.AllGateways()
		if !reflect.DeepEqual(gws, []model.NetworkGateway{{Network: "nw0", Addr: "10.0.0.1", Port: 15443}}) {
			t.Fatalf("did not get expected gws: %v", gws)
		}
	})
	t.Run("forget", func(t *testing.T) {
		meshNetworks.SetNetworks(nil)
		xdsUpdater.WaitDurationOrFail(t, 5*time.Second, "xds")
		if len(env.NetworkManager.AllGateways()) > 0 {
			t.Fatalf("expected no gateways")
		}
	})
}

type fakeDNSServer struct {
	*dns.Server
	ttl uint32

	mu sync.Mutex
	// map fqdn hostname -> query count
	hosts map[string]int
}

func newFakeDNSServer(addr string, ttl uint32, hosts sets.Set) *fakeDNSServer {
	s := &fakeDNSServer{
		Server: &dns.Server{Addr: addr, Net: "udp"},
		ttl:    ttl,
		hosts:  make(map[string]int, len(hosts)),
	}
	s.Handler = s

	for k := range hosts {
		s.hosts[dns.Fqdn(k)] = 0
	}

	go func() {
		if err := s.ListenAndServe(); err != nil {
			scopes.Framework.Errorf("fake dns server error: %v", err)
		}
	}()
	return s
}

func (s *fakeDNSServer) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := (&dns.Msg{}).SetReply(r)
	switch r.Question[0].Qtype {
	case dns.TypeA, dns.TypeANY:
		domain := msg.Question[0].Name
		c, ok := s.hosts[domain]
		if ok {
			s.hosts[domain]++
			msg.Answer = append(msg.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: domain, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: s.ttl},
				A:   net.ParseIP(fmt.Sprintf("10.0.0.%d", c)),
			})
		}
	}
	if err := w.WriteMsg(msg); err != nil {
		scopes.Framework.Errorf("failed writing fake DNS response: %v", err)
	}
}
