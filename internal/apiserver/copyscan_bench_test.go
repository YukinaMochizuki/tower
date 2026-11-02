package apiserver

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"testing"
)

// BenchmarkScan50MB scans 50 MB of wrapped base64 in 32 KiB writes for
// 200 secrets in all their forms: 180 tokens of 40 bytes, and 20
// kubeconfigs, each whole and by its token.
func BenchmarkScan50MB(b *testing.B) {
	var secrets []namedSecret
	for i := range 180 {
		secrets = append(secrets, namedSecret{name: fmt.Sprintf("$S%d", i), value: []byte(rand.Text() + rand.Text()[:14])})
	}
	for i := range 20 {
		kube := "apiVersion: v1\nkind: Config\nusers:\n- name: ci\n  user:\n    token: " + rand.Text() + "x" + rand.Text() + "\n"
		secrets = append(secrets, namedSecret{name: fmt.Sprintf("/kube%d", i), value: []byte(kube)})
	}
	patterns, _ := secretPatterns(secrets)
	chunk := make([]byte, 32<<10)
	raw := make([]byte, len(chunk)*3/4)
	_, _ = rand.Read(raw)
	base64.StdEncoding.Encode(chunk, raw)
	for i := 76; i < len(chunk); i += 77 {
		chunk[i] = '\n'
	}
	b.Logf("%d patterns", len(patterns))
	b.SetBytes(50 << 20)
	for b.Loop() {
		s := newScanner(patterns, 0, io.Discard)
		for range (50 << 20) / len(chunk) {
			if _, err := s.Write(chunk); err != nil {
				b.Fatal(err)
			}
		}
	}
}
