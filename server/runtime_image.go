package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openvaultdb/cloud/server/internal/publisherselection"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

type selectedStorage string

const (
	sealedCopy            selectedStorage = "sealed-copy"
	protectedImageStorage selectedStorage = "protected-image"
	protectedFixtureRoot                  = "/srv/fixture"
	maxServingSQLiteBytes int64           = 2 * 1024 * 1024 * 1024
	maxImageEntries                       = 4096
)

type imageEntry struct {
	mode   os.FileMode
	uid    uint32
	links  uint64
	device uint64
	inode  uint64
	size   int64
}

type protectedImage struct {
	root           string
	files          map[string]imageEntry
	verifiedSQLite map[string]string
}

type runtimeStartup struct {
	providers []runtimeDatabase
	strategy  selectedStorage
	image     *protectedImage
}

// The trusted build/deploy owner must establish immutable image/revision backing.
// Startup enforces the complementary process and filesystem boundary before reads.
func configuredRuntime() (runtimeStartup, error) {
	choice := selectedStorage(os.Getenv("OVDB_SELECTED_STORAGE"))
	if choice != "" && choice != sealedCopy && choice != protectedImageStorage {
		return runtimeStartup{}, errors.New("unsupported OVDB_SELECTED_STORAGE")
	}
	if choice == protectedImageStorage {
		if value, exists := os.LookupEnv("SAMPLE_DATABASES_INVENTORY"); exists && value != defaultRuntimeInventoryPath {
			return runtimeStartup{}, errors.New("protected-image requires the fixed /srv/fixture/inventory.json")
		}
		for _, name := range []string{"CHINOOK_MANIFEST", "NORTHWIND_MANIFEST"} {
			if _, exists := os.LookupEnv(name); exists {
				return runtimeStartup{}, errors.New("protected-image rejects legacy manifest environment paths")
			}
		}
		image, err := admitProtectedImage()
		if err != nil {
			return runtimeStartup{}, err
		}
		providers, err := loadRuntimeInventoryWithImage(defaultRuntimeInventoryPath, image)
		if err != nil {
			return runtimeStartup{}, err
		}
		providers, err = appendConfiguredDemoPostgres(providers)
		if err != nil {
			return runtimeStartup{}, err
		}
		return runtimeStartup{providers: providers, strategy: choice, image: image}, nil
	}
	providers, err := configuredDatabases()
	if err != nil {
		return runtimeStartup{}, err
	}
	providers, err = appendConfiguredDemoPostgres(providers)
	if err != nil {
		return runtimeStartup{}, err
	}
	if choice == "" {
		for _, provider := range providers {
			if provider.ReadProfile != "" {
				return runtimeStartup{}, errors.New("selected databases require explicit OVDB_SELECTED_STORAGE")
			}
		}
		choice = sealedCopy // preserve the legacy startup environment contract
	}
	return runtimeStartup{providers: providers, strategy: choice}, nil
}

func configuredHandler() ([]runtimeDatabase, http.Handler, func() error, error) {
	startup, err := configuredRuntime()
	if err != nil {
		return nil, nil, nil, err
	}
	return configuredHandlerWithRuntime(startup)
}

func checkProtectedEntry(path string, entry imageEntry, directory bool) error {
	if entry.uid != 0 {
		return fmt.Errorf("protected image entry %q is not root-owned", path)
	}
	if directory {
		if !entry.mode.IsDir() || entry.mode.Perm()&0o022 != 0 || entry.mode&os.ModeSymlink != 0 {
			return fmt.Errorf("protected image directory %q is replaceable or not ordinary", path)
		}
	} else {
		if !entry.mode.IsRegular() || entry.mode.Perm()&0o222 != 0 || entry.mode&(os.ModeSetuid|os.ModeSetgid) != 0 || entry.links != 1 {
			return fmt.Errorf("protected image file %q is writable, linked or not regular", path)
		}
		limit := int64(publisherselection.MaxBytes)
		if strings.HasSuffix(path, ".sqlite") {
			limit = maxServingSQLiteBytes
		}
		if entry.size < 0 || entry.size > limit {
			return fmt.Errorf("protected image file %q exceeds its bound", path)
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if strings.HasSuffix(path, suffix) {
				return fmt.Errorf("protected image has SQLite sidecar %q", path)
			}
		}
	}
	return nil
}

// Validate ancestors before descending. Root-owned nonwritable preceding entries
// make subsequent ordinary Lstat resolution fixed against writers in our boundary.
func inspectProtectedTree(root string, inspect func(string) (imageEntry, error), names func(string) ([]string, error)) (*protectedImage, error) {
	if root != protectedFixtureRoot {
		return nil, errors.New("protected image root must be /srv/fixture")
	}
	for _, path := range []string{"/", "/srv", root} {
		entry, err := inspect(path)
		if err != nil {
			return nil, fmt.Errorf("inspect protected ancestor %q: %w", path, err)
		}
		if err := checkProtectedEntry(path, entry, true); err != nil {
			return nil, err
		}
	}
	image := &protectedImage{root: root, files: map[string]imageEntry{}, verifiedSQLite: map[string]string{}}
	count := 0
	var walk func(string) error
	walk = func(directory string) error {
		entries, err := names(directory)
		if err != nil {
			return err
		}
		for _, name := range entries {
			count++
			if count > maxImageEntries || !safeInventoryFilename(name) {
				return errors.New("protected image tree exceeds bounds or has unsafe name")
			}
			path := filepath.Join(directory, name)
			entry, err := inspect(path)
			if err != nil {
				return err
			}
			if err := checkProtectedEntry(path, entry, entry.mode.IsDir()); err != nil {
				return err
			}
			if entry.mode.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
			} else {
				image.files[path] = entry
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	if _, exists := image.files[defaultRuntimeInventoryPath]; !exists {
		return nil, errors.New("protected image inventory is absent")
	}
	return image, nil
}

func (image *protectedImage) hashSQLite(path string) (hash string, err error) {
	expected, exists := image.files[path]
	if !exists || !strings.HasSuffix(path, ".sqlite") {
		return "", errors.New("SQLite path outside admitted protected image")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	before, err := imageFileEntry(file)
	if err != nil {
		return "", err
	}
	log.Printf("protected-image SQLite %s device=%d inode=%d bytes=%d", filepath.Base(path), before.device, before.inode, before.size)
	if before != expected {
		return "", errors.New("protected SQLite identity changed before hashing")
	}
	var header [20]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return "", err
	}
	if string(header[:16]) != "SQLite format 3\x00" || header[18] != 1 || header[19] != 1 {
		return "", errors.New("protected SQLite must be checkpointed without WAL dependency")
	}
	hasher := sha256.New()
	if _, err := hasher.Write(header[:]); err != nil {
		return "", err
	}
	n, err := io.CopyBuffer(hasher, io.LimitReader(file, maxServingSQLiteBytes+1), make([]byte, 64*1024))
	if err != nil {
		return "", err
	}
	after, err := imageFileEntry(file)
	if err != nil {
		return "", err
	}
	if before != after || n+int64(len(header)) != expected.size {
		return "", errors.New("protected SQLite identity changed while hashing")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func mountSelectedImage(provider runtimeDatabase, image *protectedImage) (*core.Database, error) {
	if image == nil || filepath.Dir(provider.Manifest) != image.root || image.verifiedSQLite[filepath.Join(image.root, provider.ID+".sqlite")] != provider.ServingSHA256 {
		return nil, errors.New("selected image mount lacks verified protected startup identity")
	}
	if _, exists := image.files[provider.Manifest]; !exists {
		return nil, errors.New("manifest outside admitted protected image")
	}
	if _, err := verifySelectedFiles(provider, image.root); err != nil {
		return nil, err
	}
	// All later manifest, driver/metadata, pool and sandbox opens use these exact
	// protected paths. No full-size copy, rewritten manifest or close-time deletion.
	return mount.FileWithOptions(provider.Manifest, mount.Options{CatalogueDir: image.root})
}

// Available diagnostics may refuse observed privilege/mount bypass. Missing proc
// observations rely on the trusted Cloud image/revision evidence, never a fallback.
func checkImageStatus(data []byte) error {
	// Privileges that can change ownership, bypass DAC, become root, acquire caps,
	// modify mounts/kernel state, or inject a privileged process defeat this proof.
	const bypass = uint64(1<<0 | 1<<1 | 1<<3 | 1<<7 | 1<<8 | 1<<16 | 1<<17 | 1<<18 | 1<<19 | 1<<21 | 1<<27)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if key == "Uid" {
			ids := strings.Fields(value)
			if len(ids) != 4 {
				return errors.New("malformed observed UID state")
			}
			for _, id := range ids {
				if id != "65532" {
					return errors.New("observed UID state defeats protected image")
				}
			}
		}
		if key == "CapEff" || key == "CapPrm" || key == "CapAmb" {
			mask, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil || mask&bypass != 0 {
				return fmt.Errorf("observed %s defeats protected image", key)
			}
		}
	}
	return nil
}
func checkImageMounts(data []byte) error {
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.Contains(line, " - ") {
			return errors.New("malformed observed mountinfo")
		}
		path := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if path == "/" {
			continue
		}
		if path == "/srv" || path == protectedFixtureRoot || strings.HasPrefix(path, protectedFixtureRoot+"/") {
			return fmt.Errorf("observed mount %q overlaps protected image", path)
		}
	}
	return nil
}
