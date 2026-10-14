package mdns

import (
	"net"
	"testing"
)

func TestQuestionAndAnswerPacket(t *testing.T) {
	query := question("media.local")
	q := questions(query)
	if len(q) != 1 || q[0] != "media.local" {
		t.Fatalf("questions: %v", q)
	}
	packet := response("media.local", net.ParseIP("192.0.2.10"))
	a := answers(packet)
	if len(a) != 1 || a[0].name != "media.local" || !a[0].ip.Equal(net.ParseIP("192.0.2.10")) {
		t.Fatalf("answers: %v", a)
	}
}
