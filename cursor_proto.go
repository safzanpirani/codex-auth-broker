package main

import (
	"bytes"
	"encoding/binary"
	"unicode/utf8"
)

type connectCodec string

const (
	connectJSON  connectCodec = "json"
	connectProto connectCodec = "proto"
)

type connectFrame struct {
	flags byte
	data  []byte
}

type protoField struct {
	no     int
	wire   int
	bytes  []byte
	varint uint64
}

func cursorCodec(contentType string) connectCodec {
	if stringsContains(contentType, "json") {
		return connectJSON
	}
	return connectProto
}

func connectStreamContentType(codec connectCodec) string {
	if codec == connectJSON {
		return "application/connect+json"
	}
	return "application/connect+proto"
}

func connectUnaryContentType(codec connectCodec) string {
	if codec == connectJSON {
		return "application/json"
	}
	return "application/proto"
}

func encodeConnectFrame(data []byte, flags byte) []byte {
	out := make([]byte, 5+len(data))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(data)))
	copy(out[5:], data)
	return out
}

func decodeConnectFrames(body []byte) []connectFrame {
	var out []connectFrame
	for offset := 0; offset+5 <= len(body); {
		length := int(binary.BigEndian.Uint32(body[offset+1 : offset+5]))
		flags := body[offset]
		offset += 5
		if offset+length > len(body) {
			return nil
		}
		out = append(out, connectFrame{flags: flags, data: body[offset : offset+length]})
		offset += length
	}
	return out
}

func pbVarint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n&0x7f)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

func pbTag(no int, wire int) []byte {
	return pbVarint(uint64(no<<3 | wire))
}

func pbString(no int, value string) []byte {
	data := []byte(value)
	out := append([]byte{}, pbTag(no, 2)...)
	out = append(out, pbVarint(uint64(len(data)))...)
	out = append(out, data...)
	return out
}

func pbInt(no int, value uint64) []byte {
	out := append([]byte{}, pbTag(no, 0)...)
	out = append(out, pbVarint(value)...)
	return out
}

func pbMessage(no int, body []byte) []byte {
	out := append([]byte{}, pbTag(no, 2)...)
	out = append(out, pbVarint(uint64(len(body)))...)
	out = append(out, body...)
	return out
}

func pbConcat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func pbFields(data []byte) []protoField {
	var out []protoField
	for i := 0; i < len(data); {
		key, n, ok := readPBVarint(data[i:])
		if !ok {
			return out
		}
		i += n
		field := protoField{no: int(key >> 3), wire: int(key & 7)}
		switch field.wire {
		case 0:
			value, m, ok := readPBVarint(data[i:])
			if !ok {
				return out
			}
			field.varint = value
			i += m
		case 2:
			length, m, ok := readPBVarint(data[i:])
			if !ok || i+m+int(length) > len(data) {
				return out
			}
			i += m
			field.bytes = data[i : i+int(length)]
			i += int(length)
		case 1:
			if i+8 > len(data) {
				return out
			}
			field.bytes = data[i : i+8]
			i += 8
		case 5:
			if i+4 > len(data) {
				return out
			}
			field.bytes = data[i : i+4]
			i += 4
		default:
			return out
		}
		out = append(out, field)
	}
	return out
}

func readPBVarint(data []byte) (uint64, int, bool) {
	var value uint64
	for i, b := range data {
		if i == 10 {
			return 0, 0, false
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1, true
		}
	}
	return 0, 0, false
}

func pbBytes(data []byte, no int) [][]byte {
	var out [][]byte
	for _, field := range pbFields(data) {
		if field.no == no && field.wire == 2 {
			out = append(out, field.bytes)
		}
	}
	return out
}

func pbStringField(data []byte, no int) string {
	values := pbBytes(data, no)
	if len(values) == 0 || !utf8.Valid(values[0]) {
		return ""
	}
	return string(values[0])
}

func pbVarintField(data []byte, no int) (uint64, bool) {
	for _, field := range pbFields(data) {
		if field.no == no && field.wire == 0 {
			return field.varint, true
		}
	}
	return 0, false
}

func protoStrings(data []byte, depth int) []string {
	if depth > 8 {
		return nil
	}
	var out []string
	for _, field := range pbFields(data) {
		if field.wire != 2 || len(field.bytes) == 0 {
			continue
		}
		if utf8.Valid(field.bytes) {
			text := stringsTrimSpace(string(field.bytes))
			if len(text) > 1 && len(text) < 4000 && hasPromptChar(text) && isProtoTextCandidate(text) {
				out = append(out, text)
			}
		}
		out = append(out, protoStrings(field.bytes, depth+1)...)
	}
	return out
}

func isProtoTextCandidate(text string) bool {
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 {
			return false
		}
	}
	return true
}
