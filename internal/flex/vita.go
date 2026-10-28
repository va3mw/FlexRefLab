package flex

import (
	"encoding/binary"
	"errors"
	"math"
)

// VITA-49 packets the radio sends to our UDP port: DAX audio and meters.

const (
	flexOUI = 0x001C2D

	// DAX audio classes. Full bandwidth is big-endian float32 stereo; reduced
	// bandwidth is big-endian int16 mono.
	classFloat24k  = 0x03E3
	classFloat48k  = 0x03E4
	classFloat96k  = 0x03E5
	classInt16_24k = 0x0123
	classInt16_48k = 0x0124
	classInt16_96k = 0x0125

	classMeter = 0x8002
)

// Packet is a decoded VITA-49 packet.
type Packet struct {
	Type        uint8
	Count       uint8 // 4-bit packet count, to spot lost packets
	StreamID    uint32
	OUI         uint32
	PacketClass uint16
	TSInt       uint32 // integer timestamp (seconds), if present
	TSFrac      uint64 // fractional timestamp, if present
	HasTS       bool
	Payload     []byte
}

// ParseVITA decodes the preamble and returns the payload slice.
func ParseVITA(b []byte) (Packet, error) {
	var p Packet
	if len(b) < 4 {
		return p, errors.New("short packet")
	}
	be := binary.BigEndian
	h := be.Uint32(b)
	p.Type = uint8(h >> 28)
	p.Count = uint8(h>>16) & 0x0F
	size := int(h&0xFFFF) * 4
	if size > len(b) || size < 4 {
		size = len(b)
	}
	i := 4
	need := func(n int) bool { return i+n <= size }
	if p.Type == 1 || p.Type == 3 {
		if !need(4) {
			return p, errors.New("truncated stream id")
		}
		p.StreamID = be.Uint32(b[i:])
		i += 4
	}
	if h&(1<<27) != 0 {
		if !need(8) {
			return p, errors.New("truncated class id")
		}
		p.OUI = be.Uint32(b[i:]) & 0xFFFFFF
		p.PacketClass = uint16(be.Uint32(b[i+4:]))
		i += 8
	}
	if (h>>22)&3 != 0 {
		if !need(4) {
			return p, errors.New("truncated timestamp")
		}
		p.TSInt, p.HasTS = be.Uint32(b[i:]), true
		i += 4
	}
	if (h>>20)&3 != 0 {
		if !need(8) {
			return p, errors.New("truncated timestamp")
		}
		p.TSFrac, p.HasTS = be.Uint64(b[i:]), true
		i += 8
	}
	end := size
	if h&(1<<26) != 0 { // trailer
		end -= 4
	}
	if i > end {
		return p, errors.New("truncated packet")
	}
	p.Payload = b[i:end]
	return p, nil
}

// AudioRate returns the sample rate of a DAX audio class, or 0.
func AudioRate(class uint16) int {
	switch class {
	case classFloat24k, classInt16_24k:
		return 24000
	case classFloat48k, classInt16_48k:
		return 48000
	case classFloat96k, classInt16_96k:
		return 96000
	}
	return 0
}

// AudioLeft returns the left (or only) channel of a DAX audio payload.
func AudioLeft(p Packet) []float32 {
	be := binary.BigEndian
	switch p.PacketClass {
	case classFloat24k, classFloat48k, classFloat96k:
		n := len(p.Payload) / 8
		out := make([]float32, n)
		for i := range out {
			out[i] = math.Float32frombits(be.Uint32(p.Payload[i*8:]))
		}
		return out
	case classInt16_24k, classInt16_48k, classInt16_96k:
		n := len(p.Payload) / 2
		out := make([]float32, n)
		for i := range out {
			out[i] = float32(int16(be.Uint16(p.Payload[i*2:]))) / 32767
		}
		return out
	}
	return nil
}

// MeterValues decodes a meter packet: pairs of (id uint16, raw int16).
func MeterValues(p Packet) map[int]int16 {
	be := binary.BigEndian
	m := map[int]int16{}
	for i := 0; i+4 <= len(p.Payload); i += 4 {
		m[int(be.Uint16(p.Payload[i:]))] = int16(be.Uint16(p.Payload[i+2:]))
	}
	return m
}
