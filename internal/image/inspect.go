package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// inspectBundleImpl is the library-side equivalent of `ko pack inspect`.
func inspectBundleImpl(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Size == 0 {
			continue
		}
		body := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, body); err != nil {
			return err
		}
		files[h.Name] = body
	}
	idxRaw, ok := files["index.json"]
	if !ok {
		return fmt.Errorf("not a ko bundle: index.json missing")
	}
	var idx ImageIndex
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	fmt.Fprintln(w, "image index:")
	fmt.Fprintf(w, "  schemaVersion: %d\n", idx.SchemaVersion)
	fmt.Fprintf(w, "  mediaType:     %s\n", idx.MediaType)
	fmt.Fprintln(w, "  manifests:")
	for _, m := range idx.Manifests {
		fmt.Fprintf(w, "    - %s  %s  (%d bytes)\n", m.MediaType, m.Digest, m.Size)
		if m.Platform != nil {
			fmt.Fprintf(w, "      platform: %s/%s\n", m.Platform.OS, m.Platform.Architecture)
		}
		manifestBody, ok := files["blobs/sha256/"+trimDigest(m.Digest)]
		if !ok {
			continue
		}
		var mf Manifest
		if err := json.Unmarshal(manifestBody, &mf); err != nil {
			continue
		}
		fmt.Fprintln(w, "      layers:")
		ls := append([]LayerDescriptor(nil), mf.Layers...)
		sort.Slice(ls, func(i, j int) bool { return ls[i].MediaType < ls[j].MediaType })
		for _, l := range ls {
			fmt.Fprintf(w, "        - %s  %s  (%d bytes)\n", l.MediaType, l.Digest, l.Size)
		}
	}
	return nil
}

func trimDigest(d string) string {
	const p = "sha256:"
	if len(d) > len(p) && d[:len(p)] == p {
		return d[len(p):]
	}
	return d
}