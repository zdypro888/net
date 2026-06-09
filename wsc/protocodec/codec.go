// Package protocodec 为 wsc 提供基于 Protocol Buffers 的二进制 codec, 作为默认 JSON
// codec 的高效替代。通过 wsc.WithCodecs(protocodec.New()) 注入, 并由握手协商启用。
//
// 使用约束: 与该 codec 搭配的 wsc.Client[T]/wsc.Server[T] 的载荷类型 T 必须是指向
// 生成的 proto.Message 的指针 (例如 *pb.Foo); 否则 Encode/Decode 返回错误。信封元数据
// (id/heart) 由本包的 Frame 承载, 载荷字节由 proto.Marshal(T) 产生。
package protocodec

import (
	"fmt"
	"reflect"

	"github.com/gorilla/websocket"
	"github.com/zdypro888/net/wsc"
	"google.golang.org/protobuf/proto"
)

// Name 是该 codec 在握手协商中的 wire 标识符。
const Name = "proto"

type codec struct{}

// New 返回一个使用 Protocol Buffers 序列化的 wsc.Codec。
func New() wsc.Codec { return codec{} }

// Name 实现 wsc.Codec。
func (codec) Name() string { return Name }

// Encode 实现 wsc.Codec: 把信封编为 Frame 并以二进制帧发送。心跳帧不带载荷。
func (codec) Encode(v any) (int, []byte, error) {
	env, ok := v.(wsc.Envelope)
	if !ok {
		return 0, nil, fmt.Errorf("protocodec: %T does not implement wsc.Envelope", v)
	}
	frame := &Frame{Id: env.EnvelopeID(), Heart: env.EnvelopeHeart()}
	if !frame.Heart {
		if payload := env.EnvelopePayload(); !isNil(payload) {
			pm, ok := payload.(proto.Message)
			if !ok {
				return 0, nil, fmt.Errorf("protocodec: payload %T does not implement proto.Message", payload)
			}
			data, err := proto.Marshal(pm)
			if err != nil {
				return 0, nil, err
			}
			frame.Data = data
			frame.HasData = true
		}
	}
	data, err := proto.Marshal(frame)
	if err != nil {
		return 0, nil, err
	}
	return websocket.BinaryMessage, data, nil
}

// Decode 实现 wsc.Codec: 解析二进制 Frame 并把载荷反序列化进信封。
func (codec) Decode(messageType int, data []byte, v any) error {
	if messageType != websocket.BinaryMessage {
		return fmt.Errorf("protocodec: expected binary message, got type %d", messageType)
	}
	env, ok := v.(wsc.Envelope)
	if !ok {
		return fmt.Errorf("protocodec: %T does not implement wsc.Envelope", v)
	}
	var frame Frame
	if err := proto.Unmarshal(data, &frame); err != nil {
		return err
	}
	env.SetEnvelopeID(frame.Id)
	env.SetEnvelopeHeart(frame.Heart)
	if frame.HasData || len(frame.Data) > 0 {
		msg, err := newProtoPayload(env.EnvelopePayload())
		if err != nil {
			return err
		}
		if err := proto.Unmarshal(frame.Data, msg); err != nil {
			return err
		}
		if err := env.SetEnvelopePayload(msg); err != nil {
			return err
		}
	}
	return nil
}

// newProtoPayload 按 (可能为 nil 的) 模板载荷的具体类型分配一个新的 proto.Message,
// 供解码时反序列化进去。模板取自零值 Message 的 Data 字段 (指针类型为 typed-nil),
// 由此拿到具体类型再分配同类型实例。
func newProtoPayload(template any) (proto.Message, error) {
	rt := reflect.TypeOf(template)
	if rt == nil || rt.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("protocodec: payload type %T is not a pointer to a proto.Message", template)
	}
	pm, ok := reflect.New(rt.Elem()).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("protocodec: payload type %T does not implement proto.Message", template)
	}
	return pm, nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
