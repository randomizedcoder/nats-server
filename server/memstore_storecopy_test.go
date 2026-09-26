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
	"testing"
)

// TestMemStoreStoreMsgPrivateCopy verifies that storeRawMsg takes a private
// copy of the caller's hdr/msg: after StoreMsg returns, mutating (or reusing)
// the caller's input buffers must not change the stored message. This is the
// invariant that lets storeRawMsg append the caller's slices directly into
// sm.buf instead of pre-copying them -- the append IS the private copy. If a
// future change reintroduced an alias to the caller's buffer, these rows fail.
func TestMemStoreStoreMsgPrivateCopy(t *testing.T) {
	fill := func(n int, base byte) []byte {
		if n == 0 {
			return nil
		}
		b := make([]byte, n)
		for i := range b {
			b[i] = base + byte(i)
		}
		return b
	}

	tests := []struct {
		description string
		hdr         []byte
		msg         []byte
		expected    string // documents the invariant being asserted
	}{
		{"body only, no header", nil, fill(256, 1), "stored msg unchanged after caller mutates msg"},
		{"header and body", fill(64, 100), fill(256, 1), "stored hdr+msg unchanged after caller mutates both"},
		{"empty body, header only", fill(64, 100), nil, "stored hdr unchanged; empty msg stays empty"},
		{"single-byte body", nil, fill(1, 7), "boundary: 1-byte body copied privately"},
		{"large body (960 KiB)", nil, fill(983040, 3), "large payload copied privately, no alias"},
		{"header + empty body edge", fill(1, 9), fill(0, 0), "boundary: 1-byte hdr, zero-len body"},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			ms, err := newMemStore(&StreamConfig{Name: "priv", Storage: MemoryStorage})
			require_NoError(t, err)
			defer ms.Stop()

			// Snapshot the intended stored bytes before the store call.
			wantHdr := append([]byte(nil), tt.hdr...)
			wantMsg := append([]byte(nil), tt.msg...)

			seq, _, err := ms.StoreMsg("priv", tt.hdr, tt.msg, 0)
			require_NoError(t, err)

			// Hostile caller: scribble over the input buffers after storing.
			// If the store aliased them, the stored message would change.
			for i := range tt.hdr {
				tt.hdr[i] = 0xEE
			}
			for i := range tt.msg {
				tt.msg[i] = 0xEE
			}

			var smv StoreMsg
			sm, err := ms.LoadMsg(seq, &smv)
			require_NoError(t, err)

			if !bytes.Equal(sm.hdr, wantHdr) {
				t.Fatalf("%s: stored hdr changed after caller mutation\n got=%x\nwant=%x",
					tt.expected, sm.hdr, wantHdr)
			}
			if !bytes.Equal(sm.msg, wantMsg) {
				t.Fatalf("%s: stored msg changed after caller mutation (len got=%d want=%d)",
					tt.expected, len(sm.msg), len(wantMsg))
			}
		})
	}
}
