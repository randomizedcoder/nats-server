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
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// tempSocketDir returns a short-lived directory for unix sockets. It is
// created under os.TempDir() rather than t.TempDir() because the latter
// embeds the test name and easily exceeds sun_path on macOS.
func tempSocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "nats-uds-")
	if err != nil {
		t.Fatalf("unable to create socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// tempSocketPath returns the URL-form address of a not-yet-created socket
// file in a fresh temporary directory, skipping the test when the platform's
// temporary directory is too deep for sun_path.
func tempSocketPath(t testing.TB, name string) string {
	t.Helper()
	p := filepath.Join(tempSocketDir(t), name)
	if len(nativeUnixAddr(urlUnixAddr(p))) > maxUnixSocketPathLen-1 {
		t.Skipf("temporary socket path %q is longer than sun_path allows", p)
	}
	return urlUnixAddr(p)
}

// skipIfNoUnixSockets skips the test when the platform cannot bind a
// pathname unix socket (older Windows builds, restricted sandboxes).
func skipIfNoUnixSockets(t testing.TB) {
	t.Helper()
	p := tempSocketPath(t, "probe.sock")
	l, err := net.Listen("unix", nativeUnixAddr(p))
	if err != nil {
		t.Skipf("unix sockets not available: %v", err)
	}
	l.Close()
}

// defaultUnixClusterOptions returns DefaultOptions() with the route listener
// on a fresh unix socket instead of the random TCP port DefaultOptions()
// asks for.
func defaultUnixClusterOptions(t testing.TB, name string) *Options {
	t.Helper()
	o := DefaultOptions()
	o.Cluster.Port = 0
	o.Cluster.Host = _EMPTY_
	o.Cluster.UnixSocket = tempSocketPath(t, name)
	return o
}

// leaveStaleSocket creates a socket file at native and closes the listener
// without unlinking, reproducing what a crashed server leaves behind.
func leaveStaleSocket(t testing.TB, native string) {
	t.Helper()
	l, err := net.Listen("unix", native)
	if err != nil {
		t.Fatalf("unable to listen on %q: %v", native, err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(native); err != nil {
		t.Fatalf("expected stale socket file to remain: %v", err)
	}
}

// TestListenRouteUnixStaleSocket covers the stale socket handling of
// listenRouteUnix against real sockets (design doc §6.6).
func TestListenRouteUnixStaleSocket(t *testing.T) {
	skipIfNoUnixSockets(t)

	type row struct {
		description string
		// setup prepares the path and returns anything to clean up.
		setup func(t *testing.T, native string) (cleanup func())
		// abstract runs the row against an abstract address instead of
		// a pathname.
		abstract    bool
		expectedErr string
		// expectedStale is the expected staleRemoved counter afterwards.
		expectedStale uint64
		// expectedWarn, when set, must appear in a Warnf call.
		expectedWarn string
		// expectedFileKept asserts that whatever setup created is still
		// there after the call (the server must not remove it).
		expectedFileKept bool
	}
	nop := func(*testing.T, string) func() { return func() {} }
	rows := []row{
		// positive
		{
			description: "nothing at the path, listener is created",
			setup:       nop,
		},
		{
			description: "stale socket file is removed and replaced",
			setup: func(t *testing.T, native string) func() {
				leaveStaleSocket(t, native)
				return func() {}
			},
			expectedStale: 1,
			expectedWarn:  "Removed stale unix socket",
		},
		// negative
		{
			description: "regular file at the path is an error and is kept",
			setup: func(t *testing.T, native string) func() {
				if err := os.WriteFile(native, []byte("not a socket"), 0600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
			expectedErr:      "is not a socket",
			expectedFileKept: true,
		},
		{
			description: "directory at the path is an error and is kept",
			setup: func(t *testing.T, native string) func() {
				if err := os.Mkdir(native, 0700); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
			expectedErr:      "is not a socket",
			expectedFileKept: true,
		},
		{
			description: "socket served by another listener is an error and is kept",
			setup: func(t *testing.T, native string) func() {
				l, err := net.Listen("unix", native)
				if err != nil {
					t.Fatal(err)
				}
				return func() { l.Close() }
			},
			expectedErr:      "is in use by another process",
			expectedFileKept: true,
		},
		{
			description: "missing parent directory is an error",
			setup: func(t *testing.T, native string) func() {
				return func() {}
			},
			expectedErr: "no such file or directory",
		},
		// corner
		{
			description: "symlink at the path is not followed and is kept",
			setup: func(t *testing.T, native string) func() {
				if err := os.Symlink(native+".target", native); err != nil {
					t.Skipf("symlinks not available: %v", err)
				}
				return func() {}
			},
			expectedErr:      "is not a socket",
			expectedFileKept: true,
		},
	}
	if runtime.GOOS == "linux" {
		rows = append(rows, row{
			description: "abstract socket needs no file and no stale handling",
			setup:       nop,
			abstract:    true,
		})
	}

	for _, tc := range rows {
		t.Run(tc.description, func(t *testing.T) {
			s := &Server{}
			warnLogger := &captureWarnLogger{warn: make(chan string, 4)}
			s.SetLogger(warnLogger, false, false)

			var addr, native string
			switch {
			case tc.abstract:
				addr = fmt.Sprintf("@nats-test-%d", time.Now().UnixNano())
				native = addr
			case strings.Contains(tc.description, "missing parent"):
				addr = tempSocketPath(t, "missing/dir/a.sock")
				native = nativeUnixAddr(addr)
			default:
				addr = tempSocketPath(t, "a.sock")
				native = nativeUnixAddr(addr)
			}
			cleanup := tc.setup(t, native)
			defer cleanup()

			l, err := s.listenRouteUnix(addr)
			if l != nil {
				defer l.Close()
			}
			if tc.expectedErr != _EMPTY_ {
				if err == nil {
					t.Fatalf("expected error containing %q, got listener %v", tc.expectedErr, l.Addr())
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %q", tc.expectedErr, err.Error())
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := s.udsStats.staleRemoved.Load(); got != tc.expectedStale {
				t.Fatalf("staleRemoved = %d, expected %d", got, tc.expectedStale)
			}
			if tc.expectedWarn != _EMPTY_ {
				select {
				case w := <-warnLogger.warn:
					if !strings.Contains(w, tc.expectedWarn) {
						t.Fatalf("expected warning containing %q, got %q", tc.expectedWarn, w)
					}
				default:
					t.Fatalf("expected a warning containing %q", tc.expectedWarn)
				}
			} else {
				select {
				case w := <-warnLogger.warn:
					t.Fatalf("unexpected warning %q", w)
				default:
				}
			}
			if tc.expectedFileKept {
				if _, err := os.Lstat(native); err != nil {
					t.Fatalf("expected the existing entry to be kept: %v", err)
				}
			}
			if err == nil {
				// The listener works: a dial connects, and a second
				// listen on the same address is refused rather than
				// treated as stale.
				c, err := net.DialTimeout("unix", native, time.Second)
				if err != nil {
					t.Fatalf("unable to dial the new listener: %v", err)
				}
				c.Close()
				if l2, err := s.listenRouteUnix(addr); err == nil {
					l2.Close()
					t.Fatalf("expected a second listen on %q to fail", addr)
				} else if !tc.abstract && !strings.Contains(err.Error(), "is in use by another process") {
					t.Fatalf("expected in-use error on second listen, got %v", err)
				}
				if got := s.udsStats.staleRemoved.Load(); got != tc.expectedStale {
					t.Fatalf("second listen must not remove the live socket, staleRemoved = %d", got)
				}
				// Closing the listener removes a pathname socket file.
				l.Close()
				if !tc.abstract {
					if _, err := os.Lstat(native); !os.IsNotExist(err) {
						t.Fatalf("expected socket file to be unlinked on Close, Lstat err=%v", err)
					}
				}
			}
		})
	}
}

// TestListenRouteUnixPathLength checks listenRouteUnix at the sun_path
// boundary with real bind calls.
func TestListenRouteUnixPathLength(t *testing.T) {
	skipIfNoUnixSockets(t)
	dir := tempSocketDir(t)
	// Room left for the file name so that the native path is exactly the
	// pathname limit (sun_path - 1).
	room := (maxUnixSocketPathLen - 1) - len(dir) - 1
	if room < 1 {
		t.Skipf("temporary directory %q leaves no room for a socket name", dir)
	}
	for _, tc := range []struct {
		description string
		name        string
		expectedErr bool
	}{
		// boundary
		{description: "native path at the pathname limit binds", name: strings.Repeat("a", room)},
		{description: "native path one over the limit fails at bind", name: strings.Repeat("a", room+1), expectedErr: true},
		{description: "shortest name binds", name: "a"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			s := &Server{}
			s.SetLogger(&DummyLogger{}, false, false)
			l, err := s.listenRouteUnix(urlUnixAddr(filepath.Join(dir, tc.name)))
			if l != nil {
				defer l.Close()
			}
			if tc.expectedErr && err == nil {
				t.Fatalf("expected an error for a %d byte path", len(dir)+1+len(tc.name))
			}
			if !tc.expectedErr && err != nil {
				t.Fatalf("unexpected error for a %d byte path: %v", len(dir)+1+len(tc.name), err)
			}
		})
	}
}

// TestServerUnixListenerAccessors covers ClusterAddr, ClusterUnixAddr,
// ClusterListenAddr, PortsInfo, the self-route map and socket cleanup on
// Shutdown for a server whose route listener is a unix socket (design doc
// §6.7).
func TestServerUnixListenerAccessors(t *testing.T) {
	skipIfNoUnixSockets(t)

	for _, tc := range []struct {
		description string
		unix        bool
	}{
		{description: "unix socket listener", unix: true},
		{description: "tcp listener", unix: false},
	} {
		t.Run(tc.description, func(t *testing.T) {
			var o *Options
			if tc.unix {
				o = defaultUnixClusterOptions(t, "a.sock")
			} else {
				o = DefaultOptions()
			}
			s := RunServer(o)
			defer s.Shutdown()

			native := nativeUnixAddr(o.Cluster.UnixSocket)
			ports := s.PortsInfo(time.Second)
			if ports == nil {
				t.Fatal("expected ports info")
			}
			if tc.unix {
				if a := s.ClusterAddr(); a != nil {
					t.Fatalf("ClusterAddr() = %v, expected nil for a unix listener", a)
				}
				ua := s.ClusterUnixAddr()
				if ua == nil || ua.Name != native {
					t.Fatalf("ClusterUnixAddr() = %v, expected name %q", ua, native)
				}
				if la := s.ClusterListenAddr(); la == nil || la.Network() != "unix" || la.String() != native {
					t.Fatalf("ClusterListenAddr() = %v, expected unix %q", la, native)
				}
				expectedURL := unixSchemePrefix + o.Cluster.UnixSocket
				if len(ports.Cluster) != 1 || ports.Cluster[0] != expectedURL {
					t.Fatalf("PortsInfo().Cluster = %v, expected [%s]", ports.Cluster, expectedURL)
				}
				if fi, err := os.Lstat(native); err != nil || fi.Mode()&os.ModeSocket == 0 {
					t.Fatalf("expected a socket file at %q, err=%v", native, err)
				}
				s.mu.RLock()
				_, self := s.unixRoutesToSelf[o.Cluster.UnixSocket]
				nTCPSelf := len(s.routesToSelf)
				s.mu.RUnlock()
				if !self {
					t.Fatalf("expected %q in unixRoutesToSelf", o.Cluster.UnixSocket)
				}
				if nTCPSelf != 0 {
					t.Fatalf("expected no ip:port self routes for a unix listener, got %d", nTCPSelf)
				}
				if o.Cluster.Port != 0 {
					t.Fatalf("expected Cluster.Port to stay 0, got %d", o.Cluster.Port)
				}
				// A raw connection is accepted and counted.
				c, err := net.DialTimeout("unix", native, time.Second)
				if err != nil {
					t.Fatalf("unable to dial route socket: %v", err)
				}
				checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
					if n := s.udsStats.accepted.Load(); n != 1 {
						return fmt.Errorf("accepted = %d, expected 1", n)
					}
					return nil
				})
				c.Close()
			} else {
				if a := s.ClusterAddr(); a == nil || a.Port == 0 {
					t.Fatalf("ClusterAddr() = %v, expected a tcp address", a)
				}
				if ua := s.ClusterUnixAddr(); ua != nil {
					t.Fatalf("ClusterUnixAddr() = %v, expected nil for a tcp listener", ua)
				}
				if la := s.ClusterListenAddr(); la == nil || la.Network() != "tcp" {
					t.Fatalf("ClusterListenAddr() = %v, expected tcp", la)
				}
				// DefaultOptions() binds every interface, so there is one
				// URL per address; none of them is a unix URL.
				if len(ports.Cluster) == 0 {
					t.Fatal("PortsInfo().Cluster is empty, expected nats://host:port entries")
				}
				for _, u := range ports.Cluster {
					if !strings.HasPrefix(u, "nats://") {
						t.Fatalf("PortsInfo().Cluster entry %q, expected a nats:// URL", u)
					}
				}
				if n := s.udsStats.accepted.Load(); n != 0 {
					t.Fatalf("accepted = %d, expected 0 for a tcp listener", n)
				}
			}

			s.Shutdown()
			if a := s.ClusterListenAddr(); a != nil {
				t.Fatalf("ClusterListenAddr() = %v after Shutdown, expected nil", a)
			}
			if tc.unix {
				if _, err := os.Lstat(native); !os.IsNotExist(err) {
					t.Fatalf("expected socket file to be removed on Shutdown, Lstat err=%v", err)
				}
			}
		})
	}
}

// TestServerUnixListenerStartupErrors checks that a route listener that
// cannot be created is reported through the same fields as a TCP failure.
func TestServerUnixListenerStartupErrors(t *testing.T) {
	skipIfNoUnixSockets(t)
	for _, tc := range []struct {
		description string
		setup       func(t *testing.T, native string) func()
		expectedErr string
	}{
		{
			description: "regular file in the way",
			setup: func(t *testing.T, native string) func() {
				if err := os.WriteFile(native, nil, 0600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
			expectedErr: "is not a socket",
		},
		{
			description: "another server on the same socket",
			setup: func(t *testing.T, native string) func() {
				l, err := net.Listen("unix", native)
				if err != nil {
					t.Fatal(err)
				}
				return func() { l.Close() }
			},
			expectedErr: "is in use by another process",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			o := defaultUnixClusterOptions(t, "a.sock")
			defer tc.setup(t, nativeUnixAddr(o.Cluster.UnixSocket))()
			s, err := NewServer(o)
			if err != nil {
				t.Fatalf("unexpected NewServer error: %v", err)
			}
			s.SetLogger(&DummyLogger{}, false, false)
			// Start returns after the accept loops report; a Fatalf
			// stops the server.
			s.Start()
			defer s.Shutdown()
			checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
				s.mu.RLock()
				lerr := s.routeListenerErr
				s.mu.RUnlock()
				if lerr == nil {
					return fmt.Errorf("route listener error not set yet")
				}
				if !strings.Contains(lerr.Error(), tc.expectedErr) {
					return fmt.Errorf("expected error containing %q, got %q", tc.expectedErr, lerr.Error())
				}
				return nil
			})
			if s.ClusterListenAddr() != nil {
				t.Fatal("expected no route listener after a listen failure")
			}
		})
	}
}

// fakeListener is a net.Listener with a fixed address, for the URL
// formatting tests.
type fakeListener struct {
	net.Listener
	addr net.Addr
}

func (l fakeListener) Addr() net.Addr { return l.addr }

// TestFormatURLUnix covers formatURL and resolveHostPorts for unix and tcp
// listeners without binding anything.
func TestFormatURLUnix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		protocol    string
		addr        net.Addr
		expected    []string
	}{
		// positive
		{description: "pathname socket", protocol: "nats", addr: &net.UnixAddr{Net: "unix", Name: nativeUnixAddr("/run/nats/a.sock")}, expected: []string{"unix:///run/nats/a.sock"}},
		{description: "abstract socket", protocol: "nats", addr: &net.UnixAddr{Net: "unix", Name: "@nats-a"}, expected: []string{"unix://@nats-a"}},
		{description: "tls protocol is ignored for unix", protocol: "tls", addr: &net.UnixAddr{Net: "unix", Name: nativeUnixAddr("/run/nats/a.sock")}, expected: []string{"unix:///run/nats/a.sock"}},
		{description: "tcp loopback unchanged", protocol: "nats", addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6222}, expected: []string{"nats://127.0.0.1:6222"}},
		{description: "tcp tls unchanged", protocol: "tls", addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6222}, expected: []string{"tls://127.0.0.1:6222"}},
		// corner
		{description: "windows drive path is given in URL form", protocol: "nats", addr: &net.UnixAddr{Net: "unix", Name: nativeUnixAddr("/C:/nats/a.sock")}, expected: []string{"unix:///C:/nats/a.sock"}},
		{description: "unknown address type yields no urls", protocol: "nats", addr: &net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, expected: []string{}},
	} {
		t.Run(tc.description, func(t *testing.T) {
			got := formatURL(tc.protocol, fakeListener{addr: tc.addr})
			if fmt.Sprint(got) != fmt.Sprint(tc.expected) {
				t.Fatalf("formatURL(%q, %v) = %v, expected %v", tc.protocol, tc.addr, got, tc.expected)
			}
		})
	}
}

// TestRouteInfoUnix covers the INFO fields a server advertises to its peers
// for each listener and advertise combination (design doc §6.8).
func TestRouteInfoUnix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		cluster     ClusterOpts
		expected    Info
		expectedErr string
	}{
		// positive
		{description: "tcp no advertise", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222}, expected: Info{Host: "127.0.0.1", Port: 6222}},
		{description: "tcp with advertise", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1:6333"}, expected: Info{Host: "10.0.0.1", Port: 6333, IP: "nats-route://10.0.0.1:6333/"}},
		{description: "tcp with host-only advertise keeps the listen port", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1"}, expected: Info{Host: "10.0.0.1", Port: 6222, IP: "nats-route://10.0.0.1:6222/"}},
		{description: "unix no advertise advertises nothing", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock"}, expected: Info{}},
		{description: "unix with advertise", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock", Advertise: "unix:///p/a.sock"}, expected: Info{IP: "unix:///p/a.sock"}},
		{description: "unix with abstract advertise", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock", Advertise: "unix://@a"}, expected: Info{IP: "unix://@a"}},
		// negative
		{description: "unix with invalid advertise", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock", Advertise: "unix://relative"}, expectedErr: "must be an absolute path"},
		{description: "tcp with invalid advertise", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1:XXXX"}, expectedErr: "invalid syntax"},
		// corner
		{description: "advertise scheme is case-insensitive", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock", Advertise: "UNIX:///p/a.sock"}, expected: Info{IP: "unix:///p/a.sock"}},
		{description: "stale values are overwritten", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock"}, expected: Info{}},
	} {
		t.Run(tc.description, func(t *testing.T) {
			s := &Server{opts: &Options{Cluster: tc.cluster}}
			// Pretend a previous call left TCP values behind.
			s.routeInfo = Info{Host: "stale", Port: 1, IP: "nats-route://stale:1/"}
			err := s.setRouteInfoHostPortAndIP()
			if tc.expectedErr != _EMPTY_ {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := s.routeInfo
			if got.Host != tc.expected.Host || got.Port != tc.expected.Port || got.IP != tc.expected.IP {
				t.Fatalf("got Host=%q Port=%d IP=%q, expected Host=%q Port=%d IP=%q",
					got.Host, got.Port, got.IP, tc.expected.Host, tc.expected.Port, tc.expected.IP)
			}
		})
	}
}

// TestHasThisRouteConfiguredUnix covers matching a gossiped unix address
// against the configured routes (design doc §6.8).
func TestHasThisRouteConfiguredUnix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		routes      []string
		info        Info
		expected    bool
	}{
		// positive
		{description: "exact path match", routes: []string{"unix:///run/b.sock"}, info: Info{IP: "unix:///run/b.sock"}, expected: true},
		{description: "abstract match", routes: []string{"unix://@b"}, info: Info{IP: "unix://@b"}, expected: true},
		{description: "match among mixed routes", routes: []string{"nats-route://127.0.0.1:6222", "unix:///run/b.sock"}, info: Info{IP: "unix:///run/b.sock"}, expected: true},
		{description: "tcp info still matches tcp route", routes: []string{"nats-route://127.0.0.1:6222"}, info: Info{Host: "127.0.0.1", Port: 6222}, expected: true},
		{description: "gossiped scheme case differs", routes: []string{"unix:///run/b.sock"}, info: Info{IP: "UNIX:///run/b.sock"}, expected: true},
		{description: "gossiped canonical URL is percent-encoded", routes: []string{"unix:///run/ñ.sock"}, info: Info{IP: unixRouteURL("/run/ñ.sock").String()}, expected: true},
		// negative
		{description: "path differs by trailing component", routes: []string{"unix:///run/b.sock"}, info: Info{IP: "unix:///run/b.sock2"}, expected: false},
		{description: "tcp configured, unix info", routes: []string{"nats-route://127.0.0.1:6222"}, info: Info{IP: "unix:///run/b.sock"}, expected: false},
		{description: "unix configured, tcp info", routes: []string{"unix:///run/b.sock"}, info: Info{Host: "127.0.0.1", Port: 6222, IP: "nats-route://127.0.0.1:6222/"}, expected: false},
		{description: "empty routes", routes: nil, info: Info{IP: "unix:///run/b.sock"}, expected: false},
		{description: "invalid gossiped unix address", routes: []string{"unix:///run/b.sock"}, info: Info{IP: "unix://run/b.sock"}, expected: false},
		// corner
		{description: "path comparison is case-sensitive", routes: []string{"unix:///run/B.sock"}, info: Info{IP: "unix:///run/b.sock"}, expected: false},
		{description: "pathname and abstract with the same name differ", routes: []string{"unix:///b"}, info: Info{IP: "unix://@b"}, expected: false},
		{description: "route with credentials still matches by address", routes: []string{"nats-route://u:p@127.0.0.1:6222", "unix:///run/b.sock"}, info: Info{IP: "unix:///run/b.sock"}, expected: true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			var routes []*url.URL
			for _, r := range tc.routes {
				u, err := parseURL(r, "route")
				if err != nil {
					t.Fatal(err)
				}
				routes = append(routes, u)
			}
			s := &Server{opts: &Options{Routes: routes}}
			if got := s.hasThisRouteConfigured(&tc.info); got != tc.expected {
				t.Fatalf("hasThisRouteConfigured(%+v) = %v, expected %v", tc.info, got, tc.expected)
			}
		})
	}
}

// acceptOnce listens on the unix socket at native, accepts one connection,
// closes it and stops listening. It reports through the returned channel.
func acceptOnce(t *testing.T, native string) chan struct{} {
	t.Helper()
	l, err := net.Listen("unix", native)
	if err != nil {
		t.Fatalf("unable to listen on %q: %v", native, err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		defer l.Close()
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Close()
		accepted <- struct{}{}
	}()
	t.Cleanup(func() { l.Close() })
	return accepted
}

// TestProcessImplicitRouteUnix checks when a gossiped INFO leads to a dial
// (design doc §6.8).
func TestProcessImplicitRouteUnix(t *testing.T) {
	skipIfNoUnixSockets(t)

	for _, tc := range []struct {
		description string
		// info builds the gossiped INFO given the server and the peer path.
		info func(s *Server, peer string) *Info
		// configured, when true, puts the peer path in the server's routes.
		configured   bool
		expectedDial bool
	}{
		// positive
		{
			description:  "unix IP not configured is dialed",
			info:         func(s *Server, peer string) *Info { return &Info{ID: "peer", IP: unixSchemePrefix + peer} },
			expectedDial: true,
		},
		{
			description:  "gossiped canonical URL is dialed",
			info:         func(s *Server, peer string) *Info { return &Info{ID: "peer", IP: unixRouteURL(peer).String()} },
			expectedDial: true,
		},
		// negative
		{
			description:  "no address at all is not dialed",
			info:         func(s *Server, peer string) *Info { return &Info{ID: "peer"} },
			expectedDial: false,
		},
		{
			description:  "unix IP already configured is left to the explicit route",
			info:         func(s *Server, peer string) *Info { return &Info{ID: "peer", IP: unixSchemePrefix + peer} },
			configured:   true,
			expectedDial: false,
		},
		{
			description:  "own ID is not dialed",
			info:         func(s *Server, peer string) *Info { return &Info{ID: s.info.ID, IP: unixSchemePrefix + peer} },
			expectedDial: false,
		},
		{
			description: "own listen path is not dialed",
			info: func(s *Server, peer string) *Info {
				return &Info{ID: "peer", IP: unixSchemePrefix + s.getOpts().Cluster.UnixSocket}
			},
			expectedDial: false,
		},
		{
			description:  "invalid unix address is not dialed",
			info:         func(s *Server, peer string) *Info { return &Info{ID: "peer", IP: "unix://relative"} },
			expectedDial: false,
		},
		// corner
		{
			description: "auth required still dials with the canonical unix URL",
			info: func(s *Server, peer string) *Info {
				return &Info{ID: "peer", IP: unixSchemePrefix + peer, AuthRequired: true}
			},
			expectedDial: true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			o := defaultUnixClusterOptions(t, "a.sock")
			o.Cluster.Username, o.Cluster.Password = "ruser", "top_secret"
			peer := tempSocketPath(t, "peer.sock")
			var accepted chan struct{}
			var sp *Server
			if tc.configured {
				// A real peer, so the explicit route settles and only an
				// implicit dial would move the counter.
				op := defaultUnixClusterOptions(t, "unused.sock")
				op.Cluster.UnixSocket = peer
				op.Cluster.Username, op.Cluster.Password = "ruser", "top_secret"
				sp = RunServer(op)
				defer sp.Shutdown()
				o.Routes = []*url.URL{unixRouteURL(peer)}
			} else {
				accepted = acceptOnce(t, nativeUnixAddr(peer))
			}
			s := RunServer(o)
			defer s.Shutdown()
			if tc.configured {
				// Wait for every pooled connection, then for the counter
				// to stop moving.
				checkClusterFormed(t, s, sp)
				var settled uint64
				checkFor(t, 2*time.Second, 50*time.Millisecond, func() error {
					if n := s.udsStats.dialed.Load(); n != settled {
						settled = n
						return fmt.Errorf("still dialing: %d", n)
					}
					return nil
				})
			}
			before := s.udsStats.dialed.Load()

			s.processImplicitRoute(tc.info(s, peer), false)

			if tc.configured {
				time.Sleep(250 * time.Millisecond)
				if got := s.udsStats.dialed.Load(); got != before {
					t.Fatalf("dialed counter moved from %d to %d, expected the configured route to be left alone", before, got)
				}
				return
			}
			select {
			case <-accepted:
				if !tc.expectedDial {
					t.Fatal("peer was dialed, expected no dial")
				}
			case <-time.After(250 * time.Millisecond):
				if tc.expectedDial {
					t.Fatalf("peer was not dialed (dialed counter %d -> %d)", before, s.udsStats.dialed.Load())
				}
			}
			if !tc.expectedDial {
				if got := s.udsStats.dialed.Load(); got != before {
					t.Fatalf("dialed counter moved from %d to %d, expected no dial", before, got)
				}
			}
		})
	}
}

// routeHosts returns the c.host of every route on s, sorted.
func routeHosts(s *Server) []string {
	var hosts []string
	s.mu.RLock()
	s.forEachRoute(func(r *client) {
		r.mu.Lock()
		hosts = append(hosts, r.host)
		r.mu.Unlock()
	})
	s.mu.RUnlock()
	sort.Strings(hosts)
	return hosts
}

// TestRouteUnixDial covers connectToRoute over unix sockets with real
// listeners (design doc §6.9).
func TestRouteUnixDial(t *testing.T) {
	skipIfNoUnixSockets(t)

	t.Run("explicit route to a listening path forms a cluster", func(t *testing.T) {
		ob := defaultUnixClusterOptions(t, "b.sock")
		sb := RunServer(ob)
		defer sb.Shutdown()

		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(ob.Cluster.UnixSocket)}
		sa := RunServer(oa)
		defer sa.Shutdown()

		checkClusterFormed(t, sa, sb)
		if n := sa.udsStats.dialed.Load(); n == 0 {
			t.Fatal("expected the dialed counter to move")
		}
		if n := sa.udsStats.dialErrors.Load(); n != 0 {
			t.Fatalf("dialErrors = %d, expected 0", n)
		}
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sb.udsStats.accepted.Load(); n == 0 {
				return fmt.Errorf("expected the accepted counter to move")
			}
			return nil
		})
		// The soliciting side knows the peer by its path; the accepting
		// side sees an unnamed peer.
		if hosts := routeHosts(sa); len(hosts) == 0 || hosts[0] != nativeUnixAddr(ob.Cluster.UnixSocket) {
			t.Fatalf("route host on the dialing side = %v, expected %q", hosts, nativeUnixAddr(ob.Cluster.UnixSocket))
		}
		for _, h := range routeHosts(sb) {
			if strings.Contains(h, "b.sock") {
				t.Fatalf("accepting side route host = %q, expected an unnamed peer", h)
			}
		}
	})

	t.Run("explicit route with cluster authorization", func(t *testing.T) {
		for _, tc := range []struct {
			description    string
			user, pass     string
			expectedFormed bool
		}{
			// positive
			{description: "matching cluster credentials", user: "ruser", pass: "top_secret", expectedFormed: true},
			// negative
			{description: "wrong password on the dialing side", user: "ruser", pass: "wrong", expectedFormed: false},
			{description: "no credentials on the dialing side", expectedFormed: false},
		} {
			t.Run(tc.description, func(t *testing.T) {
				ob := defaultUnixClusterOptions(t, "b.sock")
				ob.Cluster.Username, ob.Cluster.Password = "ruser", "top_secret"
				sb := RunServer(ob)
				defer sb.Shutdown()

				oa := defaultUnixClusterOptions(t, "a.sock")
				oa.Cluster.Username, oa.Cluster.Password = tc.user, tc.pass
				oa.Routes = []*url.URL{unixRouteURL(ob.Cluster.UnixSocket)}
				sa := RunServer(oa)
				defer sa.Shutdown()

				if tc.expectedFormed {
					checkClusterFormed(t, sa, sb)
					return
				}
				time.Sleep(200 * time.Millisecond)
				if n := sa.NumRoutes(); n != 0 {
					t.Fatalf("expected no routes with bad credentials, got %d", n)
				}
			})
		}
	})

	if runtime.GOOS == "linux" {
		t.Run("explicit route to an abstract socket", func(t *testing.T) {
			ob := defaultUnixClusterOptions(t, "b.sock")
			ob.Cluster.UnixSocket = fmt.Sprintf("@nats-uds-%d", time.Now().UnixNano())
			sb := RunServer(ob)
			defer sb.Shutdown()

			oa := defaultUnixClusterOptions(t, "a.sock")
			oa.Routes = []*url.URL{unixRouteURL(ob.Cluster.UnixSocket)}
			sa := RunServer(oa)
			defer sa.Shutdown()

			checkClusterFormed(t, sa, sb)
		})
	}

	t.Run("explicit route to a missing path keeps retrying until it appears", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(peer)}
		sa := RunServer(oa)
		defer sa.Shutdown()

		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sa.udsStats.dialErrors.Load(); n < 2 {
				return fmt.Errorf("dialErrors = %d, expected retries", n)
			}
			return nil
		})
		if sa.NumRoutes() != 0 {
			t.Fatal("expected no route yet")
		}
		ob := defaultUnixClusterOptions(t, "unused.sock")
		ob.Cluster.UnixSocket = peer
		sb := RunServer(ob)
		defer sb.Shutdown()
		checkClusterFormed(t, sa, sb)
	})

	t.Run("dial error message names the socket", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(peer)}
		sa := RunServer(oa)
		defer sa.Shutdown()
		l := &captureErrorLogger{errCh: make(chan string, 16)}
		sa.SetLogger(l, false, false)
		// Errors are reported on the first attempt, then every so often.
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sa.udsStats.dialErrors.Load(); n < 1 {
				return fmt.Errorf("dialErrors = %d", n)
			}
			return nil
		})
		select {
		case e := <-l.errCh:
			if !strings.Contains(e, nativeUnixAddr(peer)) {
				t.Fatalf("expected the error to name %q, got %q", nativeUnixAddr(peer), e)
			}
		case <-time.After(2 * time.Second):
			// The first attempt may have been logged before the logger was
			// swapped in; the counter check above is the hard assertion.
		}
	})

	t.Run("implicit route gives up after connect_retries", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		for _, tc := range []struct {
			description      string
			retries          int
			expectedAttempts uint64
		}{
			// boundary
			{description: "connect_retries 0 is exactly one attempt", retries: 0, expectedAttempts: 1},
			{description: "connect_retries 2 is three attempts", retries: 2, expectedAttempts: 3},
		} {
			t.Run(tc.description, func(t *testing.T) {
				oa := defaultUnixClusterOptions(t, "a.sock")
				oa.Cluster.ConnectRetries = tc.retries
				sa := RunServer(oa)
				defer sa.Shutdown()
				sa.processImplicitRoute(&Info{ID: "peer", IP: unixSchemePrefix + peer}, false)
				checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
					if n := sa.udsStats.dialErrors.Load(); n < tc.expectedAttempts {
						return fmt.Errorf("dialErrors = %d, expected %d", n, tc.expectedAttempts)
					}
					return nil
				})
				time.Sleep(100 * time.Millisecond)
				if n := sa.udsStats.dialErrors.Load(); n != tc.expectedAttempts {
					t.Fatalf("dialErrors = %d, expected exactly %d", n, tc.expectedAttempts)
				}
			})
		}
	})

	t.Run("explicit route with connect_backoff keeps retrying", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Cluster.ConnectBackoff = true
		oa.Routes = []*url.URL{unixRouteURL(peer)}
		sa := RunServer(oa)
		defer sa.Shutdown()
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sa.udsStats.dialErrors.Load(); n < 3 {
				return fmt.Errorf("dialErrors = %d, expected continued retries", n)
			}
			return nil
		})
	})

	t.Run("route removed by reload stops the retry loop", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(peer)}
		sa := RunServer(oa)
		defer sa.Shutdown()
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sa.udsStats.dialErrors.Load(); n < 1 {
				return fmt.Errorf("no dial attempt yet")
			}
			return nil
		})
		noRoutes := *oa
		noRoutes.Routes = nil
		if err := sa.ReloadOptions(&noRoutes); err != nil {
			t.Fatalf("reload failed: %v", err)
		}
		if sa.routeStillValid(unixRouteURL(peer)) {
			t.Fatal("expected the route to no longer be valid")
		}
		// Retries stop: the counter settles.
		var settled uint64
		checkFor(t, 2*time.Second, 50*time.Millisecond, func() error {
			n := sa.udsStats.dialErrors.Load()
			if n != settled {
				settled = n
				return fmt.Errorf("still retrying: %d", n)
			}
			return nil
		})
	})

	t.Run("explicit route to own listen path is skipped", func(t *testing.T) {
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(oa.Cluster.UnixSocket)}
		sa := RunServer(oa)
		defer sa.Shutdown()
		time.Sleep(100 * time.Millisecond)
		if n := sa.udsStats.dialed.Load(); n != 0 {
			t.Fatalf("dialed = %d, expected the self route to be skipped without dialing", n)
		}
		if sa.NumRoutes() != 0 {
			t.Fatal("expected no routes")
		}
	})

	t.Run("peer that closes right after accept triggers a reconnect", func(t *testing.T) {
		peer := tempSocketPath(t, "b.sock")
		accepted := acceptOnce(t, nativeUnixAddr(peer))
		oa := defaultUnixClusterOptions(t, "a.sock")
		oa.Routes = []*url.URL{unixRouteURL(peer)}
		sa := RunServer(oa)
		defer sa.Shutdown()
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("peer was not dialed")
		}
		// The listener is gone now, so the reconnects fail and are counted
		// as dial errors. (A reconnect racing the listener close may still
		// connect into the backlog, so the successful count is not exact.)
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			if n := sa.udsStats.dialErrors.Load(); n < 1 {
				return fmt.Errorf("dialErrors = %d, expected a failed reconnect attempt", n)
			}
			return nil
		})
		if n := sa.udsStats.dialed.Load(); n == 0 {
			t.Fatal("dialed = 0, expected the accepted dial to be counted")
		}
	})
}

// unixClusterConf renders a configuration file for a server with a unix
// socket route listener.
func unixClusterConf(name, socket, advertise string, routes ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server_name: %s\nlisten: 127.0.0.1:-1\ncluster {\n  name: uds\n  pool_size: -1\n  listen: %q\n", name, unixSchemePrefix+socket)
	if advertise != _EMPTY_ {
		fmt.Fprintf(&b, "  advertise: %q\n", advertise)
	}
	if len(routes) > 0 {
		b.WriteString("  routes: [\n")
		for _, r := range routes {
			fmt.Fprintf(&b, "    %q,\n", r)
		}
		b.WriteString("  ]\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// TestRouteUnixReload covers adding and removing unix routes, and the
// rejected listener changes, through a configuration reload (design doc
// §6.11).
func TestRouteUnixReload(t *testing.T) {
	skipIfNoUnixSockets(t)

	ob := defaultUnixClusterOptions(t, "b.sock")
	ob.Cluster.Name = "uds"
	ob.Cluster.PoolSize = -1
	sb := RunServer(ob)
	defer sb.Shutdown()
	bRoute := unixSchemePrefix + ob.Cluster.UnixSocket

	aSock := tempSocketPath(t, "a.sock")
	sa, _, conf := runReloadServerWithContent(t, []byte(unixClusterConf("a", aSock, _EMPTY_)))
	defer sa.Shutdown()
	checkNumRoutes(t, sa, 0)

	for _, tc := range []struct {
		description string
		conf        string
		expectedErr string
		// expectedRoutes is the route count on A after the reload.
		expectedRoutes int
		// expectedIP is A's advertised INFO IP after the reload.
		expectedIP string
		// expectedSelf and unexpectedSelf assert the unix self-route map after reload.
		expectedSelf   []string
		unexpectedSelf []string
	}{
		// positive
		{description: "adding a unix route dials it", conf: unixClusterConf("a", aSock, _EMPTY_, bRoute), expectedRoutes: 1},
		{description: "same route again is a no-op", conf: unixClusterConf("a", aSock, _EMPTY_, bRoute), expectedRoutes: 1},
		{description: "adding a unix advertise is accepted and applied", conf: unixClusterConf("a", aSock, "unix:///p/a.sock", bRoute), expectedRoutes: 1, expectedIP: "unix:///p/a.sock", expectedSelf: []string{aSock, "/p/a.sock"}},
		{description: "changing between unix advertise paths is accepted", conf: unixClusterConf("a", aSock, "unix:///p/a2.sock", bRoute), expectedRoutes: 1, expectedIP: "unix:///p/a2.sock", expectedSelf: []string{aSock, "/p/a2.sock"}, unexpectedSelf: []string{"/p/a.sock"}},
		{description: "removing the unix route closes it", conf: unixClusterConf("a", aSock, "unix:///p/a2.sock"), expectedRoutes: 0, expectedIP: "unix:///p/a2.sock"},
		{description: "removing the advertise is accepted", conf: unixClusterConf("a", aSock, _EMPTY_), expectedRoutes: 0, expectedSelf: []string{aSock}, unexpectedSelf: []string{"/p/a2.sock"}},
		// negative
		{description: "changing the unix path is rejected", conf: unixClusterConf("a", tempSocketPath(t, "a2.sock"), _EMPTY_), expectedErr: "config reload not supported for cluster unix socket"},
		{description: "changing listen from unix to tcp is rejected", conf: "server_name: a\nlisten: 127.0.0.1:-1\ncluster { name: uds, pool_size: -1, listen: 127.0.0.1:-1 }\n", expectedErr: "config reload not supported for cluster unix socket"},
		{description: "tcp advertise on the unix listener is rejected", conf: unixClusterConf("a", aSock, "10.0.0.1:6222"), expectedErr: `advertise transport "tcp" does not match listener transport "unix"`},
		{description: "invalid unix advertise is rejected", conf: unixClusterConf("a", aSock, "unix://relative"), expectedErr: "must be an absolute path"},
		{description: "invalid unix route is rejected", conf: unixClusterConf("a", aSock, _EMPTY_, "unix://relative"), expectedErr: "must be an absolute path"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			changeCurrentConfigContentWithNewContent(t, conf, []byte(tc.conf))
			err := sa.Reload()
			if tc.expectedErr != _EMPTY_ {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected reload error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected reload error: %v", err)
			}
			checkNumRoutes(t, sa, tc.expectedRoutes)
			if tc.expectedRoutes > 0 {
				checkClusterFormed(t, sa, sb)
			}
			sa.mu.RLock()
			ip := sa.routeInfo.IP
			if ip != tc.expectedIP {
				sa.mu.RUnlock()
				t.Fatalf("routeInfo.IP = %q, expected %q", ip, tc.expectedIP)
			}
			for _, self := range tc.expectedSelf {
				if _, ok := sa.unixRoutesToSelf[self]; !ok {
					sa.mu.RUnlock()
					t.Fatalf("expected unixRoutesToSelf to contain %q", self)
				}
			}
			for _, self := range tc.unexpectedSelf {
				if _, ok := sa.unixRoutesToSelf[self]; ok {
					sa.mu.RUnlock()
					t.Fatalf("expected unixRoutesToSelf not to contain %q", self)
				}
			}
			sa.mu.RUnlock()
		})
	}
}

// TestRouteUnixTLS covers TLS on routes over unix sockets, where the URL
// has no hostname to verify the certificate against (design doc §6.12).
func TestRouteUnixTLS(t *testing.T) {
	skipIfNoUnixSockets(t)

	const tlsBlock = `
  tls {
    cert_file: "../test/configs/certs/server-cert.pem"
    key_file: "../test/configs/certs/server-key.pem"
    ca_file: "../test/configs/certs/ca.pem"
    timeout: 2
    %s
  }`
	for _, tc := range []struct {
		description string
		// extraRoutes are configured on A besides the unix route to B.
		extraRoutes []string
		insecure    bool
		// expectedFormed says whether A and B end up routed.
		expectedFormed bool
		expectedErr    string
	}{
		// positive
		{description: "hostname from a configured tcp route is used as ServerName", extraRoutes: []string{"nats-route://localhost:1"}, expectedFormed: true},
		{description: "insecure skips verification and connects", insecure: true, expectedFormed: true},
		// negative
		{description: "no hostname anywhere is a clear error", expectedFormed: false, expectedErr: errRouteTLSUnixNoName.Error()},
		{description: "ip-only tcp route gives no usable name", extraRoutes: []string{"nats-route://127.0.0.1:1"}, expectedFormed: false, expectedErr: errRouteTLSUnixNoName.Error()},
	} {
		t.Run(tc.description, func(t *testing.T) {
			insecure := _EMPTY_
			if tc.insecure {
				insecure = "insecure: true"
			}
			bSock := tempSocketPath(t, "b.sock")
			bConf := unixClusterConf("b", bSock, _EMPTY_)
			bConf = strings.Replace(bConf, "}\n", fmt.Sprintf(tlsBlock, _EMPTY_)+"\n}\n", 1)
			ob, _ := newOptionsFromContent(t, []byte(bConf))
			ob.NoLog = true
			sb := RunServer(ob)
			defer sb.Shutdown()

			routes := append([]string{unixSchemePrefix + bSock}, tc.extraRoutes...)
			aConf := unixClusterConf("a", tempSocketPath(t, "a.sock"), _EMPTY_, routes...)
			aConf = strings.Replace(aConf, "}\n", fmt.Sprintf(tlsBlock, insecure)+"\n}\n", 1)
			oa, _ := newOptionsFromContent(t, []byte(aConf))
			oa.NoLog = true
			oa.Cluster.resolver = &localhostResolver{}
			sa, err := NewServer(oa)
			if err != nil {
				t.Fatal(err)
			}
			l := &captureErrorLogger{errCh: make(chan string, 64)}
			sa.SetLogger(l, false, false)
			sa.Start()
			defer sa.Shutdown()

			if tc.expectedFormed {
				checkClusterFormed(t, sa, sb)
				return
			}
			var seen string
			checkFor(t, 3*time.Second, 10*time.Millisecond, func() error {
				for {
					select {
					case e := <-l.errCh:
						if strings.Contains(e, tc.expectedErr) {
							seen = e
							return nil
						}
					default:
						return fmt.Errorf("error containing %q not logged yet", tc.expectedErr)
					}
				}
			})
			if seen == _EMPTY_ {
				t.Fatalf("expected an error containing %q", tc.expectedErr)
			}
			if sa.NumRoutes() != 0 || sb.NumRoutes() != 0 {
				t.Fatalf("expected no routes, got %d and %d", sa.NumRoutes(), sb.NumRoutes())
			}
		})
	}
}

// meshServer is one server in a topology row.
type meshServer struct {
	name string
	// tcp, when true, listens on a random TCP port instead of a unix socket.
	tcp bool
	// advertise is a unix advertise address, or empty.
	advertise string
	// routes names the servers this one has explicit routes to.
	routes []string
}

// runMesh starts the servers of a topology and returns them by name along
// with the socket paths.
func runMesh(t *testing.T, servers []meshServer) map[string]*Server {
	t.Helper()
	opts := make(map[string]*Options, len(servers))
	for _, ms := range servers {
		var o *Options
		if ms.tcp {
			o = DefaultOptions()
		} else {
			o = defaultUnixClusterOptions(t, ms.name+".sock")
		}
		o.ServerName = ms.name
		o.Cluster.Name = "mesh"
		o.Cluster.Advertise = ms.advertise
		opts[ms.name] = o
	}
	// TCP servers need their port before others can route to them, so
	// start them first, then everything else.
	srvs := make(map[string]*Server, len(servers))
	for _, ms := range servers {
		if ms.tcp {
			srvs[ms.name] = RunServer(opts[ms.name])
		}
	}
	for _, ms := range servers {
		o := opts[ms.name]
		for _, peer := range ms.routes {
			po := opts[peer]
			if po.Cluster.UnixSocket != _EMPTY_ {
				o.Routes = append(o.Routes, unixRouteURL(po.Cluster.UnixSocket))
			} else {
				o.Routes = append(o.Routes, RoutesFromStr(fmt.Sprintf("nats-route://127.0.0.1:%d", po.Cluster.Port))...)
			}
		}
		if !ms.tcp {
			srvs[ms.name] = RunServer(o)
		} else if len(o.Routes) > 0 {
			// TCP servers were started without routes; add them now.
			if err := srvs[ms.name].ReloadOptions(o); err != nil {
				t.Fatalf("unable to add routes to %s: %v", ms.name, err)
			}
		}
	}
	t.Cleanup(func() {
		for _, s := range srvs {
			s.Shutdown()
		}
	})
	return srvs
}

// checkMeshTraffic publishes on one server and expects delivery on another.
func checkMeshTraffic(t *testing.T, from, to *Server, subject string) {
	t.Helper()
	ncTo, err := nats.Connect(to.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer ncTo.Close()
	sub, err := ncTo.SubscribeSync(subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := ncTo.Flush(); err != nil {
		t.Fatal(err)
	}
	// Wait for the subscription interest to propagate across the route.
	checkSubInterest(t, from, globalAccountName, subject, 2*time.Second)

	ncFrom, err := nats.Connect(from.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer ncFrom.Close()
	if err := ncFrom.Publish(subject, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := ncFrom.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("message published on %s did not reach %s: %v", from.Name(), to.Name(), err)
	}
}

// TestRouteUnixThreeServerMesh runs three servers with unix socket listeners
// and a full explicit mesh (design doc §7.1).
func TestRouteUnixThreeServerMesh(t *testing.T) {
	skipIfNoUnixSockets(t)

	srvs := runMesh(t, []meshServer{
		{name: "a", routes: []string{"b", "c"}},
		{name: "b", routes: []string{"a", "c"}},
		{name: "c", routes: []string{"a", "b"}},
	})
	sa, sb, sc := srvs["a"], srvs["b"], srvs["c"]
	checkClusterFormed(t, sa, sb, sc)

	// The duplicate-route dance (A dials B while B dials A) leaves exactly
	// one route per pair; checkClusterFormed already asserts the count.
	checkMeshTraffic(t, sc, sa, "mesh.ca")
	checkMeshTraffic(t, sa, sb, "mesh.ab")
	checkMeshTraffic(t, sb, sc, "mesh.bc")

	// Restart B on the same path; the mesh re-forms.
	ob := sb.getOpts()
	sb.Shutdown()
	checkClusterFormed(t, sa, sc)
	sb = RunServer(ob)
	defer sb.Shutdown()
	checkClusterFormed(t, sa, sb, sc)
	checkMeshTraffic(t, sb, sa, "mesh.ba")

	// No socket files remain after shutdown.
	paths := []string{sa.getOpts().Cluster.UnixSocket, ob.Cluster.UnixSocket, sc.getOpts().Cluster.UnixSocket}
	sa.Shutdown()
	sb.Shutdown()
	sc.Shutdown()
	for _, p := range paths {
		if _, err := os.Lstat(nativeUnixAddr(p)); !os.IsNotExist(err) {
			t.Fatalf("socket file %q still present after shutdown (err=%v)", p, err)
		}
	}
}

// TestRouteUnixMeshTopologies runs the mesh in several route layouts
// (design doc §7.1).
func TestRouteUnixMeshTopologies(t *testing.T) {
	skipIfNoUnixSockets(t)

	for _, tc := range []struct {
		description string
		servers     []meshServer
	}{
		{
			description: "full explicit mesh",
			servers: []meshServer{
				{name: "a", routes: []string{"b", "c"}},
				{name: "b", routes: []string{"a", "c"}},
				{name: "c", routes: []string{"a", "b"}},
			},
		},
		{
			description: "ring without advertise, each pair has one explicit route",
			servers: []meshServer{
				{name: "a", routes: []string{"b"}},
				{name: "b", routes: []string{"c"}},
				{name: "c", routes: []string{"a"}},
			},
		},
		{
			description: "ring with advertise, gossip adds nothing and no duplicates",
			servers: []meshServer{
				{name: "a", routes: []string{"b"}, advertise: "unix:///" + strings.TrimPrefix(tempSocketPath(t, "adv-a.sock"), "/")},
				{name: "b", routes: []string{"c"}, advertise: "unix:///" + strings.TrimPrefix(tempSocketPath(t, "adv-b.sock"), "/")},
				{name: "c", routes: []string{"a"}, advertise: "unix:///" + strings.TrimPrefix(tempSocketPath(t, "adv-c.sock"), "/")},
			},
		},
		{
			description: "star, one server dials the other two",
			servers: []meshServer{
				{name: "a", routes: []string{"b", "c"}},
				{name: "b"},
				{name: "c", routes: []string{"b"}},
			},
		},
		{
			description: "mixed transport, a-b over tcp, b-c and c-a over unix",
			servers: []meshServer{
				{name: "a", tcp: true},
				{name: "b", routes: []string{"a", "c"}},
				{name: "c", routes: []string{"a"}},
			},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			srvs := runMesh(t, tc.servers)
			sa, sb, sc := srvs["a"], srvs["b"], srvs["c"]
			checkClusterFormed(t, sa, sb, sc)
			checkMeshTraffic(t, sa, sc, "topo.ac")
			checkMeshTraffic(t, sc, sb, "topo.cb")
			// Stay formed: gossip must not tear down or duplicate routes.
			time.Sleep(200 * time.Millisecond)
			checkClusterFormed(t, sa, sb, sc)
		})
	}
}

// monitoringUnixPair starts an acceptor B on a unix socket and a dialer A
// with an explicit route to it, both with an HTTP monitor and a system
// account user, and waits for the cluster to form. With pool_size -1 each
// side has exactly one route.
func monitoringUnixPair(t *testing.T) (sa, sb *Server, oa, ob *Options) {
	t.Helper()
	ob = defaultUnixClusterOptions(t, "b.sock")
	monitoringOptions(ob)
	sb = RunServer(ob)
	t.Cleanup(sb.Shutdown)

	oa = defaultUnixClusterOptions(t, "a.sock")
	monitoringOptions(oa)
	oa.Routes = []*url.URL{unixRouteURL(ob.Cluster.UnixSocket)}
	sa = RunServer(oa)
	t.Cleanup(sa.Shutdown)

	checkClusterFormed(t, sa, sb)
	return sa, sb, oa, ob
}

// monitoringOptions enables the HTTP monitor, disables route pooling and
// adds a system account user so STATSZ can be requested.
func monitoringOptions(o *Options) {
	o.HTTPHost = "127.0.0.1"
	o.HTTPPort = -1
	o.Cluster.PoolSize = -1
	sys := NewAccount("SYS")
	o.Accounts = []*Account{sys}
	o.SystemAccount = "SYS"
	o.Users = []*User{{Username: "sys", Password: "pwd", Account: sys}}
}

// statszRoutes requests STATSZ from a server through its system account
// and returns the route stats it reports.
func statszRoutes(t *testing.T, s *Server) []*RouteStat {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL(), nats.UserInfo("sys", "pwd"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	msg, err := nc.Request("$SYS.REQ.SERVER.PING.STATSZ", nil, 2*time.Second)
	if err != nil {
		t.Fatalf("STATSZ request failed: %v", err)
	}
	var m ServerStatsMsg
	if err := json.Unmarshal(msg.Data, &m); err != nil {
		t.Fatalf("unable to decode STATSZ: %v", err)
	}
	return m.Stats.Routes
}

// varzModes polls /varz over HTTP and through the Server API and hands
// both results to check, so JSON encoding and the in-process struct are
// covered by the same assertions.
func varzModes(t *testing.T, s *Server, check func(t *testing.T, v *Varz)) {
	t.Helper()
	for mode, name := range []string{"http", "api"} {
		t.Run(name, func(t *testing.T) {
			v := pollVarz(t, s, mode, fmt.Sprintf("http://127.0.0.1:%d/varz", s.MonitorAddr().Port), nil)
			check(t, v)
		})
	}
}

// routezModes is varzModes for /routez.
func routezModes(t *testing.T, s *Server, check func(t *testing.T, rz *Routez)) {
	t.Helper()
	for mode, name := range []string{"http", "api"} {
		t.Run(name, func(t *testing.T) {
			rz := pollRoutez(t, s, mode, fmt.Sprintf("http://127.0.0.1:%d/routez", s.MonitorAddr().Port), nil)
			check(t, rz)
		})
	}
}

// TestRouteUnixMonitoring covers the /varz, /routez, PortsInfo and STATSZ
// reporting of unix socket routes (design doc §6.10).
func TestRouteUnixMonitoring(t *testing.T) {
	skipIfNoUnixSockets(t)
	resetPreviousHTTPConnections()

	t.Run("varz reports the unix listener instead of host and port", func(t *testing.T) {
		sa, _, oa, ob := monitoringUnixPair(t)
		varzModes(t, sa, func(t *testing.T, v *Varz) {
			if v.Cluster.UnixSocket != oa.Cluster.UnixSocket {
				t.Fatalf("cluster.unix_socket = %q, expected %q", v.Cluster.UnixSocket, oa.Cluster.UnixSocket)
			}
			if v.Cluster.Host != _EMPTY_ || v.Cluster.Port != 0 {
				t.Fatalf("cluster addr/port = %q/%d, expected empty", v.Cluster.Host, v.Cluster.Port)
			}
			if expected := []string{unixSchemePrefix + ob.Cluster.UnixSocket}; !reflect.DeepEqual(v.Cluster.URLs, expected) {
				t.Fatalf("cluster urls = %q, expected %q", v.Cluster.URLs, expected)
			}
		})
		body := string(readBody(t, fmt.Sprintf("http://127.0.0.1:%d/varz", sa.MonitorAddr().Port)))
		for _, absent := range []string{`"cluster_port"`, `"addr"`} {
			if strings.Contains(body, absent) {
				t.Fatalf("/varz should omit %s for a unix listener", absent)
			}
		}
		if !strings.Contains(body, `"unix_socket": "`+oa.Cluster.UnixSocket+`"`) {
			t.Fatalf("/varz should report the unix socket, got:\n%s", body)
		}
	})

	t.Run("varz counters", func(t *testing.T) {
		sa, sb, _, _ := monitoringUnixPair(t)
		for _, tc := range []struct {
			description string
			s           *Server
			expected    RouteUnixSocketStats
		}{
			{description: "dialer", s: sa, expected: RouteUnixSocketStats{Dialed: 1, Active: 1}},
			{description: "acceptor", s: sb, expected: RouteUnixSocketStats{Accepted: 1, Active: 1}},
		} {
			t.Run(tc.description, func(t *testing.T) {
				varzModes(t, tc.s, func(t *testing.T, v *Varz) {
					if v.Cluster.UnixSocketStats == nil {
						t.Fatal("expected unix_socket_stats to be present")
					}
					if got := *v.Cluster.UnixSocketStats; got != tc.expected {
						t.Fatalf("unix_socket_stats = %+v, expected %+v", got, tc.expected)
					}
				})
			})
		}
	})

	t.Run("varz dial errors are counted for a missing peer", func(t *testing.T) {
		oa := defaultUnixClusterOptions(t, "a.sock")
		monitoringOptions(oa)
		oa.Routes = []*url.URL{unixRouteURL(tempSocketPath(t, "missing.sock"))}
		sa := RunServer(oa)
		defer sa.Shutdown()
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			v, _ := sa.Varz(nil)
			st := v.Cluster.UnixSocketStats
			switch {
			case st == nil:
				return fmt.Errorf("expected unix_socket_stats")
			case st.DialErrors == 0:
				return fmt.Errorf("expected dial_errors to move")
			case st.Dialed != 0 || st.Active != 0 || st.Accepted != 0:
				return fmt.Errorf("unexpected counters %+v", *st)
			}
			return nil
		})
	})

	t.Run("routez reports the unix transport", func(t *testing.T) {
		sa, sb, _, ob := monitoringUnixPair(t)
		for _, tc := range []struct {
			description        string
			s                  *Server
			expectedSolicit    bool
			expectedUnixSocket string
		}{
			{description: "dialer knows the peer socket", s: sa, expectedSolicit: true, expectedUnixSocket: ob.Cluster.UnixSocket},
			{description: "acceptor sees an unnamed peer", s: sb, expectedSolicit: false, expectedUnixSocket: _EMPTY_},
		} {
			t.Run(tc.description, func(t *testing.T) {
				routezModes(t, tc.s, func(t *testing.T, rz *Routez) {
					if len(rz.Routes) != 1 {
						t.Fatalf("expected 1 route, got %d", len(rz.Routes))
					}
					ri := rz.Routes[0]
					if ri.Transport != routeTransportUnix {
						t.Fatalf("transport = %q, expected %q", ri.Transport, routeTransportUnix)
					}
					if ri.IP != _EMPTY_ || ri.Port != 0 {
						t.Fatalf("ip/port = %q/%d, expected empty", ri.IP, ri.Port)
					}
					if ri.DidSolicit != tc.expectedSolicit {
						t.Fatalf("did_solicit = %v, expected %v", ri.DidSolicit, tc.expectedSolicit)
					}
					if ri.UnixSocket != tc.expectedUnixSocket {
						t.Fatalf("unix_socket = %q, expected %q", ri.UnixSocket, tc.expectedUnixSocket)
					}
				})
			})
		}
	})

	t.Run("routez tcp regression", func(t *testing.T) {
		ob := DefaultOptions()
		monitoringOptions(ob)
		sb := RunServer(ob)
		defer sb.Shutdown()
		oa := DefaultOptions()
		monitoringOptions(oa)
		oa.Routes = RoutesFromStr(fmt.Sprintf("nats-route://127.0.0.1:%d", ob.Cluster.Port))
		sa := RunServer(oa)
		defer sa.Shutdown()
		checkClusterFormed(t, sa, sb)

		for _, tc := range []struct {
			description  string
			s            *Server
			expectedPort int
		}{
			{description: "dialer reports the listener port", s: sa, expectedPort: ob.Cluster.Port},
			{description: "acceptor reports an ephemeral port", s: sb},
		} {
			t.Run(tc.description, func(t *testing.T) {
				routezModes(t, tc.s, func(t *testing.T, rz *Routez) {
					if len(rz.Routes) != 1 {
						t.Fatalf("expected 1 route, got %d", len(rz.Routes))
					}
					ri := rz.Routes[0]
					if ri.Transport != routeTransportTCP {
						t.Fatalf("transport = %q, expected %q", ri.Transport, routeTransportTCP)
					}
					if ri.IP != "127.0.0.1" || ri.Port == 0 {
						t.Fatalf("ip/port = %q/%d, expected 127.0.0.1 and a port", ri.IP, ri.Port)
					}
					if tc.expectedPort != 0 && ri.Port != tc.expectedPort {
						t.Fatalf("port = %d, expected %d", ri.Port, tc.expectedPort)
					}
					if ri.UnixSocket != _EMPTY_ {
						t.Fatalf("unix_socket = %q, expected empty", ri.UnixSocket)
					}
				})
			})
		}
		// A TCP-only server never reports unix socket stats.
		varzModes(t, sa, func(t *testing.T, v *Varz) {
			if v.Cluster.UnixSocket != _EMPTY_ || v.Cluster.UnixSocketStats != nil {
				t.Fatalf("unexpected unix fields in tcp varz: %q %+v", v.Cluster.UnixSocket, v.Cluster.UnixSocketStats)
			}
		})
		for _, s := range []*Server{sa, sb} {
			for _, rs := range statszRoutes(t, s) {
				if rs.Transport != routeTransportTCP {
					t.Fatalf("%s STATSZ route transport = %q, expected %q", s.Name(), rs.Transport, routeTransportTCP)
				}
			}
		}
	})

	t.Run("statsz reports the unix transport", func(t *testing.T) {
		sa, sb, _, _ := monitoringUnixPair(t)
		for _, s := range []*Server{sa, sb} {
			routes := statszRoutes(t, s)
			if len(routes) != 1 {
				t.Fatalf("%s: expected 1 route in STATSZ, got %d", s.Name(), len(routes))
			}
			if routes[0].Transport != routeTransportUnix {
				t.Fatalf("%s: STATSZ route transport = %q, expected %q", s.Name(), routes[0].Transport, routeTransportUnix)
			}
		}
	})

	t.Run("ports file lists the unix socket", func(t *testing.T) {
		sa, _, oa, _ := monitoringUnixPair(t)
		ports := sa.PortsInfo(time.Second)
		expected := []string{unixSchemePrefix + oa.Cluster.UnixSocket}
		if !reflect.DeepEqual(ports.Cluster, expected) {
			t.Fatalf("PortsInfo().Cluster = %v, expected %v", ports.Cluster, expected)
		}
	})

	t.Run("counters after the dialer restarts", func(t *testing.T) {
		sa, sb, oa, _ := monitoringUnixPair(t)
		sa.Shutdown()
		sa.WaitForShutdown()
		checkFor(t, 2*time.Second, 10*time.Millisecond, func() error {
			v, _ := sb.Varz(nil)
			if st := v.Cluster.UnixSocketStats; st == nil || st.Active != 0 {
				return fmt.Errorf("expected no active unix routes after the dialer left")
			}
			return nil
		})
		sa2 := RunServer(oa)
		defer sa2.Shutdown()
		checkClusterFormed(t, sa2, sb)

		for _, tc := range []struct {
			description string
			s           *Server
			expected    RouteUnixSocketStats
		}{
			{description: "acceptor counted both connections", s: sb, expected: RouteUnixSocketStats{Accepted: 2, Active: 1}},
			{description: "restarted dialer starts from zero", s: sa2, expected: RouteUnixSocketStats{Dialed: 1, Active: 1}},
		} {
			t.Run(tc.description, func(t *testing.T) {
				v, _ := tc.s.Varz(nil)
				if v.Cluster.UnixSocketStats == nil {
					t.Fatal("expected unix_socket_stats")
				}
				if got := *v.Cluster.UnixSocketStats; got != tc.expected {
					t.Fatalf("unix_socket_stats = %+v, expected %+v", got, tc.expected)
				}
			})
		}
	})

	t.Run("counters after a stale socket is removed", func(t *testing.T) {
		o := defaultUnixClusterOptions(t, "stale.sock")
		monitoringOptions(o)
		leaveStaleSocket(t, nativeUnixAddr(o.Cluster.UnixSocket))
		s := RunServer(o)
		defer s.Shutdown()
		varzModes(t, s, func(t *testing.T, v *Varz) {
			expected := RouteUnixSocketStats{StaleRemoved: 1}
			if v.Cluster.UnixSocketStats == nil {
				t.Fatal("expected unix_socket_stats")
			}
			if got := *v.Cluster.UnixSocketStats; got != expected {
				t.Fatalf("unix_socket_stats = %+v, expected %+v", got, expected)
			}
		})
	})

	t.Run("routez after the peer closes keeps the transport", func(t *testing.T) {
		// The transport is captured at creation, so a route whose
		// connection is already gone still reports it.
		r := &client{kind: ROUTER, route: &route{transport: routeTransportUnix}}
		if got := routeStat(r).Transport; got != routeTransportUnix {
			t.Fatalf("routeStat transport = %q, expected %q", got, routeTransportUnix)
		}
	})
}

// TestRouteTransport covers routeTransport over the connection types a
// route can have.
func TestRouteTransport(t *testing.T) {
	skipIfNoUnixSockets(t)
	unixPath := nativeUnixAddr(tempSocketPath(t, "t.sock"))
	ul, err := net.Listen("unix", unixPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	dial := func(network, addr string) net.Conn {
		c, err := net.Dial(network, addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	for _, tc := range []struct {
		description string
		nc          net.Conn
		expected    string
	}{
		// positive
		{description: "unix connection", nc: dial("unix", unixPath), expected: routeTransportUnix},
		{description: "tcp connection", nc: dial("tcp", tl.Addr().String()), expected: routeTransportTCP},
		// negative / corner
		{description: "nil connection defaults to tcp", nc: nil, expected: routeTransportTCP},
		{description: "fake connection with a tcp address", nc: &fakeConn{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}}, expected: routeTransportTCP},
		{description: "fake connection with a unix address", nc: &fakeConn{addr: &net.UnixAddr{Name: "/x", Net: "unix"}}, expected: routeTransportUnix},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := routeTransport(tc.nc); got != tc.expected {
				t.Fatalf("routeTransport = %q, expected %q", got, tc.expected)
			}
		})
	}
}

// fakeConn is a net.Conn that only knows its remote address.
type fakeConn struct {
	net.Conn
	addr net.Addr
}

func (f *fakeConn) RemoteAddr() net.Addr { return f.addr }

// TestVarzClusterTCPGolden pins the /varz cluster fragment of a TCP-only
// server so the unix socket fields never leak into it.
func TestVarzClusterTCPGolden(t *testing.T) {
	resetPreviousHTTPConnections()
	o := DefaultOptions()
	o.Cluster.Name = "abc"
	o.Cluster.Host = "127.0.0.1"
	o.HTTPHost = "127.0.0.1"
	o.HTTPPort = -1
	s := RunServer(o)
	defer s.Shutdown()

	expected := fmt.Sprintf(`{"name":"abc","addr":"127.0.0.1","cluster_port":%d,"auth_timeout":2,"tls_timeout":2,"pool_size":3}`, o.Cluster.Port)
	varzModes(t, s, func(t *testing.T, v *Varz) {
		b, err := json.Marshal(v.Cluster)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != expected {
			t.Fatalf("cluster varz = %s, expected %s", b, expected)
		}
	})
	body := string(readBody(t, fmt.Sprintf("http://127.0.0.1:%d/varz", s.MonitorAddr().Port)))
	if strings.Contains(body, "unix") {
		t.Fatalf("/varz of a tcp-only server mentions unix:\n%s", body)
	}
}

// TestURLsToStringsUnix covers the /varz route URL rendering for unix,
// TCP and mixed route lists.
func TestURLsToStringsUnix(t *testing.T) {
	t.Parallel()
	tcp := RoutesFromStr("nats-route://127.0.0.1:6222")[0]
	for _, tc := range []struct {
		description string
		in          []*url.URL
		expected    []string
	}{
		// positive
		{description: "tcp route keeps host:port", in: []*url.URL{tcp}, expected: []string{"127.0.0.1:6222"}},
		{description: "unix pathname route", in: []*url.URL{unixRouteURL("/run/nats/b.sock")}, expected: []string{"unix:///run/nats/b.sock"}},
		{description: "abstract unix route", in: []*url.URL{unixRouteURL("@nats-b")}, expected: []string{"unix://@nats-b"}},
		{description: "mixed list keeps order", in: []*url.URL{tcp, unixRouteURL("/run/nats/b.sock")}, expected: []string{"127.0.0.1:6222", "unix:///run/nats/b.sock"}},
		// boundary
		{description: "empty list", in: nil, expected: []string{}},
		// corner: a parsed unix URL (User/Host form) renders the same as the canonical one
		{description: "parsed abstract url", in: []*url.URL{mustParseURL(t, "unix://@nats-b")}, expected: []string{"unix://@nats-b"}},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := urlsToStrings(tc.in); !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("urlsToStrings = %q, expected %q", got, tc.expected)
			}
		})
	}
}
