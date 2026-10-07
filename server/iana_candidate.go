package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

var errIANAConfig = errors.New("IANA operator configuration refused")

// This is a transport declaration, not a publisher response or rights approval.
const ianaHTTPManifest = `database:
  id: iana-http-status
  schema_mode: strict
  retention: none
  license:
    name: IANA protocol registry licensing terms
    url: https://www.iana.org/help/licensing-terms
storage:
  engine: http
  http:
    profile: iana-http-status/1
    collection: rows
schemas:
  collections:
    rows:
      fields:
        Value: {type: string}
        Description: {type: string}
        Reference: {type: string}
`

type ianaAdmission struct {
	Format               string `json:"format"`
	Decision             string `json:"decision"`
	ApprovedBy           string `json:"approvedBy"`
	ApprovedAt           string `json:"approvedAt"`
	HostConfigSHA256     string `json:"hostConfigSHA256"`
	DefinitionSHA256     string `json:"definitionSHA256"`
	DecoderSHA256        string `json:"decoderSHA256"`
	RightsSHA256         string `json:"rightsSHA256"`
	ExecutorID           string `json:"executorId"`
	ResourceID           string `json:"resourceId"`
	Method               string `json:"method"`
	Path                 string `json:"path"`
	MaxReadsPerExecution int    `json:"maxReadsPerExecution"`
	MaxRows              int    `json:"maxRows"`
	PublicAccess         bool   `json:"publicAccess"`
	PaidAccess           bool   `json:"paidAccess"`
}

type ianaHostConfig struct {
	Format               string                `json:"format"`
	PublisherDefinition  string                `json:"publisherDefinition"`
	HTTPManifest         string                `json:"httpManifest"`
	DecoderVersion       string                `json:"decoderVersion"`
	DecoderModuleVersion string                `json:"decoderModuleVersion"`
	SourceRight          license.SourceRight   `json:"sourceRight"`
	Binding              providerreads.Binding `json:"binding"`
}

func isSHA256(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func decodeIANAFile(path, digest string, limit int64, members map[string]byte, out any) error {
	data, err := readECBFile(path, limit)
	if err != nil || !isSHA256(digest) || fmt.Sprintf("%x", sha256.Sum256(data)) != digest || rejectDuplicateJSON(data) != nil {
		return errIANAConfig
	}
	if _, err := canonicalMembers(data, members, nil, true); err != nil {
		return errIANAConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return errIANAConfig
	}
	return nil
}

func checkedIANAAdmission(path, digest string) (ianaAdmission, error) {
	var a ianaAdmission
	err := decodeIANAFile(path, digest, 4096, map[string]byte{
		"format": '"', "decision": '"', "approvedBy": '"', "approvedAt": '"',
		"hostConfigSHA256": '"', "definitionSHA256": '"', "decoderSHA256": '"', "rightsSHA256": '"',
		"executorId": '"', "resourceId": '"', "method": '"', "path": '"',
		"maxReadsPerExecution": 'n', "maxRows": 'n', "publicAccess": 'b', "paidAccess": 'b',
	}, &a)
	approvedAt, dateErr := time.Parse(time.RFC3339, a.ApprovedAt)
	if err != nil || dateErr != nil || approvedAt.IsZero() || strings.TrimSpace(a.ApprovedBy) == "" || strings.TrimSpace(a.ApprovedBy) != a.ApprovedBy ||
		a.Format != "ovdb-iana-operator-admission/1" || a.Decision != "operator-free-transient-read-only" ||
		!isSHA256(a.HostConfigSHA256) || !isSHA256(a.DefinitionSHA256) || !isSHA256(a.DecoderSHA256) || !isSHA256(a.RightsSHA256) ||
		a.ExecutorID != "openvaultdb-cloud" || a.ResourceID != "iana-http-status-codes" || a.Method != http.MethodPost || a.Path != ianaQueryPath ||
		a.MaxReadsPerExecution != 1 || a.MaxRows != 50 || a.PublicAccess || a.PaidAccess {
		return ianaAdmission{}, errIANAConfig
	}
	return a, nil
}

func assembleIANAHost(path string, admission ianaAdmission, open ecbMount, diagnostic io.Writer) (http.Handler, func() error, error) {
	var config ianaHostConfig
	if decodeIANAFile(path, admission.HostConfigSHA256, 32<<10, map[string]byte{
		"format": '"', "publisherDefinition": '"', "httpManifest": '"', "decoderVersion": '"', "decoderModuleVersion": '"',
		"sourceRight": '{', "binding": '{',
	}, &config) != nil || config.Format != "ovdb-iana-host-candidate/1" || config.DecoderVersion != "strict-csv-three-column/1" || config.DecoderModuleVersion != "v0.4.0" {
		return nil, nil, errIANAConfig
	}
	want := providerreads.Binding{ProviderSourceID: "provider:iana/HttpStatusRegistryRow", RightsSourceID: "ovdb:openvaultdb-cloud/iana-http-status/rows",
		ResourceID: "iana-http-status-codes", DefinitionDigest: admission.DefinitionSHA256, DecoderDigest: admission.DecoderSHA256, RightsDigest: admission.RightsSHA256}
	rightsDigest, err := providerreads.RightsDigest(config.SourceRight)
	if err != nil || config.Binding != want || rightsDigest != admission.RightsSHA256 || len(config.SourceRight.Pins) != 1 {
		return nil, nil, errIANAConfig
	}
	// Operator pins cover definition metadata, never a saved upstream CSV response.
	pin := config.SourceRight.Pins[0]
	definition, err := readECBFile(config.PublisherDefinition, 32<<10)
	if err != nil || pin.Role != "provider" || pin.SHA256 != admission.DefinitionSHA256 || pin.Bytes != int64(len(definition)) || fmt.Sprintf("%x", sha256.Sum256(definition)) != admission.DefinitionSHA256 {
		return nil, nil, errIANAConfig
	}
	transport, err := readECBFile(config.HTTPManifest, int64(len(ianaHTTPManifest)))
	if err != nil || !bytes.Equal(transport, []byte(ianaHTTPManifest)) {
		return nil, nil, errIANAConfig
	}
	db, err := open(config.HTTPManifest)
	if err != nil || db == nil {
		return nil, nil, errIANAConfig
	}
	fail := func() (http.Handler, func() error, error) { _ = db.Close(); return nil, nil, errIANAConfig }
	expected, err := manifest.Parse([]byte(ianaHTTPManifest))
	if err != nil || db.ID() != "iana-http-status" || !db.ReadOnlyHTTP() || !db.NoRetention() || !reflect.DeepEqual(db.Manifest, expected) {
		return fail()
	}
	profile := server.ProviderReadProfile{RequestProfile: server.IANANativeOperatorRequestProfile, Collection: "rows", SourceRight: &config.SourceRight, Binding: config.Binding}
	options := append(cloudServerOptions(), server.WithReadOnly(true), server.WithSourceRights("openvaultdb-cloud", nil),
		server.WithProviderReadProfiles(map[string]server.ProviderReadProfile{"iana-http-status": profile}), server.WithLogger(slog.New(&ianaMarkerHandler{out: diagnostic})))
	checked, err := server.NewChecked("iana-host-candidate", map[string]*core.Database{"iana-http-status": db}, options...)
	if err != nil {
		return fail()
	}
	return checked.Handler(), func() error {
		checked.CloseSnapshots()
		if db.Close() != nil {
			return errIANAConfig
		}
		return nil
	}, nil
}

type ianaMarkerHandler struct {
	mu  sync.Mutex
	out io.Writer
}

func (h *ianaMarkerHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *ianaMarkerHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelError {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, "iana_candidate_internal_error\n")
	return err
}
func (h *ianaMarkerHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *ianaMarkerHandler) WithGroup(string) slog.Handler      { return h }
