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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
