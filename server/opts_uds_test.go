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
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// expectedClusterOpts is the subset of ClusterOpts and Options.Routes that
// the unix socket configuration tests compare.
type expectedClusterOpts struct {
	unixSocket string
	host       string
	port       int
	advertise  string
	// routes holds the String() form of each canonical route URL, in order.
	routes []string
}

func checkClusterOpts(t *testing.T, opts *Options, expected expectedClusterOpts) {
	t.Helper()
	if opts.Cluster.UnixSocket != expected.unixSocket {
		t.Errorf("UnixSocket = %q, expected %q", opts.Cluster.UnixSocket, expected.unixSocket)
	}
	if opts.Cluster.Host != expected.host {
		t.Errorf("Host = %q, expected %q", opts.Cluster.Host, expected.host)
	}
	if opts.Cluster.Port != expected.port {
		t.Errorf("Port = %d, expected %d", opts.Cluster.Port, expected.port)
	}
	if opts.Cluster.Advertise != expected.advertise {
		t.Errorf("Advertise = %q, expected %q", opts.Cluster.Advertise, expected.advertise)
	}
	var routes []string
	for _, u := range opts.Routes {
		if u == nil {
			routes = append(routes, "<nil>")
			continue
		}
		routes = append(routes, u.String())
	}
	if fmt.Sprint(routes) != fmt.Sprint(expected.routes) {
		t.Errorf("Routes = %v, expected %v", routes, expected.routes)
	}
}

// TestClusterOptsUnixSocketConfig covers the configuration file surface for
// unix socket route listeners, routes and advertise (design doc §6.4).
func TestClusterOptsUnixSocketConfig(t *testing.T) {
	// A pathname may use sun_path minus the NUL terminator.
	maxPath := "/" + strings.Repeat("a", maxUnixSocketPathLen-2)

	type row struct {
		description string
		config      string
		expected    expectedClusterOpts
		// expectedErr, when set, must be contained in the returned error.
		expectedErr string
		// expectedWarn, when set, must be contained in one warning and the
		// configuration must otherwise be accepted.
		expectedWarn string
	}
	rows := []row{
		// positive
		{
			description: "listen unix sets UnixSocket and leaves host and port zero",
			config:      `cluster { listen: "unix:///run/a.sock" }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "abstract listen",
			config:      `cluster { listen: "unix://@a" }`,
			expected:    expectedClusterOpts{unixSocket: "@a"},
		},
		{
			description: "unix routes are canonicalised",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["unix:///run/b.sock", "unix://@c"] }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock", "unix://@c"}},
		},
		{
			description: "mixed tcp and unix routes keep their order",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["nats-route://127.0.0.1:6222", "unix:///run/b.sock"] }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"nats-route://127.0.0.1:6222", "unix:///run/b.sock"}},
		},
		{
			description: "unix routes with a tcp listener",
			config:      `cluster { listen: "127.0.0.1:6222", routes: ["unix:///run/b.sock"] }`,
			expected:    expectedClusterOpts{host: "127.0.0.1", port: 6222, routes: []string{"unix:///run/b.sock"}},
		},
		{
			description: "unix advertise with unix listener",
			config:      `cluster { listen: "unix:///run/a.sock", advertise: "unix:///run/p/a.sock" }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", advertise: "unix:///run/p/a.sock"},
		},
		{
			description: "abstract advertise with unix listener",
			config:      `cluster { listen: "unix:///run/a.sock", advertise: "unix://@proxy-a" }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", advertise: "unix://@proxy-a"},
		},
		{
			description: "cluster_advertise alias with unix listener",
			config:      `cluster { listen: "unix:///run/a.sock", cluster_advertise: "unix:///run/p/a.sock" }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", advertise: "unix:///run/p/a.sock"},
		},
		{
			description: "tcp cluster is untouched",
			config:      `cluster { listen: "127.0.0.1:6222" }`,
			expected:    expectedClusterOpts{host: "127.0.0.1", port: 6222},
		},
		{
			description: "tcp cluster with tcp advertise is untouched",
			config:      `cluster { listen: "127.0.0.1:6222", advertise: "10.0.0.1:6222" }`,
			expected:    expectedClusterOpts{host: "127.0.0.1", port: 6222, advertise: "10.0.0.1:6222"},
		},
		{
			description: "unix listener with authorization block",
			config:      `cluster { listen: "unix:///run/a.sock", authorization { user: ruser, password: top_secret } }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "advertise only, listener may come from the -cluster flag",
			config:      `cluster { advertise: "unix:///run/p/a.sock" }`,
			expected:    expectedClusterOpts{advertise: "unix:///run/p/a.sock"},
		},
		// negative
		{
			description: "unix listen plus port",
			config:      `cluster { listen: "unix:///run/a.sock", port: 6222 }`,
			expectedErr: "unix socket listen and host/port are mutually exclusive",
		},
		{
			description: "unix listen plus host",
			config:      `cluster { listen: "unix:///run/a.sock", host: "127.0.0.1" }`,
			expectedErr: "unix socket listen and host/port are mutually exclusive",
		},
		{
			description: "unix listen plus net alias",
			config:      `cluster { listen: "unix:///run/a.sock", net: "127.0.0.1" }`,
			expectedErr: "unix socket listen and host/port are mutually exclusive",
		},
		{
			description: "unix advertise with tcp listener",
			config:      `cluster { listen: "127.0.0.1:6222", advertise: "unix:///run/a.sock" }`,
			expectedErr: `advertise transport "unix" does not match listener transport "tcp"`,
		},
		{
			description: "unix advertise with port only listener",
			config:      `cluster { port: 6222, advertise: "unix:///run/a.sock" }`,
			expectedErr: `advertise transport "unix" does not match listener transport "tcp"`,
		},
		{
			description: "tcp advertise with unix listener",
			config:      `cluster { listen: "unix:///run/a.sock", advertise: "10.0.0.1:6222" }`,
			expectedErr: `advertise transport "tcp" does not match listener transport "unix"`,
		},
		{
			description: "invalid unix advertise",
			config:      `cluster { listen: "unix:///run/a.sock", advertise: "unix://run/a.sock" }`,
			expectedErr: "must be an absolute path",
		},
		{
			description: "invalid unix route",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["unix://run/b.sock"] }`,
			expectedErr: "must be an absolute path",
		},
		{
			description: "invalid unix route error names the route",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["unix://run/b.sock"] }`,
			expectedErr: `error parsing route url ["unix://run/b.sock"]`,
		},
		{
			description: "unix route with a query",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["unix:///run/b.sock?x=1"] }`,
			expectedErr: `must not contain a "?" query`,
		},
		{
			description: "invalid unix listen, directory",
			config:      `cluster { listen: "unix:///run/a.sock/" }`,
			expectedErr: "not a directory",
		},
		{
			description: "invalid unix listen, relative",
			config:      `cluster { listen: "unix://run/a.sock" }`,
			expectedErr: "must be an absolute path",
		},
		{
			description: "invalid unix listen, empty",
			config:      `cluster { listen: "unix://" }`,
			expectedErr: "has no socket path",
		},
		{
			description: "invalid unix listen, bare abstract marker",
			config:      `cluster { listen: "unix://@" }`,
			expectedErr: "no abstract socket name",
		},
		{
			description: "unix listen with percent",
			config:      `cluster { listen: "unix:///run/a%20b.sock" }`,
			expectedErr: `must not contain "%"`,
		},
		{
			description: "unix listen with credentials is not a valid address",
			config:      `cluster { listen: "unix://user:pass@/run/a.sock" }`,
			expectedErr: "must be an absolute path",
		},
		// boundary
		{
			description: "listen path at OS limit",
			config:      fmt.Sprintf(`cluster { listen: "unix://%s" }`, maxPath),
			expected:    expectedClusterOpts{unixSocket: maxPath},
		},
		{
			description: "listen path one over OS limit",
			config:      fmt.Sprintf(`cluster { listen: "unix://%sa" }`, maxPath),
			expectedErr: "too long",
		},
		{
			description: "route path one over OS limit",
			config:      fmt.Sprintf(`cluster { listen: "unix:///run/a.sock", routes: ["unix://%sa"] }`, maxPath),
			expectedErr: "too long",
		},
		{
			description: "shortest possible listen path",
			config:      `cluster { listen: "unix:///a" }`,
			expected:    expectedClusterOpts{unixSocket: "/a"},
		},
		// corner
		{
			description:  "duplicate unix route is a warning, second copy dropped",
			config:       `cluster { listen: "unix:///run/a.sock", routes: ["unix:///run/b.sock", "unix:///run/b.sock"] }`,
			expected:     expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock"}},
			expectedWarn: "Duplicate route entry detected",
		},
		{
			description:  "route duplicate detection is textual, spelling variants both kept",
			config:       `cluster { listen: "unix:///run/a.sock", routes: ["unix:///run/b.sock", "UNIX:///run/b.sock"] }`,
			expected:     expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock", "unix:///run/b.sock"}},
			expectedWarn: "",
		},
		{
			description: "scheme case in listen",
			config:      `cluster { listen: "UNIX:///run/a.sock" }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "scheme case in route",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["Unix:///run/b.sock"] }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock"}},
		},
		{
			description: "route surrounded by spaces is trimmed like tcp routes",
			config:      `cluster { listen: "unix:///run/a.sock", routes: ["  unix:///run/b.sock  "] }`,
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock"}},
		},
		{
			description: "unquoted unix listen is accepted, the lexer does not treat // after : as a comment",
			config:      "cluster {\n  listen: unix:///run/a.sock\n}",
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "unquoted abstract listen is accepted",
			config:      "cluster {\n  listen: unix://@a\n}",
			expected:    expectedClusterOpts{unixSocket: "@a"},
		},
		{
			description: "listen given as a bare port is still tcp",
			config:      `cluster { listen: 6222 }`,
			expected:    expectedClusterOpts{port: 6222},
		},
		{
			description: "unix listen with a windows drive path is accepted on every OS",
			config:      `cluster { listen: "unix:///C:/nats/a.sock" }`,
			expected:    expectedClusterOpts{unixSocket: "/C:/nats/a.sock"},
		},
	}
	if runtime.GOOS == "linux" {
		rows = append(rows, row{
			description: "abstract listen gives no warning on linux",
			config:      `cluster { listen: "unix://@a" }`,
			expected:    expectedClusterOpts{unixSocket: "@a"},
		})
	} else {
		rows = append(rows, row{
			description:  "abstract listen warns off linux",
			config:       `cluster { listen: "unix://@a" }`,
			expected:     expectedClusterOpts{unixSocket: "@a"},
			expectedWarn: "abstract unix socket names are only supported on Linux",
		})
	}

	for _, tc := range rows {
		t.Run(tc.description, func(t *testing.T) {
			conf := createConfFile(t, []byte(tc.config))
			opts := &Options{}
			err := opts.ProcessConfigFile(conf)

			if tc.expectedErr != _EMPTY_ {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.expectedErr)
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %q", tc.expectedErr, err.Error())
				}
				return
			}

			var warnings []error
			if err != nil {
				cerr, ok := err.(*processConfigErr)
				if !ok || len(cerr.Errors()) > 0 {
					t.Fatalf("unexpected error: %v", err)
				}
				warnings = cerr.Warnings()
			}
			if tc.expectedWarn != _EMPTY_ {
				found := false
				for _, w := range warnings {
					if strings.Contains(w.Error(), tc.expectedWarn) {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected a warning containing %q, got %v", tc.expectedWarn, warnings)
				}
			} else if len(warnings) > 0 {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
			checkClusterOpts(t, opts, tc.expected)
		})
	}
}

// TestClusterUnixFlags covers -cluster, -routes and -cluster_advertise with
// unix socket values, alone and layered over a configuration file (design
// doc §6.5).
func TestClusterUnixFlags(t *testing.T) {
	// ConfigureOptions snapshots the flags for reload; reset afterwards so
	// other tests are not affected.
	defer func() { FlagSnapshot = nil }()

	tcpConf := createConfFile(t, []byte(`
		cluster {
		  listen: "127.0.0.1:6222"
		  authorization { user: ruser, password: top_secret }
		  routes: ["nats-route://ruser:top_secret@127.0.0.1:6223"]
		}
	`))
	unixConf := createConfFile(t, []byte(`
		cluster {
		  listen: "unix:///run/a.sock"
		  routes: ["unix:///run/b.sock"]
		}
	`))
	unixAdvertiseOnlyConf := createConfFile(t, []byte(`
		cluster {
		  advertise: "unix:///run/p/a.sock"
		}
	`))

	for _, tc := range []struct {
		description string
		args        []string
		expected    expectedClusterOpts
		// expectedListenStr is checked only when non-empty.
		expectedListenStr string
		expectedErr       string
	}{
		// positive
		{
			description: "-cluster unix",
			args:        []string{"-cluster", "unix:///run/a.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "-cluster abstract",
			args:        []string{"-cluster", "unix://@a"},
			expected:    expectedClusterOpts{unixSocket: "@a"},
		},
		{
			description: "-cluster_listen alias",
			args:        []string{"-cluster_listen", "unix:///run/a.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock"},
		},
		{
			description: "-cluster unix overrides config tcp listen and clears credentials",
			args:        []string{"-c", tcpConf, "-cluster", "unix:///run/a.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"nats-route://ruser:top_secret@127.0.0.1:6223"}},
		},
		{
			description: "-cluster tcp overrides config unix listen",
			args:        []string{"-c", unixConf, "-cluster", "nats://127.0.0.1:6222"},
			expected:    expectedClusterOpts{host: "127.0.0.1", port: 6222, routes: []string{"unix:///run/b.sock"}},
		},
		{
			description: "-cluster empty disables a config unix listener",
			args:        []string{"-c", unixConf, "-cluster", "", "-routes", ""},
			expected:    expectedClusterOpts{},
		},
		{
			description: "-routes unix list with -cluster unix",
			args:        []string{"-cluster", "unix:///run/a.sock", "-routes", "unix:///run/b.sock,unix://@c"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock", "unix://@c"}},
		},
		{
			description: "-routes with spaces around commas",
			args:        []string{"-cluster", "unix:///run/a.sock", "-routes", "unix:///run/b.sock, unix://@c"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/b.sock", "unix://@c"}},
		},
		{
			description: "-routes mixed tcp and unix",
			args:        []string{"-cluster", "unix:///run/a.sock", "-routes", "nats-route://127.0.0.1:6223,unix:///run/b.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"nats-route://127.0.0.1:6223", "unix:///run/b.sock"}},
		},
		{
			description: "-routes unix with config tcp listener",
			args:        []string{"-c", tcpConf, "-routes", "unix:///run/b.sock"},
			expected:    expectedClusterOpts{host: "127.0.0.1", port: 6222, routes: []string{"unix:///run/b.sock"}},
		},
		{
			description: "-routes with config unix listener, no -cluster flag",
			args:        []string{"-c", unixConf, "-routes", "unix:///run/c.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", routes: []string{"unix:///run/c.sock"}},
		},
		{
			description: "-cluster_advertise unix",
			args:        []string{"-cluster", "unix:///run/a.sock", "-cluster_advertise", "unix:///p/a.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", advertise: "unix:///p/a.sock"},
		},
		{
			description: "config with advertise only gets its listener from -cluster",
			args:        []string{"-c", unixAdvertiseOnlyConf, "-cluster", "unix:///run/a.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/a.sock", advertise: "unix:///run/p/a.sock"},
		},
		// negative
		{
			description: "invalid -routes surfaces an error instead of being dropped",
			args:        []string{"-cluster", "unix:///run/a.sock", "-routes", "unix://relative"},
			expectedErr: "must be an absolute path",
		},
		{
			description: "invalid second -routes entry surfaces an error",
			args:        []string{"-cluster", "unix:///run/a.sock", "-routes", "unix:///run/b.sock,unix:///run/c.sock/"},
			expectedErr: "not a directory",
		},
		{
			description: "invalid -cluster unix value",
			args:        []string{"-cluster", "unix://run/a.sock"},
			expectedErr: "must be an absolute path",
		},
		{
			description: "-cluster unix with a query",
			args:        []string{"-cluster", "unix:///run/a.sock?x=1"},
			expectedErr: `must not contain a "?" query`,
		},
		{
			description: "-routes without -cluster",
			args:        []string{"-routes", "unix:///run/b.sock"},
			expectedErr: "solicited routes require cluster capabilities",
		},
		// boundary
		{
			description: "-cluster unix path one over OS limit",
			args:        []string{"-cluster", "unix:///" + strings.Repeat("a", maxUnixSocketPathLen-1)},
			expectedErr: "too long",
		},
		// corner
		{
			description:       "-cluster random-port suffix is tcp-only syntax and is kept verbatim",
			args:              []string{"-cluster", "unix:///run/a.sock:-1"},
			expected:          expectedClusterOpts{unixSocket: "/run/a.sock:-1"},
			expectedListenStr: "unix:///run/a.sock:-1",
		},
		{
			description: "-cluster unix repeated, last one wins",
			args:        []string{"-cluster", "unix:///run/a.sock", "-cluster", "unix:///run/b.sock"},
			expected:    expectedClusterOpts{unixSocket: "/run/b.sock"},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			// Silence the flag set so a failure prints nothing.
			fs.SetOutput(&bytes.Buffer{})
			opts, err := ConfigureOptions(fs, tc.args, PrintServerAndExit, fs.Usage, PrintTLSHelpAndDie)
			if tc.expectedErr != _EMPTY_ {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (opts=%+v)", tc.expectedErr, opts.Cluster)
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %q", tc.expectedErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			checkClusterOpts(t, opts, tc.expected)
			if tc.expectedListenStr != _EMPTY_ && opts.Cluster.ListenStr != tc.expectedListenStr {
				t.Fatalf("ListenStr = %q, expected %q", opts.Cluster.ListenStr, tc.expectedListenStr)
			}
			if opts.Cluster.UnixSocket != _EMPTY_ && (opts.Cluster.Username != _EMPTY_ || opts.Cluster.Password != _EMPTY_) {
				t.Fatalf("credentials must be cleared for a unix listener, got %q/%q", opts.Cluster.Username, opts.Cluster.Password)
			}
		})
	}
}

// TestRoutesFromStrUnix covers the error-returning route list parser used by
// the -routes flag, and the exported wrapper's lenient behaviour.
func TestRoutesFromStrUnix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    []string
		expectedErr string
	}{
		// positive
		{description: "single unix route", in: "unix:///run/b.sock", expected: []string{"unix:///run/b.sock"}},
		{description: "two unix routes", in: "unix:///run/b.sock,unix://@c", expected: []string{"unix:///run/b.sock", "unix://@c"}},
		{description: "tcp route unchanged", in: "nats-route://127.0.0.1:6222", expected: []string{"nats-route://127.0.0.1:6222"}},
		{description: "mixed", in: "nats-route://127.0.0.1:6222,unix:///run/b.sock", expected: []string{"nats-route://127.0.0.1:6222", "unix:///run/b.sock"}},
		// negative
		{description: "relative unix route", in: "unix://relative", expectedErr: "must be an absolute path"},
		{description: "first error wins but later entries still parse", in: "unix://x,unix:///run/b.sock", expected: []string{"unix:///run/b.sock"}, expectedErr: "must be an absolute path"},
		{description: "malformed tcp route", in: "nats-route://127.0.0.1:XXXX,unix:///run/b.sock", expected: []string{"unix:///run/b.sock"}, expectedErr: "error parsing route url"},
		// boundary
		{description: "empty string is one empty url, as before", in: "", expected: []string{""}},
		// corner
		{description: "spaces around commas are trimmed", in: " unix:///run/b.sock , unix://@c ", expected: []string{"unix:///run/b.sock", "unix://@c"}},
		{description: "trailing comma yields an empty url, as before", in: "unix:///run/b.sock,", expected: []string{"unix:///run/b.sock", ""}},
	} {
		t.Run(tc.description, func(t *testing.T) {
			urls, err := routesFromStr(tc.in)
			var got []string
			for _, u := range urls {
				got = append(got, u.String())
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.expected) {
				t.Errorf("routesFromStr(%q) = %v, expected %v", tc.in, got, tc.expected)
			}
			if tc.expectedErr == _EMPTY_ {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("expected error containing %q, got %v", tc.expectedErr, err)
			}
			// The exported wrapper returns the same URLs without the error.
			var wrapped []string
			for _, u := range RoutesFromStr(tc.in) {
				wrapped = append(wrapped, u.String())
			}
			if fmt.Sprint(wrapped) != fmt.Sprint(got) {
				t.Errorf("RoutesFromStr(%q) = %v, expected %v", tc.in, wrapped, got)
			}
		})
	}
}

// TestClusterListenEnabled covers the predicate that replaces Cluster.Port
// comparisons.
func TestClusterListenEnabled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		opts        ClusterOpts
		expected    bool
	}{
		// positive
		{description: "tcp port", opts: ClusterOpts{Port: 6222}, expected: true},
		{description: "unix socket", opts: ClusterOpts{UnixSocket: "/run/a.sock"}, expected: true},
		{description: "abstract socket", opts: ClusterOpts{UnixSocket: "@a"}, expected: true},
		{description: "random port sentinel", opts: ClusterOpts{Port: -1}, expected: true},
		// negative
		{description: "zero value", opts: ClusterOpts{}, expected: false},
		{description: "host alone does not enable", opts: ClusterOpts{Host: "127.0.0.1"}, expected: false},
		{description: "listen string alone does not enable", opts: ClusterOpts{ListenStr: "nats://127.0.0.1:6222"}, expected: false},
		{description: "advertise alone does not enable", opts: ClusterOpts{Advertise: "unix:///run/a.sock"}, expected: false},
		// corner
		{description: "both set is still enabled, validation rejects it elsewhere", opts: ClusterOpts{Port: 6222, UnixSocket: "/run/a.sock"}, expected: true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := tc.opts.listenEnabled(); got != tc.expected {
				t.Fatalf("listenEnabled() = %v, expected %v", got, tc.expected)
			}
		})
	}
}

// TestSetBaselineOptionsUnixSocket checks that cluster defaults apply to a
// unix socket listener without inventing a TCP host.
func TestSetBaselineOptionsUnixSocket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		cluster     ClusterOpts
		expected    ClusterOpts
	}{
		// positive
		{
			description: "unix listener gets pool size and timeouts but no host",
			cluster:     ClusterOpts{UnixSocket: "/run/a.sock"},
			expected:    ClusterOpts{UnixSocket: "/run/a.sock", Host: _EMPTY_, PoolSize: DEFAULT_ROUTE_POOL_SIZE},
		},
		{
			description: "tcp listener still defaults host",
			cluster:     ClusterOpts{Port: 6222},
			expected:    ClusterOpts{Port: 6222, Host: DEFAULT_HOST, PoolSize: DEFAULT_ROUTE_POOL_SIZE},
		},
		{
			description: "unix listen string from the flag also gets defaults",
			cluster:     ClusterOpts{ListenStr: "unix:///run/a.sock", UnixSocket: "/run/a.sock"},
			expected:    ClusterOpts{ListenStr: "unix:///run/a.sock", UnixSocket: "/run/a.sock", Host: _EMPTY_, PoolSize: DEFAULT_ROUTE_POOL_SIZE},
		},
		// negative
		{
			description: "no listener leaves cluster options alone",
			cluster:     ClusterOpts{},
			expected:    ClusterOpts{},
		},
		// corner
		{
			description: "explicit host with unix socket is left for validation to reject",
			cluster:     ClusterOpts{UnixSocket: "/run/a.sock", Host: "127.0.0.1"},
			expected:    ClusterOpts{UnixSocket: "/run/a.sock", Host: "127.0.0.1", PoolSize: DEFAULT_ROUTE_POOL_SIZE},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			opts := &Options{Cluster: tc.cluster, NoSystemAccount: true}
			setBaselineOptions(opts)
			got := opts.Cluster
			if got.Host != tc.expected.Host || got.Port != tc.expected.Port ||
				got.UnixSocket != tc.expected.UnixSocket || got.PoolSize != tc.expected.PoolSize ||
				got.ListenStr != tc.expected.ListenStr {
				t.Fatalf("got Host=%q Port=%d UnixSocket=%q PoolSize=%d ListenStr=%q, expected Host=%q Port=%d UnixSocket=%q PoolSize=%d ListenStr=%q",
					got.Host, got.Port, got.UnixSocket, got.PoolSize, got.ListenStr,
					tc.expected.Host, tc.expected.Port, tc.expected.UnixSocket, tc.expected.PoolSize, tc.expected.ListenStr)
			}
			if tc.expected.PoolSize != 0 && (got.TLSTimeout == 0 || got.AuthTimeout == 0) {
				t.Fatalf("expected timeouts to be defaulted, got TLSTimeout=%v AuthTimeout=%v", got.TLSTimeout, got.AuthTimeout)
			}
		})
	}
}

// TestValidateClusterUnixSocket covers validateCluster for options built in
// code, where the parse-time checks do not run.
func TestValidateClusterUnixSocket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		cluster     ClusterOpts
		expectedErr string
	}{
		// positive
		{description: "unix listener alone", cluster: ClusterOpts{UnixSocket: "/run/a.sock"}},
		{description: "unix listener with unix advertise", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix:///run/p/a.sock"}},
		{description: "unix listener with abstract advertise", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix://@p"}},
		{description: "tcp listener with tcp advertise", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1:6222"}},
		{description: "tcp listener with host-only advertise", cluster: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1"}},
		{description: "no listener ignores advertise", cluster: ClusterOpts{Advertise: "unix:///run/p/a.sock"}},
		// negative
		{description: "unix listener plus port", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Port: 6222}, expectedErr: "mutually exclusive"},
		{description: "unix listener plus host", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Host: "127.0.0.1"}, expectedErr: "mutually exclusive"},
		{description: "unix advertise with tcp listener", cluster: ClusterOpts{Port: 6222, Advertise: "unix:///run/a.sock"}, expectedErr: `advertise transport "unix" does not match listener transport "tcp"`},
		{description: "tcp advertise with unix listener", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "10.0.0.1:6222"}, expectedErr: `advertise transport "tcp" does not match listener transport "unix"`},
		{description: "invalid unix advertise", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix://run/a.sock"}, expectedErr: "must be an absolute path"},
		{description: "invalid unix advertise with tcp listener reports the address before the transport", cluster: ClusterOpts{Port: 6222, Advertise: "unix://x/"}, expectedErr: "must be an absolute path"},
		// corner
		{description: "error is prefixed with cluster", cluster: ClusterOpts{UnixSocket: "/run/a.sock", Port: 6222}, expectedErr: "cluster: unix socket listen"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			o := &Options{Cluster: tc.cluster}
			err := validateCluster(o)
			if tc.expectedErr == _EMPTY_ {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
				t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
			}
		})
	}
}

// TestValidateClusterOptsUnixSocket covers the reload rejection rules for
// unix socket listeners (design doc §6.11, rejection rows). Add and remove
// rows need a running server and live with the dial tests.
func TestValidateClusterOptsUnixSocket(t *testing.T) {
	t.Parallel()
	unixA := ClusterOpts{UnixSocket: "/run/a.sock"}
	tcpA := ClusterOpts{Host: "127.0.0.1", Port: 6222}
	for _, tc := range []struct {
		description string
		old, new    ClusterOpts
		expectedErr string
	}{
		// positive
		{description: "unchanged unix listener", old: unixA, new: unixA},
		{description: "unchanged tcp listener", old: tcpA, new: tcpA},
		{description: "advertise added between unix paths", old: unixA, new: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix:///run/p/a.sock"}},
		{description: "advertise changed between unix paths", old: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix:///run/p/a.sock"}, new: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix:///run/p/a2.sock"}},
		{description: "advertise removed", old: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix:///run/p/a.sock"}, new: unixA},
		{description: "tcp advertise on tcp listener still validated with parseHostPort", old: tcpA, new: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1:6222"}},
		// negative
		{description: "unix path changed", old: unixA, new: ClusterOpts{UnixSocket: "/run/b.sock"}, expectedErr: "config reload not supported for cluster unix socket"},
		{description: "unix to tcp", old: unixA, new: tcpA, expectedErr: "config reload not supported for cluster unix socket"},
		{description: "tcp to unix", old: tcpA, new: unixA, expectedErr: "config reload not supported for cluster unix socket"},
		{description: "pathname to abstract", old: unixA, new: ClusterOpts{UnixSocket: "@a"}, expectedErr: "config reload not supported for cluster unix socket"},
		{description: "tcp advertise on unix listener", old: unixA, new: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "10.0.0.1:6222"}, expectedErr: `advertise transport "tcp" does not match listener transport "unix"`},
		{description: "unix advertise on tcp listener", old: tcpA, new: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "unix:///run/a.sock"}, expectedErr: `advertise transport "unix" does not match listener transport "tcp"`},
		{description: "invalid unix advertise", old: unixA, new: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "unix://run/a.sock"}, expectedErr: "must be an absolute path"},
		{description: "invalid tcp advertise still rejected", old: tcpA, new: ClusterOpts{Host: "127.0.0.1", Port: 6222, Advertise: "10.0.0.1:XXXX"}, expectedErr: "invalid Cluster.Advertise"},
		// corner
		{description: "error names both paths", old: unixA, new: ClusterOpts{UnixSocket: "/run/b.sock"}, expectedErr: `old="/run/a.sock", new="/run/b.sock"`},
		{description: "advertise error is wrapped with the value", old: unixA, new: ClusterOpts{UnixSocket: "/run/a.sock", Advertise: "10.0.0.1:6222"}, expectedErr: "invalid Cluster.Advertise value of 10.0.0.1:6222"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			err := validateClusterOpts(tc.old, tc.new)
			if tc.expectedErr == _EMPTY_ {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
				t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
			}
		})
	}
}

// TestClusterPortPredicateSites guards the "clustering enabled" predicate:
// non-test code must ask ClusterOpts.listenEnabled() rather than compare
// Cluster.Port with zero, otherwise a unix socket listener would be treated
// as "not clustered" at that site.
func TestClusterPortPredicateSites(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`Cluster\.Port\s*(==|!=|>|<|>=|<=)\s*0\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("Cluster.Port compared with zero; use ClusterOpts.listenEnabled() instead:\n%s", strings.Join(hits, "\n"))
	}
}
