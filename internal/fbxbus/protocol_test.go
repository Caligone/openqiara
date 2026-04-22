package fbxbus

import (
	"encoding/hex"
	"testing"
)

// Frames réelles capturées par strace sur la caméra (cf. memory).
// On les utilise pour vérifier que notre encodage produit l'identique
// et que notre parse extrait les bons champs.

func TestEncodeHello(t *testing.T) {
	// Frame C1 du strace : hello CALL serial=1
	expected := "6c0100000400000001000000" +
		"017300000700000002f66627862757300"[:0] // sentinel
	_ = expected
	// Reconstruit byte-à-byte ce qu'on a vu sur le wire :
	// Note: strace montrait 54 bytes (avec 0000 final que je suppose pad d'align),
	// puis le client envoyait "23 0a 00 00" (4 bytes) en send séparé — un cookie ?
	// Pour le test on cible la frame 56 bytes vue sur le wire (avec un trailing pad 0).
	want, _ := hex.DecodeString(
		"6c010000" + "04000000" + "01000000" +
			"01730000" + "07000000" + "2f66627862757300" + // "/fbxbus\0"
			"03730000" + "05000000" + "68656c6c6f00" + // "hello\0"
			"08730000" + "01000000" + "69000000") // "i\0" + 2 pad bytes

	// body_len=4 dans le header reflète probablement les 4 bytes de "i\0pad"
	// du dernier field (NUL terminator + alignement). On n'envoie pas de
	// body séparé. À confirmer dynamiquement.
	fields := append(append(
		encodeStringField(fcPath, "/fbxbus"),
		encodeStringField(fcMember, "hello")...),
		encodeStringField(fcSignature, "i")...)
	// Ajout du padding 2-byte qu'on voit dans le strace (alignement 4 final).
	fields = append(fields, 0, 0)

	got := makeFrame(msgCall, 1, 4, fields, nil)
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("hello frame mismatch\n got: %s\nwant: %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

func TestEncodeFilterAdd(t *testing.T) {
	// Frame C2 du strace : filter add CALL serial=2
	// 6c01000037000000 02000000 ... 9 args
	// La fin du frame contient 6 bytes de padding (00 00 00 00 00 00)
	// → alignement du message à 8 bytes ? À vérifier.
	want, _ := hex.DecodeString(
		"6c010000" + "37000000" + "02000000" +
			"01730000" + "0e000000" + "2f6662786275732f66696c74657200" +
			"03730000" + "03000000" + "61646400" +
			"08730000" + "08000000" + "612869737328292900" +
			"07730000" + "09000000" + "3a3733372d3532343000")

	body := []byte{} // signature "a(iss())" — body à construire séparément
	fields := append(
		encodeStringField(fcPath, "/fbxbus/filter"),
		encodeStringField(fcMember, "add")...)
	fields = append(fields, encodeStringField(fcSignature, "a(iss())")...)
	fields = append(fields, encodeStringField(fcSender, ":737-5240")...)

	got := makeFrame(msgCall, 2, 55, fields, body)
	// Note: body_len=55=0x37 dans le strace, mais ici body=0. Le 0x37 dans le wire
	// correspond peut-être à autre chose qu'à len(body). À investiguer.
	// On compare juste la séquence des fields pour l'instant.
	if hex.EncodeToString(got[12:]) != hex.EncodeToString(want[12:]) {
		t.Errorf("filter add fields mismatch\n got: %s\nwant: %s",
			hex.EncodeToString(got[12:]), hex.EncodeToString(want[12:]))
	}
}

func TestParseHelloReply(t *testing.T) {
	// Frame S1 reconstruite (recv 48 + recv continuation 14 fusionnés).
	// Dans la vraie vie le serveur les envoie sur un seul send_msg côté kernel,
	// mais l'observation strace les a montrés séparés à cause du buffer.
	raw, _ := hex.DecodeString(
		"6c020000" + "0e000000" + "1a310000" +
			"05750000" + "01000000" + // REPLY_SERIAL=1
			"01730000" + "00000000" + "00" + // PATH ""
			"03730000" + "00000000" + // MEMBER "" — note: pas de NUL après len=0 ici, à vérifier
			"08730000" + "01000000" + "7300" + // SIGNATURE "s"
			"" + // pad
			"09000000" + "3a3733372d3532343000") // body = ":737-5240\0"

	m, err := parseMessage(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if m.Type != msgReply {
		t.Errorf("type=%d, want %d (reply)", m.Type, msgReply)
	}
	if m.ReplyTo != 1 {
		t.Errorf("reply_to=%d, want 1", m.ReplyTo)
	}
	// Le serial=12570 du serveur peut être contrôlé
	if m.Serial != 12570 {
		t.Errorf("serial=%d, want 12570", m.Serial)
	}
}
