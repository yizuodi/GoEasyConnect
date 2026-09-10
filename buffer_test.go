package main

import (
	"strings"
	"testing"
)

func TestByteRingIsBounded(t *testing.T) {
	t.Parallel()
	ring := byteRing{max: 8}
	ring.Append([]byte("12345"))
	ring.Append([]byte("67890"))
	if got := string(ring.Bytes()); got != "34567890" {
		t.Fatalf("ring content = %q", got)
	}
	ring.Append([]byte("abcdefghijk"))
	if got := string(ring.Bytes()); got != "defghijk" {
		t.Fatalf("oversized append = %q", got)
	}
}

func TestPollBufferIsBoundedAndSequenceAware(t *testing.T) {
	t.Parallel()
	buffer := pollBuffer{maxBytes: 7, maxItems: 3}
	buffer.Append([]byte("one"))
	buffer.Append([]byte("two"))
	buffer.Append([]byte("three"))
	output, seq := buffer.Since(0)
	if seq != 3 || string(output) != "three" {
		t.Fatalf("output=%q seq=%d", output, seq)
	}
	buffer.Append([]byte("x"))
	output, seq = buffer.Since(3)
	if seq != 4 || !strings.Contains(string(output), "x") {
		t.Fatalf("incremental output=%q seq=%d", output, seq)
	}
	buffer.Append([]byte("0123456789"))
	output, seq = buffer.Since(0)
	if seq != 5 || string(output) != "3456789" {
		t.Fatalf("oversized chunk output=%q seq=%d", output, seq)
	}
}
