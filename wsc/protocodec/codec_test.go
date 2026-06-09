package protocodec

import (
	"testing"

	"github.com/gorilla/websocket"
	"github.com/zdypro888/net/wsc"
	"google.golang.org/protobuf/proto"
)

func TestCodecRoundTripEmptyPayload(t *testing.T) {
	c := New()
	in := &wsc.Message[*Frame]{
		ID:   "request-id",
		Data: &Frame{},
	}

	messageType, data, err := c.Encode(in)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("messageType = %d, want binary", messageType)
	}

	var wire Frame
	if err := proto.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire unmarshal failed: %v", err)
	}
	if !wire.HasData {
		t.Fatalf("empty proto payload must be marked present")
	}
	if len(wire.Data) != 0 {
		t.Fatalf("empty proto payload encoded data length = %d, want 0", len(wire.Data))
	}

	var out wsc.Message[*Frame]
	if err := c.Decode(messageType, data, &out); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if out.ID != in.ID {
		t.Fatalf("ID = %q, want %q", out.ID, in.ID)
	}
	if out.Data == nil {
		t.Fatalf("empty proto payload was dropped")
	}
}

func TestCodecRoundTripNilPayload(t *testing.T) {
	c := New()
	in := &wsc.Message[*Frame]{ID: "nil-payload"}

	messageType, data, err := c.Encode(in)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var wire Frame
	if err := proto.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire unmarshal failed: %v", err)
	}
	if wire.HasData {
		t.Fatalf("nil payload must not be marked present")
	}

	var out wsc.Message[*Frame]
	if err := c.Decode(messageType, data, &out); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if out.ID != in.ID {
		t.Fatalf("ID = %q, want %q", out.ID, in.ID)
	}
	if out.Data != nil {
		t.Fatalf("nil proto payload decoded as %#v", out.Data)
	}
}

func TestCodecDecodeLegacyFrameWithData(t *testing.T) {
	payloadData, err := proto.Marshal(&Frame{Id: "payload"})
	if err != nil {
		t.Fatalf("payload marshal failed: %v", err)
	}
	wireData, err := proto.Marshal(&Frame{Id: "request-id", Data: payloadData})
	if err != nil {
		t.Fatalf("wire marshal failed: %v", err)
	}

	var out wsc.Message[*Frame]
	if err := New().Decode(websocket.BinaryMessage, wireData, &out); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if out.ID != "request-id" {
		t.Fatalf("ID = %q, want request-id", out.ID)
	}
	if out.Data == nil {
		t.Fatalf("legacy proto payload was dropped")
	}
	if out.Data.Id != "payload" {
		t.Fatalf("payload ID = %q, want payload", out.Data.Id)
	}
}
