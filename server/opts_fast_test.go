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
	"strings"
	"testing"
)

// TestFastEndpointConfigParse covers parsing the cluster fast_endpoint key and
// the three-way listener-transport exclusion (design 58 P2a). Rows are
// POS/NEG/BND/COR; each names the config and the expected FastEndpoint (on
// success) or a substring of the parse error (on failure).
func TestFastEndpointConfigParse(t *testing.T) {
	// A 15-byte name is exactly the wire cap; a 16-byte one is one over.
	const name15 = "abcdefghijklmno"  // 15 bytes -> ok
	const name16 = "abcdefghijklmnop" // 16 bytes -> rejected

	for _, tc := range []struct {
		description  string
		conf         string
		expectedName string
		expectedErr  string
	}{
		// positive
		{
			description:  "fast endpoint alone parses",
			conf:         "cluster { name: c1, fast_endpoint: \"nats-a\" }\n",
			expectedName: "nats-a",
		},
		{
			description:  "off leaves FastEndpoint empty (byte-identical to upstream)",
			conf:         "cluster { name: c1, listen: 127.0.0.1:-1 }\n",
			expectedName: _EMPTY_,
		},
		{
			description:  "no cluster block leaves FastEndpoint empty",
			conf:         "listen: 127.0.0.1:-1\n",
			expectedName: _EMPTY_,
		},
		// boundary
		{
			description:  "name at the 15-byte cap is accepted",
			conf:         "cluster { name: c1, fast_endpoint: \"" + name15 + "\" }\n",
			expectedName: name15,
		},
		{
			description: "name one byte over the cap is rejected",
			conf:        "cluster { name: c1, fast_endpoint: \"" + name16 + "\" }\n",
			expectedErr: "exceeds the 15-byte urp endpoint-name cap",
		},
		// negative
		{
			description: "empty name is rejected",
			conf:        "cluster { name: c1, fast_endpoint: \"\" }\n",
			expectedErr: "fast_endpoint must not be empty",
		},
		{
			description: "fast endpoint and host/port are mutually exclusive",
			conf:        "cluster { name: c1, listen: 127.0.0.1:-1, fast_endpoint: \"nats-a\" }\n",
			expectedErr: "fast endpoint and host/port are mutually exclusive",
		},
		{
			description: "fast endpoint and unix socket are mutually exclusive",
			conf:        "cluster { name: c1, listen: \"unix:///run/nats/a.sock\", fast_endpoint: \"nats-a\" }\n",
			expectedErr: "fast endpoint and unix socket listen are mutually exclusive",
		},
		// corner
		{
			description: "non-string fast_endpoint is rejected",
			conf:        "cluster { name: c1, fast_endpoint: 42 }\n",
			expectedErr: "fast_endpoint must be a string",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			opts, err := ProcessConfigFile(createConfFile(t, []byte(tc.conf)))
			if tc.expectedErr != _EMPTY_ {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected parse error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if opts.Cluster.FastEndpoint != tc.expectedName {
				t.Fatalf("FastEndpoint = %q, expected %q", opts.Cluster.FastEndpoint, tc.expectedName)
			}
		})
	}
}

// TestFastEndpointListenEnabled asserts a fast endpoint enables clustering the
// same way a TCP port or a unix socket does.
func TestFastEndpointListenEnabled(t *testing.T) {
	for _, tc := range []struct {
		description string
		cluster     ClusterOpts
		expected    bool
	}{
		{description: "nothing configured", cluster: ClusterOpts{}, expected: false},
		{description: "tcp port enables", cluster: ClusterOpts{Port: 6222}, expected: true},
		{description: "unix socket enables", cluster: ClusterOpts{UnixSocket: "/run/nats/a.sock"}, expected: true},
		{description: "fast endpoint enables", cluster: ClusterOpts{FastEndpoint: "nats-a"}, expected: true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := tc.cluster.listenEnabled(); got != tc.expected {
				t.Fatalf("listenEnabled() = %v, expected %v", got, tc.expected)
			}
		})
	}
}

// TestValidateFastEndpointName covers the wire-cap / NUL / empty validation of
// the urp endpoint name in isolation.
func TestValidateFastEndpointName(t *testing.T) {
	for _, tc := range []struct {
		description string
		name        string
		expectedErr string
	}{
		// positive
		{description: "short name", name: "a"},
		{description: "name at the 15-byte cap", name: "abcdefghijklmno"},
		// boundary
		{description: "name one over the cap", name: "abcdefghijklmnop", expectedErr: "exceeds the 15-byte"},
		// negative
		{description: "empty", name: "", expectedErr: "must not be empty"},
		{description: "embedded NUL", name: "a\x00b", expectedErr: "must not contain a NUL byte"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			err := validateFastEndpointName(tc.name)
			if tc.expectedErr != _EMPTY_ {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestSetBaselineOptionsFastEndpoint is the regression for the bug where a
// fast-endpoint-only cluster (no host/port, no unix socket) survived the
// parse-time check but then had Cluster.Host defaulted to DEFAULT_HOST by
// setBaselineOptions -- which made the later validateClusterListenTransport
// reject it as "fast endpoint and host/port are mutually exclusive" at startup
// (config parsed with `-t` OK, real server exited 1). A fast listener, like a
// unix listener, has no host to invent. Mirrors TestSetBaselineOptionsUnixSocket.
func TestSetBaselineOptionsFastEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		cluster     ClusterOpts
		expected    ClusterOpts
	}{
		// positive: the bug -- fast endpoint alone must NOT get a defaulted host,
		// and defaults to no-pool (PoolSize -1) because it is a single
		// point-to-point connection that cannot back a route pool or a dedicated
		// per-account route.
		{
			description: "fast listener defaults to no-pool and gets no host",
			cluster:     ClusterOpts{FastEndpoint: "na_ca"},
			expected:    ClusterOpts{FastEndpoint: "na_ca", Host: _EMPTY_, PoolSize: -1},
		},
		// negative: a tcp listener still defaults its host (unchanged behaviour).
		{
			description: "tcp listener still defaults host",
			cluster:     ClusterOpts{Port: 6222},
			expected:    ClusterOpts{Port: 6222, Host: DEFAULT_HOST, PoolSize: DEFAULT_ROUTE_POOL_SIZE},
		},
		// boundary: no listener at all leaves the cluster options untouched.
		{
			description: "no listener leaves cluster options alone",
			cluster:     ClusterOpts{},
			expected:    ClusterOpts{},
		},
		// corner: an explicit host alongside a fast endpoint is left as-is for
		// validation to reject (baseline must not mask the conflict).
		{
			description: "explicit host with fast endpoint is left for validation to reject",
			cluster:     ClusterOpts{FastEndpoint: "na_ca", Host: "127.0.0.1"},
			expected:    ClusterOpts{FastEndpoint: "na_ca", Host: "127.0.0.1", PoolSize: -1},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			opts := &Options{Cluster: tc.cluster, NoSystemAccount: true}
			setBaselineOptions(opts)
			got := opts.Cluster
			if got.Host != tc.expected.Host || got.Port != tc.expected.Port ||
				got.FastEndpoint != tc.expected.FastEndpoint || got.PoolSize != tc.expected.PoolSize {
				t.Fatalf("got Host=%q Port=%d FastEndpoint=%q PoolSize=%d, expected Host=%q Port=%d FastEndpoint=%q PoolSize=%d",
					got.Host, got.Port, got.FastEndpoint, got.PoolSize,
					tc.expected.Host, tc.expected.Port, tc.expected.FastEndpoint, tc.expected.PoolSize)
			}
			// A fast-endpoint-only cluster must also pass validation after
			// baselining -- this is the exact path that failed at startup.
			if tc.expected.FastEndpoint != _EMPTY_ && tc.expected.Host == _EMPTY_ {
				if err := validateClusterListenTransport(&got); err != nil {
					t.Fatalf("fast endpoint alone failed validation after baselining: %v", err)
				}
			}
		})
	}
}
