// Copyright 2026 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/tls"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Design 58 P2 fast-endpoint route addressing (route model §3). The urp://
// route URL is the fast analog of unix://; these tables lock its parse,
// canonical form and discrimination against the unix and tcp route URL forms.

func TestParseFastAddr(t *testing.T) {
	// A 16-byte name is one over the 15-byte urp wire cap.
	name15 := strings.Repeat("a", 15)
	name16 := strings.Repeat("a", 16)
	for _, tc := range []struct {
		description string
		raw         string
		expectName  string
		expectErr   bool
	}{
		{"POS valid simple name", "urp://foo", "foo", false},
		{"POS valid name at 15-byte cap", "urp://" + name15, name15, false},
		{"POS scheme is case-insensitive", "URP://foo", "foo", false},
		{"NEG empty name after scheme", "urp://", _EMPTY_, true},
		{"NEG wrong scheme unix", "unix:///run/a.sock", _EMPTY_, true},
		{"NEG wrong scheme nats-route", "nats-route://h:6222", _EMPTY_, true},
		{"NEG no scheme, bare name", "foo", _EMPTY_, true},
		{"NEG leading space", " urp://foo", _EMPTY_, true},
		{"NEG trailing space", "urp://foo ", _EMPTY_, true},
		{"NEG embedded NUL in name", "urp://fo\x00o", _EMPTY_, true},
		{"NEG slash in name", "urp://a/b", _EMPTY_, true},
		{"NEG query in name", "urp://a?b", _EMPTY_, true},
		{"NEG fragment in name", "urp://a#b", _EMPTY_, true},
		{"BND name one over the 15-byte cap", "urp://" + name16, _EMPTY_, true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			got, err := parseFastAddr(tc.raw)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("%s: expected error, got name=%q", tc.description, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.description, err)
			}
			if got != tc.expectName {
				t.Fatalf("%s: name=%q, want %q", tc.description, got, tc.expectName)
			}
		})
	}
}

func TestFastRouteURLRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		description string
		name        string
		expectStr   string
	}{
		{"POS simple name", "foo", "urp://foo"},
		{"POS single char", "a", "urp://a"},
		{"BND 15-byte name", strings.Repeat("z", 15), "urp://" + strings.Repeat("z", 15)},
	} {
		t.Run(tc.description, func(t *testing.T) {
			u := fastRouteURL(tc.name)
			if got := u.String(); got != tc.expectStr {
				t.Fatalf("%s: String()=%q, want %q", tc.description, got, tc.expectStr)
			}
			// String() -> url.Parse -> fastAddrFromRouteURL recovers the name.
			pu, err := url.Parse(u.String())
			if err != nil {
				t.Fatalf("%s: url.Parse(%q): %v", tc.description, u.String(), err)
			}
			name, ok := fastAddrFromRouteURL(pu)
			if !ok || name != tc.name {
				t.Fatalf("%s: fastAddrFromRouteURL=%q,%v want %q,true", tc.description, name, ok, tc.name)
			}
		})
	}
}

func TestIsFastRouteURL(t *testing.T) {
	mustURL := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return u
	}
	for _, tc := range []struct {
		description string
		u           *url.URL
		expectFast  bool
		expectName  string
	}{
		{"POS canonical fast URL", fastRouteURL("foo"), true, "foo"},
		{"POS parsed fast URL string", mustURL("urp://bar"), true, "bar"},
		{"NEG unix route URL", unixRouteURL("/run/a.sock"), false, _EMPTY_},
		{"NEG tcp route URL", mustURL("nats-route://10.0.0.1:6222"), false, _EMPTY_},
		{"NEG nil URL", nil, false, _EMPTY_},
		{"COR urp scheme but empty host", mustURL("urp://"), false, _EMPTY_},
		{"COR urp scheme with over-cap host", mustURL("urp://" + strings.Repeat("a", 16)), false, _EMPTY_},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := isFastRouteURL(tc.u); got != tc.expectFast {
				t.Fatalf("%s: isFastRouteURL=%v, want %v", tc.description, got, tc.expectFast)
			}
			name, ok := fastAddrFromRouteURL(tc.u)
			if ok != tc.expectFast || name != tc.expectName {
				t.Fatalf("%s: fastAddrFromRouteURL=%q,%v want %q,%v",
					tc.description, name, ok, tc.expectName, tc.expectFast)
			}
		})
	}
}

// TestValidateClusterListenTransportFast locks the fast-listener rules
// (route model §4/§5): fast is pairwise exclusive with host/port and unix,
// carries no TLS, and is never advertised. Byte-identical when FastEndpoint
// is unset (the tcp/unix rows).
func TestValidateClusterListenTransportFast(t *testing.T) {
	for _, tc := range []struct {
		description string
		c           ClusterOpts
		expectErr   string // substring; empty = expect success
	}{
		{"POS fast only", ClusterOpts{FastEndpoint: "nats-a"}, ""},
		{"POS tcp only (unaffected)", ClusterOpts{Host: "0.0.0.0", Port: 6222}, ""},
		{"POS unix only (unaffected)", ClusterOpts{UnixSocket: "/run/a.sock"}, ""},
		{"NEG fast + host/port", ClusterOpts{FastEndpoint: "nats-a", Host: "0.0.0.0", Port: 6222}, "host/port"},
		{"NEG fast + unix", ClusterOpts{FastEndpoint: "nats-a", UnixSocket: "/run/a.sock"}, "unix socket"},
		{"NEG fast + tls", ClusterOpts{FastEndpoint: "nats-a", TLSConfig: &tls.Config{}}, "tls is not supported"},
		{"NEG fast + advertise", ClusterOpts{FastEndpoint: "nats-a", Advertise: "urp://nats-a"}, "not gossiped"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			err := validateClusterListenTransport(&tc.c)
			if tc.expectErr == _EMPTY_ {
				if err != nil {
					t.Fatalf("%s: unexpected error: %v", tc.description, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.expectErr) {
				t.Fatalf("%s: err=%v, want substring %q", tc.description, err, tc.expectErr)
			}
		})
	}
}

// TestFastEndpointBackendHook covers openFastEndpoint's pluggable backend: it
// errors when no device backend is linked (the default fork build), returns a
// named fastConn when an opener is installed, and propagates opener errors.
func TestFastEndpointBackendHook(t *testing.T) {
	saved := fastEndpointOpener
	defer setFastEndpointOpener(saved)
	s := &Server{}

	for _, tc := range []struct {
		description string
		opener      func(string) (zeroCopyEndpoint, error)
		expectErr   bool
		expectName  string
	}{
		{"NEG no backend linked", nil, true, _EMPTY_},
		{"POS opener returns endpoint", func(string) (zeroCopyEndpoint, error) {
			return newRecordingEndpoint(4, 4096), nil
		}, false, "nats-a"},
		{"NEG opener error propagated", func(string) (zeroCopyEndpoint, error) {
			return nil, errors.New("attach boom")
		}, true, _EMPTY_},
	} {
		t.Run(tc.description, func(t *testing.T) {
			setFastEndpointOpener(tc.opener)
			fc, err := s.openFastEndpoint("nats-a")
			if tc.expectErr {
				if err == nil {
					t.Fatalf("%s: expected error", tc.description)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.description, err)
			}
			if got := fc.RemoteAddr().String(); got != tc.expectName {
				t.Fatalf("%s: RemoteAddr=%q, want %q", tc.description, got, tc.expectName)
			}
		})
	}
}

// TestCreateFastRouteAndTeardown drives the real route constructor over a fast
// endpoint: createRoute(nil, fc, ...) must build a ROUTER whose transport is
// "fast" with nc==nil, spin the readLoopFast/writeLoop seams, and — when the
// endpoint drains (Recv returns closed) — tear the route down and Close the
// endpoint (which fires fastConn.Closed, the accept supervisor's re-arm signal).
func TestCreateFastRouteAndTeardown(t *testing.T) {
	saved := fastEndpointOpener
	defer setFastEndpointOpener(saved)

	// Unloaded recording endpoint: Recv returns errFastEndpointClosed at once,
	// so the route handshake starts and then the loop closes the connection.
	ep := newRecordingEndpoint(8, 65536)
	setFastEndpointOpener(func(string) (zeroCopyEndpoint, error) { return ep, nil })

	o := DefaultOptions()
	o.Cluster.Name = "fast-route-test"
	s := RunServer(o)
	defer s.Shutdown()

	fc, err := s.openFastEndpoint("nats-a")
	if err != nil {
		t.Fatalf("openFastEndpoint: %v", err)
	}
	c := s.createRoute(nil, fc, nil, Implicit, gossipDefault, _EMPTY_)
	if c == nil {
		t.Fatal("createRoute over fast endpoint returned nil")
	}

	// Transport identity is stable regardless of the teardown race.
	c.mu.Lock()
	kind, transport, hasNC := c.kind, c.route.transport, c.nc != nil
	c.mu.Unlock()
	if kind != ROUTER {
		t.Fatalf("kind=%v, want ROUTER", kind)
	}
	if transport != routeTransportFast {
		t.Fatalf("route transport=%q, want %q", transport, routeTransportFast)
	}
	if hasNC {
		t.Fatal("fast route must have nil nc")
	}

	select {
	case <-fc.Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("fast route did not tear down within 2s")
	}
	if !ep.isClosed() {
		t.Fatal("endpoint was not Closed on route teardown")
	}
}
