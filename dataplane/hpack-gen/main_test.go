package main

import (
	"bytes"
	"testing"
)

func TestEncode(t *testing.T) {
	cases := map[string][]byte{
		// 上游源码中的已知常量，用于校验码表。
		"nginx": {0x84, 0xaa, 0x63, 0x55, 0xe7},
		// 已写入 0001-syapp-rename.patch，并经 curl --http2 解码验证为 "server: syapp"。
		"syapp": {0x84, 0x47, 0xa1, 0xd7, 0x5f},
	}
	for s, want := range cases {
		got, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: got % x want % x", s, got, want)
		}
	}
	if _, err := Encode("SYAPP"); err == nil {
		t.Fatal("uppercase should be rejected")
	}
}
