package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProtectedTreePolicy(t *testing.T) {
	directory := imageEntry{mode: os.ModeDir | 0o755}
	file := imageEntry{mode: 0o444, links: 1, size: 50}
	cases := map[string]imageEntry{
		"writable":      {mode: 0o644, links: 1},
		"owner-runtime": {mode: 0o444, uid: 65532, links: 1},
		"hardlink":      {mode: 0o444, links: 2},
		"symlink":       {mode: os.ModeSymlink | 0o777, links: 1},
		"fifo":          {mode: os.ModeNamedPipe | 0o444, links: 1},
		"device":        {mode: os.ModeDevice | 0o444, links: 1},
		"oversized":     {mode: 0o444, links: 1, size: 3 * 1024 * 1024},
	}
	fixture := func() map[string]imageEntry {
		return map[string]imageEntry{"/": directory, "/srv": directory, protectedFixtureRoot: directory, defaultRuntimeInventoryPath: file}
	}
	inspect := func(entries map[string]imageEntry) func(string) (imageEntry, error) {
		return func(path string) (imageEntry, error) {
			value, exists := entries[path]
			if !exists {
				return imageEntry{}, os.ErrNotExist
			}
			return value, nil
		}
	}
	names := func(path string) ([]string, error) {
		if path == protectedFixtureRoot {
			return []string{"inventory.json"}, nil
		}
		return nil, nil
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			entries := fixture()
			entries[defaultRuntimeInventoryPath] = entry
			if _, err := inspectProtectedTree(protectedFixtureRoot, inspect(entries), names); err == nil {
				t.Fatal("unsafe image file admitted")
			}
		})
	}
	for _, path := range []string{"/", "/srv", protectedFixtureRoot} {
		for _, mode := range []os.FileMode{os.ModeDir | 0o777, os.ModeSymlink | 0o777, 0o444} {
			t.Run(path+mode.String(), func(t *testing.T) {
				entries := fixture()
				entries[path] = imageEntry{mode: mode}
				called := false
				list := func(string) ([]string, error) { called = true; return nil, nil }
				if _, err := inspectProtectedTree(protectedFixtureRoot, inspect(entries), list); err == nil || called {
					t.Fatalf("unsafe ancestor was descended: err=%v called=%t", err, called)
				}
			})
		}
	}
	entries := fixture()
	image, err := inspectProtectedTree(protectedFixtureRoot, inspect(entries), names)
	if err != nil || len(image.files) != 1 {
		t.Fatalf("valid tree: %v", err)
	}
	if _, err := inspectProtectedTree("/tmp/fixture", inspect(entries), names); err == nil {
		t.Fatal("alternate root admitted")
	}
	if _, err := inspectProtectedTree(protectedFixtureRoot, inspect(entries), func(string) ([]string, error) { return nil, errors.New("unreadable") }); err == nil {
		t.Fatal("unreadable tree admitted")
	}
	delete(entries, defaultRuntimeInventoryPath)
	if _, err := inspectProtectedTree(protectedFixtureRoot, inspect(entries), func(string) ([]string, error) { return nil, nil }); err == nil {
		t.Fatal("missing inventory admitted")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := checkProtectedEntry("data.sqlite"+suffix, file, false); err == nil {
			t.Fatal("SQLite sidecar admitted")
		}
	}
	sqlite := file
	sqlite.size = maxServingSQLiteBytes + 1
	if err := checkProtectedEntry("data.sqlite", sqlite, false); err == nil {
		t.Fatal("oversized SQLite admitted")
	}
}

func TestProtectedImageObservedPrivilegeAndMountRefusals(t *testing.T) {
	accepted := []byte("Uid:\t65532\t65532\t65532\t65532\nCapEff:\t0000000000000000\nCapPrm:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t0\n")
	if err := checkImageStatus(accepted); err != nil {
		t.Fatal(err)
	}
	// NoNewPrivs is diagnostic, not a prerequisite. NET_BIND_SERVICE cannot mutate the tree.
	if err := checkImageStatus([]byte("CapEff:\t0000000000000400\n")); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"Uid: 0 0 0 0", "Uid: 65532 65532 0 65532", "Uid: bad", "CapEff: 2", "CapPrm: 200000", "CapAmb: 80", "CapEff: malformed"} {
		if err := checkImageStatus([]byte(status)); err == nil {
			t.Fatalf("unsafe status accepted: %s", status)
		}
	}
	row := func(path string) []byte { return []byte("20 1 0:1 / " + path + " rw - tmpfs none rw\n") }
	for _, path := range []string{"/srv", protectedFixtureRoot, protectedFixtureRoot + "/data.sqlite"} {
		if err := checkImageMounts(row(path)); err == nil {
			t.Fatalf("overlapping mount accepted: %s", path)
		}
	}
	for _, path := range []string{"/", "/tmp", "/srv-other", "/srv/fixture-other"} {
		if err := checkImageMounts(row(path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkImageMounts([]byte("malformed")); err == nil {
		t.Fatal("malformed mount observation accepted")
	}
}

func TestProtectedStartupFailureReturnsNoHandler(t *testing.T) {
	for _, name := range []string{"CHINOOK_MANIFEST", "NORTHWIND_MANIFEST", "SAMPLE_DATABASES_INVENTORY", "OVDB_SELECTED_STORAGE"} {
		t.Setenv(name, "")
	}
	for _, name := range []string{"CHINOOK_MANIFEST", "NORTHWIND_MANIFEST", "SAMPLE_DATABASES_INVENTORY"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"unknown", string(protectedImageStorage)} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OVDB_SELECTED_STORAGE", value)
			t.Setenv("SAMPLE_DATABASES_INVENTORY", "/tmp/mutable/inventory.json")
			_, handler, cleanup, err := configuredHandler()
			if err == nil || handler != nil || cleanup != nil {
				t.Fatal("unsafe startup reached serving handler")
			}
		})
	}
	t.Setenv("OVDB_SELECTED_STORAGE", string(protectedImageStorage))
	t.Setenv("SAMPLE_DATABASES_INVENTORY", defaultRuntimeInventoryPath)
	t.Setenv("CHINOOK_MANIFEST", defaultRuntimeInventoryPath)
	if _, handler, _, err := configuredHandler(); err == nil || handler != nil {
		t.Fatal("legacy override admitted in image mode")
	}
	if _, _, err := newHandlerWithStorage(nil, protectedImageStorage, nil); err == nil {
		t.Fatal("image mount without proof admitted")
	}
	if _, _, err := newHandlerWithStorage(nil, selectedStorage("unknown"), nil); err == nil {
		t.Fatal("unknown strategy admitted")
	}
}

func TestExplicitSealedCopyStartupRetainsSelectedAndLegacy(t *testing.T) {
	path, providers := selectedInventoryFixture(t)
	t.Setenv("SAMPLE_DATABASES_INVENTORY", path)
	t.Setenv("OVDB_SELECTED_STORAGE", "")
	if _, err := configuredRuntime(); err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("selected mode guessed: %v", err)
	}
	t.Setenv("OVDB_SELECTED_STORAGE", string(sealedCopy))
	startup, err := configuredRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if startup.image != nil || startup.strategy != sealedCopy || !reflect.DeepEqual(databaseIDs(startup.providers), databaseIDs(providers)) {
		t.Fatal("explicit copy changed inventory")
	}
	handler, closeDBs, err := newHandlerWithStorage(startup.providers, startup.strategy, startup.image)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeDBs(); err != nil {
		t.Fatal(err)
	}
	if handler == nil {
		t.Fatal("copy startup lacks handler")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "candidate.sqlite")); err != nil {
		t.Fatal("copy close removed original")
	}
}
