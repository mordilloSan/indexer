package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStringSliceFlag(t *testing.T) {
	var flag stringSliceFlag
	if err := flag.Set("/one"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if err := flag.Set("/two"); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	want := "/one,/two"
	if got := flag.String(); got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
}

func TestNormalizeTargetPath(t *testing.T) {
	root := filepath.Join(os.TempDir(), "index-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "absolute inside root",
			input: root,
			want:  filepath.Clean(root),
		},
		{
			name:  "relative path becomes inside root",
			input: "docs",
			want:  filepath.Join(filepath.Clean(root), "docs"),
		},
		{
			name:    "outside root",
			input:   "/tmp/elsewhere",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeTargetPath(root, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTargetPath error: %v", err)
			}
			if !strings.HasPrefix(got, filepath.Clean(root)) {
				t.Fatalf("result %s not inside root %s", got, root)
			}
			if filepath.Clean(got) != filepath.Clean(tt.want) {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
