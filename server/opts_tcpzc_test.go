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

// TestTCPZCConfigParse covers parsing the cluster tcpzc key and its relation to
// the other listener transports (design 58 §9a): tcpzc is a local io_uring
// drive-mode over an ordinary TCP route listener, so it REQUIRES host/port and
// is mutually exclusive with the unix and fast endpoint transports. Rows are
// POS/NEG/BND/COR; each names the config and the expected TCPZC bool (on
// success) or a substring of the parse error (on failure).
func TestTCPZCConfigParse(t *testing.T) {
	for _, tc := range []struct {
		description   string
		conf          string
		expectedTCPZC bool
		expectedErr   string
	}{
		// positive
		{
			description:   "tcpzc on with a host/port listener parses",
			conf:          "cluster { name: c1, listen: 127.0.0.1:-1, tcpzc: true }\n",
			expectedTCPZC: true,
		},
		{
			description:   "tcpzc explicitly off with a listener leaves it false",
			conf:          "cluster { name: c1, listen: 127.0.0.1:-1, tcpzc: false }\n",
			expectedTCPZC: false,
		},
		{
			description:   "tcpzc omitted leaves it false (byte-identical to upstream)",
			conf:          "cluster { name: c1, listen: 127.0.0.1:-1 }\n",
			expectedTCPZC: false,
		},
		{
			description:   "no cluster block leaves tcpzc false",
			conf:          "listen: 127.0.0.1:-1\n",
			expectedTCPZC: false,
		},
		// boundary
		{
			description:   "tcpzc on with an explicit fixed port parses",
			conf:          "cluster { name: c1, port: 6222, tcpzc: true }\n",
			expectedTCPZC: true,
		},
		{
			description:   "tcpzc off with no listener at all is fine (clustering simply disabled)",
			conf:          "cluster { name: c1, tcpzc: false }\n",
			expectedTCPZC: false,
		},
		// negative
		{
			description: "tcpzc on without any host/port listener is rejected",
			conf:        "cluster { name: c1, tcpzc: true }\n",
			expectedErr: "tcpzc requires a host/port route listener",
		},
		{
			description: "tcpzc and unix socket are mutually exclusive",
			conf:        "cluster { name: c1, listen: \"unix:///run/nats/a.sock\", tcpzc: true }\n",
			expectedErr: "tcpzc and unix socket listen are mutually exclusive",
		},
		{
			description: "tcpzc and fast endpoint are mutually exclusive",
			conf:        "cluster { name: c1, fast_endpoint: \"nats-a\", tcpzc: true }\n",
			expectedErr: "tcpzc and fast endpoint are mutually exclusive",
		},
		// corner
		{
			description: "non-boolean tcpzc is rejected",
			conf:        "cluster { name: c1, listen: 127.0.0.1:-1, tcpzc: \"yes\" }\n",
			expectedErr: "tcpzc must be a boolean",
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
			if opts.Cluster.TCPZC != tc.expectedTCPZC {
				t.Fatalf("TCPZC = %v, expected %v", opts.Cluster.TCPZC, tc.expectedTCPZC)
			}
		})
	}
}
