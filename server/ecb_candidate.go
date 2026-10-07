package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The candidate has no call from configuredHandler or the public listener.
// An operator must supply a separate opt-in document before this composition
// can be exercised; the preparatory OVDB proposal is never a runtime input.
const (
	ecbHostFormat      = "ovdb-ecb-host-candidate/1"
	ecbPublisherSHA    = "399ce77bc4513b1a819f61e26a54fe8e6c46569b2b45ea078582d9ad6758697e"
	ecbPublisherBlob   = "f3e7e7b0410f23c2de6cc2fa69d9244e54b1e792"
	ecbPublisherCommit = "c72f1e711041a85ec67d6fe86f7621ae4bde302e"
	ecbDecoderSHA      = "20477d567705fe7cf8115caf696848b9ea1db2e1a165b977a4f54bd610955b49"
	ecbRightsSHA       = "08669fda7a7d255d1c77d2be733e587a23bb2ff1a916e96c727b82d2540cdc93"
)

// This is the local storage manifest for the fixed library HTTP profile. It
// is distinct from the pinned original publisher manifest, which is metadata.
const ecbHTTPManifest = `database:
  id: ecb
  schema_mode: strict
  retention: none
  license:
    name: ECB reuse conditions
    url: https://www.ecb.europa.eu/services/using-our-site/disclaimer/html/index.en.html
storage:
  engine: http
  http:
    profile: ecb-daily/1
    collection: daily
schemas:
  collections:
    daily:
      fields:
        time: {type: string}
        currency: {type: string}
        rate: {type: string}
`

type ecbHostConfig struct {
	Format            string                `json:"format"`
	PublisherManifest string                `json:"publisherManifest"`
	HTTPManifest      string                `json:"httpManifest"`
	PublisherCommit   string                `json:"publisherCommit"`
	PublisherBlob     string                `json:"publisherBlob"`
	DecoderVersion    string                `json:"decoderVersion"`
	DecoderModule     string                `json:"decoderModuleVersion"`
	SourceRight       license.SourceRight   `json:"sourceRight"`
	Binding           providerreads.Binding `json:"binding"`
}

var errECBCandidate = errors.New("ECB candidate configuration refused")

func readECBFile(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, errECBCandidate
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errECBCandidate
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > limit {
		return nil, errECBCandidate
	}
	return data, nil
}

func checkedECBConfig(path string) (ecbHostConfig, error) {
	data, err := readECBFile(path, 32<<10)
	if err != nil || rejectDuplicateJSON(data) != nil {
		return ecbHostConfig{}, errECBCandidate
	}
	if _, err := canonicalMembers(data, map[string]byte{
		"format": '"', "publisherManifest": '"', "httpManifest": '"',
		"publisherCommit": '"', "publisherBlob": '"',
		"decoderVersion": '"', "decoderModuleVersion": '"',
		"sourceRight": '{', "binding": '{',
	}, nil, true); err != nil {
		return ecbHostConfig{}, errECBCandidate
	}
	var config ecbHostConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF ||
		config.Format != ecbHostFormat || config.PublisherCommit != ecbPublisherCommit ||
		config.PublisherBlob != ecbPublisherBlob || config.DecoderVersion != "ecb-eurofxref/1" ||
		config.DecoderModule != "v0.3.0" {
		return ecbHostConfig{}, errECBCandidate
	}
	publisher, err := readECBFile(config.PublisherManifest, 4504)
	if err != nil || len(publisher) != 4504 || fmt.Sprintf("%x", sha256.Sum256(publisher)) != ecbPublisherSHA {
		return ecbHostConfig{}, errECBCandidate
	}
	gitBlob := append([]byte("blob "+strconv.Itoa(len(publisher))+"\x00"), publisher...)
	if fmt.Sprintf("%x", sha1.Sum(gitBlob)) != ecbPublisherBlob {
		return ecbHostConfig{}, errECBCandidate
	}
	storage, err := readECBFile(config.HTTPManifest, int64(len(ecbHTTPManifest)))
	if err != nil || !bytes.Equal(storage, []byte(ecbHTTPManifest)) {
		return ecbHostConfig{}, errECBCandidate
	}
	wantBinding := providerreads.Binding{
		ProviderSourceID: "provider:ecb/FxReferenceQuote", RightsSourceID: "ovdb:openvaultdb-cloud/ecb/daily",
		ResourceID: "ecb-daily", DefinitionDigest: ecbPublisherSHA,
		DecoderDigest: ecbDecoderSHA, RightsDigest: ecbRightsSHA,
	}
	if !reflect.DeepEqual(config.Binding, wantBinding) || len(config.SourceRight.Pins) != 1 ||
		config.SourceRight.Pins[0] != (license.Pin{Role: "provider", Repository: "https://github.com/openvaultdb/ovdb", Revision: ecbPublisherCommit, Path: "publisher/source/ecb-daily/ovdb.yaml", SHA256: ecbPublisherSHA, Bytes: 4504}) {
		return ecbHostConfig{}, errECBCandidate
	}
	rightsDigest, err := providerreads.RightsDigest(config.SourceRight)
	if err != nil || rightsDigest != ecbRightsSHA {
		return ecbHostConfig{}, errECBCandidate
	}
	return config, nil
}

type ecbMount func(string) (*core.Database, error)

func assembleECBHostCandidate(configPath string, open ecbMount, diagnostic io.Writer) (http.Handler, func() error, error) {
	config, err := checkedECBConfig(configPath)
	if err != nil {
		return nil, nil, errECBCandidate
	}
	db, err := open(config.HTTPManifest)
	if err != nil || db == nil {
		return nil, nil, errECBCandidate
	}
	fail := func() (http.Handler, func() error, error) {
		_ = db.Close()
		return nil, nil, errECBCandidate
	}
	if db.ID() != "ecb" || !db.ReadOnlyHTTP() || !db.NoRetention() || db.Manifest == nil {
		return fail()
	}
	// Recheck the mounted object, not just the path read before opening it.
	// This also rejects a manifest swap between pin verification and opening.
	expectedManifest, err := manifest.Parse([]byte(ecbHTTPManifest))
	if err != nil || !reflect.DeepEqual(db.Manifest, expectedManifest) {
		return fail()
	}
	profile := server.ProviderReadProfile{RequestProfile: server.ECBPublicFreeRequestProfile,
		Collection: "daily", SourceRight: &config.SourceRight, Binding: config.Binding}
	options := append(cloudServerOptions(), server.WithReadOnly(true),
		server.WithSourceRights("openvaultdb-cloud", nil),
		server.WithProviderReadProfiles(map[string]server.ProviderReadProfile{"ecb": profile}),
		server.WithLogger(slog.New(&ecbMarkerHandler{out: diagnostic})),
	)
	checked, err := server.NewChecked("ecb-host-candidate", map[string]*core.Database{"ecb": db}, options...)
	if err != nil {
		return fail()
	}
	return checked.Handler(), func() error { checked.CloseSnapshots(); return db.Close() }, nil
}

// The selected provider's internal errors may carry a body or row marker.
// Emit only a fixed event; no URL, query, error, header or payload reaches this sink.
type ecbMarkerHandler struct {
	mu  sync.Mutex
	out io.Writer
}

func (h *ecbMarkerHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *ecbMarkerHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelError {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, "ecb_candidate_internal_error\n")
	return err
}
func (h *ecbMarkerHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *ecbMarkerHandler) WithGroup(string) slog.Handler      { return h }
