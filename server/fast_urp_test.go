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

//go:build linux

package server

import (
	"testing"

	"github.com/randomizedcoder/uds-rdma-proxy/tools/urp-fast-go/fastcore"
)

// classifyURPCompletion is the pure decision the ring goroutine makes for each
// reaped completion; these tables lock its four outcomes across POS/NEG/BND/COR.
// bufSize 64 KiB => usable = 65536 - HeaderResv.
func TestClassifyURPCompletion(t *testing.T) {
	const bufSize uint32 = 65536
	usable := bufSize - fastcore.HeaderResv // largest acceptable RECV length

	for _, tc := range []struct {
		description string
		kind        uint8
		res         int32
		expectAct   urpAction
		expectLen   uint32
	}{
		{"POS send success frees the buffer", fastcore.UDKindSend, 0, actFreeSend, 0},
		{"POS send with negative status still frees", fastcore.UDKindSend, -5, actFreeSend, 0},
		{"POS recv delivers landed payload", fastcore.UDKindRecv, 100, actDeliverRecv, 100},
		{"BND recv exactly fills usable region", fastcore.UDKindRecv, int32(usable), actDeliverRecv, usable},
		{"NEG recv one byte over usable is overflow", fastcore.UDKindRecv, int32(usable) + 1, actRecvError, 0},
		{"NEG recv negative res is a kernel errno", fastcore.UDKindRecv, -int32(104), actRecvError, 0},
		{"BND recv zero-length payload is valid", fastcore.UDKindRecv, 0, actDeliverRecv, 0},
		{"COR unknown user_data kind is ignored", 9, 0, actIgnore, 0},
	} {
		t.Run(tc.description, func(t *testing.T) {
			act, length := classifyURPCompletion(tc.kind, tc.res, bufSize)
			if act != tc.expectAct {
				t.Fatalf("%s: act=%d, want %d", tc.description, act, tc.expectAct)
			}
			if length != tc.expectLen {
				t.Fatalf("%s: length=%d, want %d", tc.description, length, tc.expectLen)
			}
		})
	}
}

// urpRXCount splits the pool into an RX half and a TX half; both halves must be
// non-empty for the endpoint to both receive and send.
func TestURPRXCount(t *testing.T) {
	for _, tc := range []struct {
		description string
		count       uint32
		expect      uint32
	}{
		{"POS even pool splits in half", 256, 128},
		{"POS large pool splits in half", 4096, 2048},
		{"BND minimum viable pool (4) -> 2 RX", 4, 2},
		{"BND odd pool floors RX", 5, 2},
		{"BND count 3 keeps a TX buffer", 3, 1},
		{"BND count 2 -> 1 RX 1 TX", 2, 1},
	} {
		t.Run(tc.description, func(t *testing.T) {
			got := urpRXCount(tc.count)
			if got != tc.expect {
				t.Fatalf("%s: urpRXCount(%d)=%d, want %d", tc.description, tc.count, got, tc.expect)
			}
			if got == 0 || got >= tc.count {
				t.Fatalf("%s: split %d/%d leaves an empty half", tc.description, got, tc.count)
			}
		})
	}
}

// envUint32 reads a positive geometry override, falling back to the default on
// any absent/malformed/non-positive value.
func TestEnvUint32(t *testing.T) {
	const key = "URP_NATS_TEST_KNOB"
	const def uint32 = 256

	for _, tc := range []struct {
		description string
		set         bool
		val         string
		expect      uint32
	}{
		{"POS valid override", true, "512", 512},
		{"POS override of 1", true, "1", 1},
		{"NEG unset falls back to default", false, "", def},
		{"NEG empty string falls back", true, "", def},
		{"NEG non-numeric falls back", true, "abc", def},
		{"NEG zero is rejected (must be positive)", true, "0", def},
		{"NEG negative is rejected", true, "-5", def},
		{"BND over-uint32 falls back", true, "4294967296", def},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.val)
			} else {
				// Ensure it is not inherited from the environment.
				t.Setenv(key, "")
				_ = tc.val
			}
			if got := envUint32(key, def); got != tc.expect {
				t.Fatalf("%s: envUint32=%d, want %d", tc.description, got, tc.expect)
			}
		})
	}
}
