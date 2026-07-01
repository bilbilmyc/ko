package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// hashFile returns (digest, diffID, size) for a file. digest is "sha256:..."
// in OCI form; diffID is the bare hex sha256 (matching OCI uncompressed
// layer semantics — for our tar.gz layers the diffID is the uncompressed
// tar hash, but we shortcut to file sha256 for simplicity since ko reads
// the layer as-is).
func hashFile(path string) (digest, diffID string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return "sha256:" + sum, sum, st.Size(), nil
}

func hashBytes(b []byte) (digest, diffID string, size int64, err error) {
	sum := sha256.Sum256(b)
	hex := hex.EncodeToString(sum[:])
	return "sha256:" + hex, hex, int64(len(b)), nil
}

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestAndSize(b []byte) (string, int64, error) {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:])), int64(len(b)), nil
}