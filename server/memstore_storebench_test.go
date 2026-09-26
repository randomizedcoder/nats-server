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
	"testing"
)

// Benchmark_MemStoreStoreMsg measures the per-message cost of storing into a
// memory-backed stream across a payload sweep. MaxMsgs caps residency so large
// payloads don't grow unbounded over b.N. Reports ns/op, B/op, allocs/op --
// the metrics that show the storeRawMsg copy/alloc behaviour directly.
//
// The sweep spans two-orders-of-magnitude so the fixed per-message overhead
// (small payloads) and the copy/alloc-dominated regime (large payloads) are
// both visible: the double-copy removal is a wash at 64B and ~2x at >=4KiB.
func Benchmark_MemStoreStoreMsg(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536, 131072, 262144, 524288, 983040}
	for _, sz := range sizes {
		b.Run(fmt.Sprintf("payload=%d", sz), func(b *testing.B) {
			benchMemStoreStore(b, 0, sz)
		})
	}
}

// Benchmark_MemStoreStoreMsgHdr exercises the header path as well as the body,
// since storeRawMsg previously copied BOTH hdr and msg an extra time. A fixed
// 512B header is paired with a body sweep.
func Benchmark_MemStoreStoreMsgHdr(b *testing.B) {
	sizes := []int{64, 4096, 65536, 262144, 983040}
	for _, sz := range sizes {
		b.Run(fmt.Sprintf("payload=%d", sz), func(b *testing.B) {
			benchMemStoreStore(b, 512, sz)
		})
	}
}

func benchMemStoreStore(b *testing.B, hdrSz, msgSz int) {
	cfg := &StreamConfig{
		Name:    "bench",
		Storage: MemoryStorage,
		MaxMsgs: 1024, // cap residency; keeps the store bounded
	}
	ms, err := newMemStore(cfg)
	require_NoError(b, err)
	defer ms.Stop()

	var hdr []byte
	if hdrSz > 0 {
		hdr = make([]byte, hdrSz)
		for i := range hdr {
			hdr[i] = byte(i)
		}
	}
	msg := make([]byte, msgSz)
	for i := range msg {
		msg[i] = byte(i)
	}

	b.SetBytes(int64(hdrSz + msgSz))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := ms.StoreMsg("bench", hdr, msg, 0); err != nil {
			b.Fatal(err)
		}
	}
}
