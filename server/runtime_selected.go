package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/openvaultdb/cloud/server/internal/publisherselection"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func validateRuntimeMode(p runtimeDatabase, version int) error {
	bounded := p.ReadProfile == "bounded-immutable/1"
	if p.ReadProfile != "" && !bounded {
		return errors.New("unsupported runtime readProfile")
	}
	if p.ServingAdapter != "" && p.ServingAdapter != "separate-id/1" {
		return errors.New("unsupported runtime servingAdapter")
	}
	if version == 1 && (p.ReadProfile != "" || p.ServingAdapter != "" || p.ManifestSHA256 != "" || p.PublisherManifest != nil || p.PublicDescriptor != nil || p.RequirePublishedQuery != nil) {
		return errors.New("runtime version 1 does not admit selection/profile fields")
	}
	if version == 2 && !providerSHA256Pattern.MatchString(p.ManifestSHA256) {
		return errors.New("runtime version 2 requires manifestSha256")
	}
	if bounded {
		if !providerRepositoryPattern.MatchString(p.ProviderRepository) || !providerRevisionPattern.MatchString(p.ProviderRevision) || !providerSHA256Pattern.MatchString(p.SourceSHA256) || !providerSHA256Pattern.MatchString(p.ServingSHA256) {
			return errors.New("bounded runtime requires immutable provider/source/serving identity")
		}
		if p.ServingAdapter != "separate-id/1" || p.RequirePublishedQuery == nil || p.PublisherManifest == nil || p.PublicDescriptor == nil {
			return errors.New("bounded runtime requires adapter, publisherManifest, publicDescriptor and query flag")
		}
		for _, pin := range []*runtimePinnedFile{p.PublisherManifest, p.PublicDescriptor} {
			if !safePinnedPath(pin.Path) || !providerSHA256Pattern.MatchString(pin.SHA256) || pin.Bytes <= 0 || pin.Bytes > publisherselection.MaxBytes {
				return errors.New("invalid bounded metadata pin")
			}
		}
	} else if p.ServingAdapter != "" || p.PublisherManifest != nil || p.PublicDescriptor != nil || p.RequirePublishedQuery != nil {
		return errors.New("runtime selection fields require bounded-immutable/1")
	}
	return nil
}
func safePinnedPath(path string) bool {
	if path == "" || strings.ContainsAny(path, "\\?#%") || filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if c < 32 || c == 127 {
				return false
			}
		}
	}
	return true
}

type selectedFiles struct{ manifest, publisher, descriptor []byte }

func readBoundedPinned(path, hash string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, publisherselection.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > publisherselection.MaxBytes || (size > 0 && int64(len(data)) != size) || digest(data) != hash {
		return nil, fmt.Errorf("metadata pin mismatch: %s", filepath.Base(path))
	}
	return data, nil
}
func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func verifySelectedFiles(p runtimeDatabase, base string) (*selectedFiles, error) {
	if err := validateRuntimeMode(p, 2); err != nil {
		return nil, err
	}
	f := &selectedFiles{}
	var err error
	f.manifest, err = readBoundedPinned(filepath.Join(base, filepath.Base(p.Manifest)), p.ManifestSHA256, 0)
	if err != nil {
		return nil, err
	}
	f.publisher, err = readBoundedPinned(filepath.Join(base, p.ID+".publisher.yaml"), p.PublisherManifest.SHA256, p.PublisherManifest.Bytes)
	if err != nil {
		return nil, err
	}
	f.descriptor, err = readBoundedPinned(filepath.Join(base, p.ID+".descriptor.json"), p.PublicDescriptor.SHA256, p.PublicDescriptor.Bytes)
	if err != nil {
		return nil, err
	}
	publisher, err := publisherselection.Parse(f.publisher)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(f.manifest)
	if err != nil {
		return nil, err
	}
	if m.Database.ID != p.ID || m.Database.SchemaMode != "strict" || m.Storage.Engine != "sqlite" || m.Storage.Path != "./"+p.ID+".sqlite" || m.Storage.SQLite == nil || m.Storage.SQLite.BusyTimeout == nil || *m.Storage.SQLite.BusyTimeout != "0s" || m.ACL != nil || m.ACLStore != nil {
		return nil, errors.New("bounded manifest identity/storage/profile mismatch")
	}
	if publisher.ID != p.ID || m.Schemas == nil {
		return nil, errors.New("publisher/generated manifest identity mismatch")
	}
	names := make([]string, 0, len(m.Schemas.Collections))
	for name := range m.Schemas.Collections {
		names = append(names, name)
	}
	sort.Strings(names)
	selected := append([]string(nil), publisher.Recordsets...)
	sort.Strings(selected)
	if !reflect.DeepEqual(names, selected) || len(m.Storage.SQLite.RecordKeys) != len(names) {
		return nil, errors.New("publisher selection differs from generated collections/key map")
	}
	for _, name := range names {
		if m.Storage.SQLite.RecordKeys[name] == "" {
			return nil, errors.New("publisher selection missing generated record key")
		}
	}
	var descriptor struct {
		Format       string                     `json:"format"`
		LocalID      string                     `json:"localId"`
		ID           string                     `json:"id"`
		Homepage     string                     `json:"homepage"`
		ServerID     string                     `json:"serverId"`
		Base         string                     `json:"serverDbBaseUrl"`
		API          string                     `json:"apiUrl"`
		Deployment   map[string]string          `json:"deployment"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Recordsets   []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Columns []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"columns"`
		} `json:"recordsets"`
	}
	if err := validateDescriptorMembers(f.descriptor); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(f.descriptor, &descriptor); err != nil {
		return nil, err
	}
	root := "https://cloud.openvaultdb.com"
	dbURL := root + "/ovdb/dbs/" + p.ID
	deployment := map[string]string{"engine": "sqlite", "url": dbURL, "discovery": root + "/.well-known/openvaultdb"}
	publisherDeployment := map[string]string{"engine": "sqlite", "url": dbURL, "discovery": root + "/.well-known/openvaultdb", "recordset_page": dbURL + "/collections/{name}"}
	if descriptor.Format != "ovdb-database/draft-1" || descriptor.LocalID != p.ID || descriptor.ID != dbURL || descriptor.Homepage == "" || descriptor.Homepage != publisher.Homepage || publisher.URL != dbURL || descriptor.ServerID != root+"/ovdb/" || descriptor.Base != dbURL || descriptor.API != root+"/v1/databases/"+p.ID || !reflect.DeepEqual(descriptor.Deployment, deployment) || !reflect.DeepEqual(publisher.Deployment, publisherDeployment) {
		return nil, errors.New("publisher/descriptor route linkage mismatch")
	}
	if !checkedDescriptorCapabilities(descriptor.Capabilities, *p.RequirePublishedQuery) {
		return nil, errors.New("descriptor query/profile flags mismatch")
	}
	publicNames := make([]string, 0, len(descriptor.Recordsets))
	for _, r := range descriptor.Recordsets {
		if r.Kind != "" && r.Kind != "table" {
			return nil, errors.New("descriptor recordset must be a selected physical table")
		}
		publicNames = append(publicNames, r.Name)
		collection, exists := m.Schemas.Collections[r.Name]
		if !exists || len(collection.Fields) != len(r.Columns)+1 {
			return nil, errors.New("descriptor/generated field linkage mismatch")
		}
		occupied := map[string]bool{}
		for _, column := range r.Columns {
			if column.Name == "" || occupied[strings.ToLower(column.Name)] {
				return nil, errors.New("descriptor repeats native columns")
			}
			occupied[strings.ToLower(column.Name)] = true
			field, exists := collection.Fields[column.Name]
			if !exists || !matchesNativeField(column.Type, field) {
				return nil, errors.New("descriptor/generated native field mismatch")
			}
		}
		helper := "__ovdb_record_id"
		for suffix := 1; occupied[strings.ToLower(helper)]; suffix++ {
			helper = fmt.Sprintf("__ovdb_record_id_%d", suffix)
		}
		if m.Storage.SQLite.RecordKeys[r.Name] != helper || collection.Fields[helper].Type != schema.TypeString {
			return nil, errors.New("generated key map does not use allocated separate serving helper")
		}
	}
	sort.Strings(publicNames)
	if !reflect.DeepEqual(publicNames, selected) {
		return nil, errors.New("descriptor recordsets differ from publisher selection")
	}
	for _, name := range append(append([]string{p.SmokeRecordset}, p.SmokeRecordsets...), p.EmptyRecordsets...) {
		i := sort.SearchStrings(selected, name)
		if i == len(selected) || selected[i] != name {
			return nil, errors.New("runtime smoke names outside publisher selection")
		}
	}
	return f, nil
}

// mountSelectedSnapshot copies through held source handles, verifies the copied
// bytes, then seals the private directory before the library reopens it. The
// mounted driver never opens original paths. This trusts the process owner;
// an adversary with the same OS credentials is outside this startup boundary.
func mountSelectedSnapshot(p runtimeDatabase) (*core.Database, error) {
	base := filepath.Dir(p.Manifest)
	files, err := verifySelectedFiles(p, base)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "ovdb-selected-")
	if err != nil {
		return nil, err
	}
	cleanup := func() error { _ = os.Chmod(directory, 0o700); return os.RemoveAll(directory) }
	failed := true
	defer func() {
		if failed {
			_ = cleanup()
		}
	}()
	if err = copyPinnedSQLite(filepath.Join(base, p.ID+".sqlite"), filepath.Join(directory, p.ID+".sqlite"), p.ServingSHA256); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(directory, filepath.Base(p.Manifest))
	if err = os.WriteFile(manifestPath, files.manifest, 0o400); err != nil {
		return nil, err
	}
	if err = os.Chmod(directory, 0o500); err != nil {
		return nil, err
	}
	database, err := mount.FileWithOptions(manifestPath, mount.Options{CatalogueDir: directory})
	if err != nil {
		return nil, err
	}
	database.OnClose(cleanup)
	failed = false
	return database, nil
}
func copyPinnedSQLite(source, destination, hash string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 2*1024*1024*1024 {
		return errors.New("invalid serving SQLite file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hasher), io.LimitReader(input, 2*1024*1024*1024+1))
	closeErr := output.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != hash {
		return errors.New("serving SQLite snapshot pin mismatch")
	}
	return nil
}

// Reject JSON duplicate members before typed decoding, including inside closed pins.
func rejectDuplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON metadata depth exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delimiter == '{' {
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON member")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		} else if delimiter == '[' {
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		} else {
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON requires exactly one document")
	}
	return nil
}

var decimalTypePattern = regexp.MustCompile(`(?i)^\s*DECIMAL_TEXT\s*\(\s*(\d+)\s*,\s*(\d+)\s*\)\s*$`)

func matchesNativeField(sqlType string, field schema.Field) bool {
	if decimal := decimalTypePattern.FindStringSubmatch(sqlType); decimal != nil {
		precision, _ := strconv.Atoi(decimal[1])
		scale, _ := strconv.Atoi(decimal[2])
		return field.Type == schema.TypeDecimal && field.Decimal != nil && field.Decimal.Precision == precision && field.Decimal.Scale == scale && field.Decimal.Storage == "text"
	}
	kind := strings.ToUpper(sqlType)
	expected := schema.TypeString
	if strings.Contains(kind, "INT") {
		expected = schema.TypeInteger
	} else if strings.Contains(kind, "BLOB") {
		expected = schema.TypeAny
	} else {
		for _, token := range []string{"REAL", "FLOA", "DOUB", "DECIMAL", "NUMERIC", "MONEY"} {
			if strings.Contains(kind, token) {
				expected = schema.TypeNumber
				break
			}
		}
	}
	return field.Type == expected && field.Decimal == nil
}

func checkedDescriptorCapabilities(flags map[string]json.RawMessage, query bool) bool {
	if len(flags) != 3 {
		return false
	}
	for name, expected := range map[string]bool{"read": true, "write": false, "query": query} {
		raw, exists := flags[name]
		if !exists {
			return false
		}
		var actual *bool
		if json.Unmarshal(raw, &actual) != nil || actual == nil || *actual != expected {
			return false
		}
	}
	return true
}

// canonicalMembers checks the original JSON member spelling and presence before
// encoding/json can match a case alias to a tagged Go field. Unconsumed public
// descriptor metadata stays open; consumed bindings and runtime pins stay exact.
func canonicalMembers(data []byte, required, optional map[string]byte, closed bool) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, errors.New("JSON binding must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	expected := make(map[string]byte, len(required)+len(optional))
	for name, kind := range required {
		expected[name] = kind
	}
	for name, kind := range optional {
		expected[name] = kind
	}
	for name, value := range object {
		kind, known := expected[name]
		if !known {
			for canonical := range expected {
				if strings.EqualFold(name, canonical) {
					return nil, fmt.Errorf("JSON binding member %q must use canonical spelling %q", name, canonical)
				}
			}
			if closed {
				return nil, fmt.Errorf("unknown JSON binding member %q", name)
			}
			continue
		}
		raw := bytes.TrimSpace(value)
		if kind == 'a' {
			continue
		}
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return nil, fmt.Errorf("JSON binding member %q cannot be null", name)
		}
		matches := kind == 'a' || raw[0] == kind || (kind == 'b' && (bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false")))) || (kind == 'n' && (raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')))
		if !matches {
			return nil, fmt.Errorf("JSON binding member %q has wrong type", name)
		}
	}
	for name := range required {
		if _, exists := object[name]; !exists {
			return nil, fmt.Errorf("JSON binding lacks canonical member %q", name)
		}
	}
	return object, nil
}

func runtimeMemberKinds(value any) map[string]byte {
	result := map[string]byte{}
	typeOf := reflect.TypeOf(value)
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			result[name] = 'a'
		}
	}
	return result
}
func validateRuntimeMembers(data []byte) error {
	if err := rejectDuplicateJSON(data); err != nil {
		return err
	}
	inventory, err := canonicalMembers(data, map[string]byte{"version": 'n', "databases": '['}, nil, true)
	if err != nil {
		return err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(inventory["databases"], &entries); err != nil {
		return err
	}
	kinds := runtimeMemberKinds(runtimeDatabase{})
	kinds["manifestSha256"] = '"'
	kinds["servingAdapter"] = '"'
	kinds["readProfile"] = '"'
	kinds["publisherManifest"] = '{'
	kinds["publicDescriptor"] = '{'
	kinds["requirePublishedQuery"] = 'b'
	for _, entry := range entries {
		members, err := canonicalMembers(entry, nil, kinds, true)
		if err != nil {
			return err
		}
		for _, name := range []string{"publisherManifest", "publicDescriptor"} {
			if pin, present := members[name]; present {
				if _, err := canonicalMembers(pin, map[string]byte{"path": '"', "sha256": '"', "bytes": 'n'}, nil, true); err != nil {
					return err
				}
			}
		}
		for name, accepted := range map[string]string{"servingAdapter": "separate-id/1", "readProfile": "bounded-immutable/1"} {
			if raw, present := members[name]; present {
				var value string
				if json.Unmarshal(raw, &value) != nil || value != accepted {
					return fmt.Errorf("unsupported explicit runtime %s", name)
				}
			}
		}
	}
	return nil
}
func validateDescriptorMembers(data []byte) error {
	if err := rejectDuplicateJSON(data); err != nil {
		return err
	}
	members, err := canonicalMembers(data, map[string]byte{"format": '"', "localId": '"', "id": '"', "homepage": '"', "serverId": '"', "serverDbBaseUrl": '"', "apiUrl": '"', "deployment": '{', "capabilities": '{', "recordsets": '['}, nil, false)
	if err != nil {
		return err
	}
	var recordsets []json.RawMessage
	if err := json.Unmarshal(members["recordsets"], &recordsets); err != nil {
		return err
	}
	for _, raw := range recordsets {
		recordset, err := canonicalMembers(raw, map[string]byte{"name": '"', "columns": '['}, map[string]byte{"kind": '"'}, false)
		if err != nil {
			return err
		}
		var columns []json.RawMessage
		if err := json.Unmarshal(recordset["columns"], &columns); err != nil {
			return err
		}
		for _, column := range columns {
			if _, err := canonicalMembers(column, map[string]byte{"name": '"', "type": '"'}, nil, false); err != nil {
				return err
			}
		}
	}
	return nil
}
