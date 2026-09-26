//go:build linux

package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxClipboardReadsAdvertisedJPEG(t *testing.T) {
	for _, backend := range []struct {
		name string
		list string
		read string
	}{
		{"xclip", "-selection clipboard -t TARGETS -o", "-selection clipboard -t image/jpeg -o"},
		{"wl-paste", "--list-types", "--type image/jpeg"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			binDir := t.TempDir()
			// Only this fake backend is discoverable: neither a real clipboard nor
			// the other backend can hide a failed JPEG read. printf is a shell builtin.
			t.Setenv("PATH", binDir)
			script := "#!/bin/sh\ncase \"$*\" in\n" +
				"  \"" + backend.list + "\") printf 'image/jpeg\\n' ;;\n" +
				"  \"" + backend.read + "\") printf '\\377\\330\\000jpeg\\377\\331' ;;\n" +
				"  *) exit 1 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(binDir, backend.name), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}

			reader := NewClipboardReader()
			info, err := reader.Type()
			if err != nil || info.Type != ClipboardImage || info.Format != "jpeg" {
				t.Fatalf("Type() = %+v, %v; want image/jpeg", info, err)
			}
			got, err := reader.ImageBytes()
			if err != nil {
				t.Fatalf("ImageBytes() after advertising JPEG: %v", err)
			}
			want := []byte{0xff, 0xd8, 0x00, 'j', 'p', 'e', 'g', 0xff, 0xd9}
			if !bytes.Equal(got, want) {
				t.Fatalf("ImageBytes() = %x, want %x", got, want)
			}
		})
	}
}
