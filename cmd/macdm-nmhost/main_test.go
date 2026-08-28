package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestMessageFraming(t *testing.T) {
	payload := []byte(`{"type":"ping"}`)

	var framed bytes.Buffer
	if err := writeMessage(&framed, payload); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	// 4-byte little-endian length prefix
	if got := binary.LittleEndian.Uint32(framed.Bytes()[:4]); got != uint32(len(payload)) {
		t.Fatalf("length prefix = %d, want %d", got, len(payload))
	}

	round, err := readMessage(&framed)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if !bytes.Equal(round, payload) {
		t.Fatalf("round trip mismatch: %q vs %q", round, payload)
	}
}

func TestReadMessageRejectsHugeLength(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(200*1024*1024))
	if _, err := readMessage(&buf); err == nil {
		t.Fatal("expected error for oversized message length")
	}
}

func TestInboundDecode(t *testing.T) {
	raw := `{"type":"download","url":"https://x/v.mp4","headers":{"Referer":"https://x/"},"conns":4}`
	var in inbound
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.Type != "download" || in.URL != "https://x/v.mp4" || in.Headers["Referer"] != "https://x/" || in.Conns != 4 {
		t.Fatalf("bad decode: %+v", in)
	}
}
