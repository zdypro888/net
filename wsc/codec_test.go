package wsc

import "testing"

type emptyNameCodec struct{}

func (emptyNameCodec) Name() string { return "" }

func (emptyNameCodec) Encode(v any) (int, []byte, error) {
	return JSONCodec{}.Encode(v)
}

func (emptyNameCodec) Decode(messageType int, data []byte, v any) error {
	return JSONCodec{}.Decode(messageType, data, v)
}

func TestNewCodecSetIgnoresEmptyNames(t *testing.T) {
	cs := newCodecSet([]Codec{emptyNameCodec{}})
	names := cs.names()
	if len(names) != 1 || names[0] != CodecJSON {
		t.Fatalf("names = %#v, want only %q", names, CodecJSON)
	}
	if _, ok := cs.get(""); ok {
		t.Fatalf("empty codec name must not be registered")
	}
}
