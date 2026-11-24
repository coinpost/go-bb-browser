package store

import (
	"testing"
)

func TestParseResponseSummaryOK(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tab":"abcd","seq":7}}`)
	seq, ok, errMsg := ParseResponseSummary(resp)
	if !ok || errMsg != "" || seq != 7 {
		t.Fatalf("got seq=%d ok=%v err=%q", seq, ok, errMsg)
	}
}

func TestParseResponseSummaryError(t *testing.T) {
	resp := []byte(`{"jsonrpc":"2.0","id":1,"error":{"message":"nope"}}`)
	_, ok, errMsg := ParseResponseSummary(resp)
	if ok || errMsg != "nope" {
		t.Fatalf("got ok=%v err=%q", ok, errMsg)
	}
}
