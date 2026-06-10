package hosting

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

func TestPackageArtifactPassesThroughGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("already-a-tar-gz"))
	_ = gz.Close()
	original := buf.Bytes()

	data, name := packageArtifact(original)
	if name != "code.tar.gz" {
		t.Errorf("filename = %q, want code.tar.gz", name)
	}
	if !bytes.Equal(data, original) {
		t.Error("gzip artifact should pass through unchanged")
	}
}

func TestPackageArtifactWrapsNonGzip(t *testing.T) {
	raw := []byte("plain-uncompressed-tar-bytes")
	data, name := packageArtifact(raw)
	if name != "code.tar.gz" {
		t.Errorf("filename = %q, want code.tar.gz", name)
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		t.Fatalf("fallback output is not gzip: % x", data)
	}
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("round-trip = %q, want %q", got, raw)
	}
}
