package fbxbus

import (
	"encoding/binary"
)

// fastFrame construit une frame compacte (magic 0x2f) pour les actions
// enable/enabled sur /fbxbus/p2p/* et /fbxbus/monitor/*.
//
// Layout observé byte-pour-byte sur le wire :
//   total_len(4) | f1=1(4) | f2=1(4) | path_len(4) path NUL pad-4 | member_len(4) member NUL
//
// Le serial n'apparaît PAS dans le fast frame (différence majeure avec les
// frames 0x6c). f1 et f2 sont des constantes (action code = 1).
func fastFrame(_ uint32, path, member string) []byte {
	pathBytes := []byte(path)
	memBytes := []byte(member)

	body := make([]byte, 0, 16+len(pathBytes)+1+4+4+len(memBytes)+1)
	body = binary.LittleEndian.AppendUint32(body, 1) // f1 = 1
	body = binary.LittleEndian.AppendUint32(body, 1) // f2 = 1
	body = binary.LittleEndian.AppendUint32(body, uint32(len(pathBytes)))
	body = append(body, pathBytes...)
	body = append(body, 0)
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	body = binary.LittleEndian.AppendUint32(body, uint32(len(memBytes)))
	body = append(body, memBytes...)
	body = append(body, 0)

	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

// nameRequestFrame envoie CALL /fbxbus/name member="request" signature="sb".
func nameRequestFrame(serial uint32, sender string) []byte {
	out := []byte{
		0x6c, 0x01, 0x00, 0x00,
		0x13, 0x00, 0x00, 0x00,
		0, 0, 0, 0,
		// PATH /fbxbus/name
		0x01, 0x73, 0x00, 0x00, 0x0c, 0x00, 0x00, 0x00,
		'/', 'f', 'b', 'x', 'b', 'u', 's', '/', 'n', 'a', 'm', 'e', 0x00,
		// MEMBER "request" (format inline)
		0x03, 0x73, 0x00, 0x07, 0x00, 0x00, 0x00,
		'r', 'e', 'q', 'u', 'e', 's', 't', 0x00,
		// SIGNATURE "sb"
		0x08, 0x73, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00,
		's', 'b', 0x00,
	}
	// SENDER (format inline)
	sb := []byte(sender)
	out = append(out, 0x07, 0x73, 0x00, byte(len(sb)), 0x00, 0x00, 0x00)
	out = append(out, sb...)
	out = append(out, 0x00)
	for len(out) < 80 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[8:], serial)
	return out
}

// nameRequestBody : body suivant nameRequestFrame. Format : name_len(4) name NUL pad-4
func nameRequestBody(peerName string) []byte {
	nb := []byte(peerName)
	out := make([]byte, 0, 4+len(nb)+4)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(nb)))
	out = append(out, nb...)
	out = append(out, 0x00)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	return out
}

// filterAddFrame retourne la frame "filter add" (CALL /fbxbus/filter member=add
// signature=a(iss())). Le body de subscription est envoyé séparément via
// subscribeBody2.
//
// bodyLen : valeur à inscrire dans le header body_len. Pour les filter_add
// d'init (sans subscribe body qui suit), c'est 0x37 (constante observée).
// Pour les filter_add qui PRÉCÈDENT un subscribeBody2, c'est la taille
// totale du body (typiquement 0x35=53 pour "/fbxhome"+"alarm_status_changed").
func filterAddFrame(serial uint32, sender string, bodyLen uint32) []byte {
	out := []byte{
		0x6c, 0x01, 0x00, 0x00,
		0, 0, 0, 0, // body_len filled below
		0, 0, 0, 0, // serial filled below
		// PATH "/fbxbus/filter"
		0x01, 0x73, 0x00, 0x00, 0x0e, 0x00, 0x00, 0x00,
		'/', 'f', 'b', 'x', 'b', 'u', 's', '/', 'f', 'i', 'l', 't', 'e', 'r', 0x00,
		// MEMBER "add" (format inline observé : 03 73 00 00 00 03 00 00 00 add\0)
		0x03, 0x73, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 'a', 'd', 'd', 0x00,
		// SIGNATURE "a(iss())"
		0x08, 0x73, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00,
		'a', '(', 'i', 's', 's', '(', ')', ')', 0x00,
	}
	// SENDER (format inline byte-len) : 07 73 00 <len> 00 00 00 <bytes> NUL pad
	sb := []byte(sender)
	out = append(out, 0x07, 0x73, 0x00, byte(len(sb)), 0x00, 0x00, 0x00)
	out = append(out, sb...)
	out = append(out, 0x00)
	for len(out) < 88 {
		out = append(out, 0)
	}
	binary.LittleEndian.PutUint32(out[4:], bodyLen)
	binary.LittleEndian.PutUint32(out[8:], serial)
	return out
}

// subscribeBody2 — body envoyé après le filter_add. Contient le match rule.
// Format observé pour `("fbxhome", "alarm_status_changed")` :
//   total_len(4) | sig_type(4)=1 | filter_id(4) | path_len(4) | path NUL pad-4 | member_len(4) | member NUL
func subscribeBody2(daemon, signalName string) []byte {
	daemonStr := "/" + daemon
	pb := []byte(daemonStr)
	mb := []byte(signalName)

	body := make([]byte, 0, 16+len(pb)+4+4+len(mb)+1)
	body = binary.LittleEndian.AppendUint32(body, 1) // sig_type=1 (SIGNAL)
	body = binary.LittleEndian.AppendUint32(body, 4) // filter_id (slot)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(pb)))
	body = append(body, pb...)
	body = append(body, 0)
	for len(body)%4 != 0 {
		body = append(body, 0)
	}
	body = binary.LittleEndian.AppendUint32(body, uint32(len(mb)))
	body = append(body, mb...)
	body = append(body, 0)

	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}
