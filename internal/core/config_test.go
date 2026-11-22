package core

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestGlobalConfigDir(t *testing.T) {
	t.Setenv("SLOPGUARD_CONFIG_DIR", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := GlobalConfigDir()
	if err != nil || got != filepath.Join(base, "slopguard") {
		t.Fatalf("default directory = %q, %v", got, err)
	}
	want := t.TempDir()
	t.Setenv("SLOPGUARD_CONFIG_DIR", want)
	got, err = GlobalConfigDir()
	if err != nil || got != want {
		t.Fatalf("override directory = %q, %v", got, err)
	}
}

func TestInitProjectConfig(t *testing.T) {
	root := t.TempDir()
	langs := []string{"go", "python"}
	var wg sync.WaitGroup
	results := make(chan InitResult, 8)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := InitProjectConfig(root, langs)
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	writes := 0
	for r := range results {
		if !r.AlreadyExists {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("created %d configs, want 1", writes)
	}
	cfg, err := LoadProjectConfig(root)
	if err != nil || !reflect.DeepEqual(cfg.Languages, langs) {
		t.Fatalf("load = %+v, %v", cfg, err)
	}
	custom := []byte("# user comment\r\nlanguages: [rust]\r\n")
	if err := os.WriteFile(configPath(root), custom, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := InitProjectConfig(root, langs)
	if err != nil || !r.AlreadyExists {
		t.Fatalf("init = %+v, %v", r, err)
	}
	data, err := os.ReadFile(r.Path)
	if err != nil || !bytes.Equal(data, custom) {
		t.Fatalf("existing config = %q, %v", data, err)
	}
}
