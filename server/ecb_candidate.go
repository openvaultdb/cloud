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
	"strings"
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The candidate can be selected only through the separate operator admission
// and host configuration; the preparatory OVDB proposal is never a runtime input.
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

// The admission is an operator-owned decision, separate from the publisher's
// blocked proposal and from the transport manifest. Its digest is supplied
// independently by the selected service configuration.
type ecbAdmission struct {
	Format                  string `json:"format"`
	Decision                string `json:"decision"`
	ApprovedBy              string `json:"approvedBy"`
	ApprovedAt              string `json:"approvedAt"`
	HostConfigSHA256        string `json:"hostConfigSHA256"`
	PublisherManifestSHA256 string `json:"publisherManifestSHA256"`
	RightsDigest            string `json:"rightsDigest"`
	ExecutorID              string `json:"executorId"`
	ResourceID              string `json:"resourceId"`
	Method                  string `json:"method"`
	Path                    string `json:"path"`
	MaxReadsPerExecution    int    `json:"maxReadsPerExecution"`
	MaxRows                 int    `json:"maxRows"`
	PublicAccess            bool   `json:"publicAccess"`
	PaidAccess              bool   `json:"paidAccess"`
}

var errECBCandidate = errors.New("ECB candidate configuration refused")

func checkedECBAdmission(path, digest, configPath string) (string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", errECBCandidate
	}
	data, err := readECBFile(path, 4096)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != digest || rejectDuplicateJSON(data) != nil {
		return "", errECBCandidate
	}
	if _, err := canonicalMembers(data, map[string]byte{
		"format": '"', "decision": '"', "approvedBy": '"', "approvedAt": '"',
		"hostConfigSHA256": '"', "publisherManifestSHA256": '"', "rightsDigest": '"',
		"executorId": '"', "resourceId": '"', "method": '"', "path": '"',
		"maxReadsPerExecution": 'n', "maxRows": 'n', "publicAccess": 'b', "paidAccess": 'b',
	}, nil, true); err != nil {
		return "", errECBCandidate
	}
	var admission ecbAdmission
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&admission) != nil || decoder.Decode(new(any)) != io.EOF {
		return "", errECBCandidate
	}
	approvedAt, err := time.Parse(time.RFC3339, admission.ApprovedAt)
	if err != nil || approvedAt.IsZero() || admission.ApprovedBy == "" || strings.TrimSpace(admission.ApprovedBy) != admission.ApprovedBy {
		return "", errECBCandidate
	}
	config, err := readECBFile(configPath, 32<<10)
	if err != nil || admission.HostConfigSHA256 != fmt.Sprintf("%x", sha256.Sum256(config)) ||
		admission.Format != "ovdb-ecb-b1-operator-admission/1" ||
		admission.Decision != "operator-free-transient-read-only" ||
		admission.PublisherManifestSHA256 != ecbPublisherSHA || admission.RightsDigest != ecbRightsSHA ||
		admission.ExecutorID != "openvaultdb-cloud" || admission.ResourceID != "ecb-daily" ||
		admission.Method != http.MethodPost || admission.Path != ecbQueryPath ||
		admission.MaxReadsPerExecution != 1 || admission.MaxRows != 50 ||
		admission.PublicAccess || admission.PaidAccess {
		return "", errECBCandidate
	}
	return admission.HostConfigSHA256, nil
}

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

func checkedECBConfig(path string, expectedDigest ...string) (ecbHostConfig, error) {
	return checkedECBConfigVersion(path, false, expectedDigest...)
}

func checkedECBConfigVersion(path string, public bool, expectedDigest ...string) (ecbHostConfig, error) {
	data, err := readECBFile(path, 32<<10)
	if err != nil || rejectDuplicateJSON(data) != nil {
		return ecbHostConfig{}, errECBCandidate
	}
	if len(expectedDigest) > 1 || (len(expectedDigest) == 1 && fmt.Sprintf("%x", sha256.Sum256(data)) != expectedDigest[0]) {
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
		config.DecoderModule != map[bool]string{false: "v0.3.0", true: "v0.4.0"}[public] {
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

func assembleECBHostCandidate(configPath string, open ecbMount, diagnostic io.Writer, expectedDigest ...string) (http.Handler, func() error, error) {
	return assembleECBHostVersion(configPath, open, diagnostic, false, expectedDigest...)
}

func assembleECBHostVersion(configPath string, open ecbMount, diagnostic io.Writer, public bool, expectedDigest ...string) (http.Handler, func() error, error) {
	config, err := checkedECBConfigVersion(configPath, public, expectedDigest...)
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
