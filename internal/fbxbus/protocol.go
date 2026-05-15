// Package fbxbus implémente un client natif du bus IPC fbxbus utilisé en
// interne par fbxhome (et d'autres démons Freebox sur la caméra Qiara).
//
// Le protocole est DBus-like : Unix socket SOCK_SEQPACKET sur l'adresse
// abstraite @fbxbus_daemon, messages préfixés par un en-tête fixe puis
// une suite de fields TLV (PATH, MEMBER, REPLY_SERIAL, SENDER, SIGNATURE).
//
// Référence wire format complète : memory/feedback_fbxbus_protocol.md
package fbxbus

import (
	"encoding/binary"
	"fmt"
)

// Wire constants — découverts via RE strace (cf. memory).
const (
	magicLong = 0x6c // little-endian indicator ('l')

	msgCall   = 1
	msgReply  = 2
	msgSignal = 3
	msgError  = 4

	fcPath        = 0x01
	fcMember      = 0x03
	fcErrorName   = 0x04
	fcReplySerial = 0x05
	fcDest        = 0x06
	fcSender      = 0x07
	fcSignature   = 0x08

	sigString = 's' // 0x73
	sigUint32 = 'u' // 0x75
	sigInt32  = 'i' // 0x69
	sigByte   = 'y'
	sigBool   = 'b'
)

// message représente un message fbxbus parsé.
type message struct {
	Type     byte
	Serial   uint32
	BodyLen  uint32 // value annoncée par le header, sémantique exacte à confirmer
	Path     string
	Member   string
	Sender   string
	ErrName  string
	Signat   string
	ReplyTo  uint32 // serial du message auquel on répond (REPLY_SERIAL)
	Body     []byte // bytes restants après les fields
}

// encodeStringField encode un champ string TLV.
// Format : fc(1) sig=0x73(1) pad(2)=0 len(4) bytes NUL
func encodeStringField(fc byte, s string) []byte {
	b := []byte(s)
	out := make([]byte, 0, 8+len(b)+1)
	out = append(out, fc, sigString, 0, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b)))
	out = append(out, b...)
	out = append(out, 0)
	return out
}

// helloFrame retourne la frame hello byte-for-byte vue sur le wire.
// Le hello est invariant (sauf le serial) — on copie textuellement les bytes
// du strace fbxbusctl plutôt que de réimplémenter l'encodage des SIGNATURE
// fields courts qui suit un format DBus-like spécial.
func helloFrame(serial uint32) []byte {
	frame := []byte{
		0x6c, 0x01, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x73, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x2f, 0x66, 0x62, 0x78, 0x62, 0x75, 0x73, 0x00,
		0x03, 0x73, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x00,
		0x08, 0x73, 0x01, 0x00, 0x00, 0x00, 0x69, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	binary.LittleEndian.PutUint32(frame[8:], serial)
	return frame
}

// makeFrame construit un message complet (header + fields + body).
// body : payload value(s) qui suivent les fields (les "args" du signal/call).
// bodyLen : la valeur exacte à inscrire dans le header pour body_len.
// Sémantique de body_len observée empiriquement (pas toujours = len(body)).
func makeFrame(msgType byte, serial uint32, bodyLen uint32, fields, body []byte) []byte {
	out := make([]byte, 0, 12+len(fields)+len(body))
	out = append(out, magicLong, msgType, 0, 0)
	out = binary.LittleEndian.AppendUint32(out, bodyLen)
	out = binary.LittleEndian.AppendUint32(out, serial)
	out = append(out, fields...)
	out = append(out, body...)
	return out
}

// parseMessage parse un buffer reçu en un message structuré.
// Tolère les bytes en trop en fin de buffer.
func parseMessage(buf []byte) (*message, error) {
	if len(buf) < 12 {
		return nil, fmt.Errorf("frame too short: %d bytes", len(buf))
	}
	if buf[0] != magicLong {
		return nil, fmt.Errorf("unsupported magic 0x%02x (only 0x6c implemented)", buf[0])
	}
	m := &message{
		Type:    buf[1],
		BodyLen: binary.LittleEndian.Uint32(buf[4:]),
		Serial:  binary.LittleEndian.Uint32(buf[8:]),
	}
	off := 12
	for off+8 <= len(buf) {
		fc := buf[off]
		sig := buf[off+1]
		// next 2 bytes are pad
		switch sig {
		case sigString:
			ln := binary.LittleEndian.Uint32(buf[off+4:])
			start := off + 8
			end := start + int(ln)
			if end > len(buf) {
				return nil, fmt.Errorf("string field overflow at offset %d: len=%d, remaining=%d", off, ln, len(buf)-start)
			}
			s := string(buf[start:end])
			switch fc {
			case fcPath:
				m.Path = s
			case fcMember:
				m.Member = s
			case fcSender:
				m.Sender = s
			case fcErrorName:
				m.ErrName = s
			case fcSignature:
				m.Signat = s
			}
			// +1 NUL après la string
			off = end + 1
		case sigUint32, sigInt32:
			v := binary.LittleEndian.Uint32(buf[off+4:])
			if fc == fcReplySerial {
				m.ReplyTo = v
			}
			off += 8
		default:
			// Sig inconnu → on stoppe le parse des fields, le reste est body
			goto bodyTail
		}
	}
bodyTail:
	if off < len(buf) {
		m.Body = buf[off:]
	}
	return m, nil
}
