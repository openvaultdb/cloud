package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Listus demo relay moved here when the public gateway Worker was retired.
// It stays disabled unless every binding below is present. Sessions are
// process-local because the Worker KV namespace left with that Worker.

const (
	demoSessionPrefix     = "demo:session:"
	demoSpacePrefix       = "demo:space:"
	demoOwnerPrefix       = "demo:owner:"
	demoOriginPrefix      = "demo:origin:"
	demoStableDatabaseID  = "demo-sneat-space"
	demoInternalPrefix    = "/internal/demo/sessions/"
	demoControlHeader     = "X-OVDB-Demo-Control-Secret"
	demoJWKSURL           = "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com"
	demoMaxJSON           = 64 * 1024
	demoMaxJWKS           = 64 * 1024
	demoMaxResponse       = 1 << 20
	demoOriginTimeout     = 10 * time.Second
	demoJWKSTimeout       = 3 * time.Second
	demoUnknownKidWait    = 60 * time.Second
	demoSessionLeaseLimit = time.Hour
)

var (
	demoOriginHostPattern = regexp.MustCompile(`^ovdb-demo-[a-z0-9-]+\.openvaultdb\.com$`)
	demoSessionIDPattern  = regexp.MustCompile(`^[a-z0-9-]{16,48}$`)
	demoSafeIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	demoSafeSuffixPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~-]|%[0-9A-Fa-f]{2}|/)*$`)
	demoBadEscapePattern  = regexp.MustCompile(`(?i)%2f|%5c|%2e%2e`)
	demoMaxAgePattern     = regexp.MustCompile(`(?i)(?:^|,)\s*max-age=(\d+)`)
	errDemoTooLarge       = errors.New("demo body too large")
)

type demoRelay struct {
	enabledFlag     bool
	controlSecret   string
	encryptionKey   []byte
	firebaseProject string
	corsOrigin      string
	originSuffix    string
	store           *memoryDemoStore
	now             func() time.Time
	fetchJWKS       func(*http.Request) (*http.Response, error)
	jwksClient      *http.Client
	origin          http.RoundTripper
	cache           *demoJWKSCache
	mu              sync.Mutex
}

type encryptedValue struct {
	IV         string `json:"iv"`
	Ciphertext string `json:"ciphertext"`
}

type storedSession struct {
	SessionID            string         `json:"sessionId"`
	OwnerUserID          string         `json:"ownerUserId"`
	SpaceID              string         `json:"spaceId"`
	DatabaseID           string         `json:"databaseId"`
	ExpiresAt            string         `json:"expiresAt"`
	OriginURL            string         `json:"originUrl"`
	EncryptedOriginToken encryptedValue `json:"encryptedOriginToken"`
}

type demoJWK struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type demoJWKSet struct {
	Keys []demoJWK `json:"keys"`
}

type firebaseClaims struct {
	Sub      string
	Aud      string
	Iss      string
	Exp      int64
	Iat      int64
	AuthTime int64
}

func newDemoRelay(getenv func(string) string) *demoRelay {
	relay := &demoRelay{
		enabledFlag:     getenv("OVDB_DEMO_ENABLED") == "true",
		controlSecret:   getenv("OVDB_DEMO_CONTROL_SECRET"),
		firebaseProject: getenv("OVDB_DEMO_FIREBASE_PROJECT_ID"),
		corsOrigin:      getenv("OVDB_DEMO_CORS_ORIGIN"),
		originSuffix:    strings.ToLower(strings.TrimSpace(getenv("OVDB_DEMO_ORIGIN_HOST_SUFFIX"))),
		store:           newMemoryDemoStore(),
		now:             time.Now,
		origin: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: demoOriginTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   demoOriginTimeout,
			ResponseHeaderTimeout: demoOriginTimeout,
			IdleConnTimeout:       60 * time.Second,
		},
		jwksClient: &http.Client{
			Timeout: demoJWKSTimeout,
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: demoJWKSTimeout}).DialContext,
				TLSHandshakeTimeout:   demoJWKSTimeout,
				ResponseHeaderTimeout: demoJWKSTimeout,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cache: &demoJWKSCache{kids: map[string]time.Time{}},
	}
	if key, err := decodeDemoKey(getenv("OVDB_DEMO_ENCRYPTION_KEY")); err == nil && len(key) == 32 {
		relay.encryptionKey = key
	}
	return relay
}

func decodeDemoKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("missing")
	}
	if key, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return key, nil
	}
	return base64.URLEncoding.DecodeString(raw)
}

func (d *demoRelay) configured() bool {
	return d.controlSecret != "" && len(d.encryptionKey) == 32 && d.firebaseProject != "" && d.corsOrigin != "" && d.originSuffix != "" && d.store != nil
}

func (d *demoRelay) cleanupReady() bool {
	return d.controlSecret != "" && d.store != nil
}

func (d *demoRelay) handle(w http.ResponseWriter, r *http.Request, canonical string) bool {
	host := strings.ToLower(stripHostPort(r.Host))
	if host == cloudHost && r.Method == http.MethodDelete && strings.HasPrefix(canonical, demoInternalPrefix) && d.cleanupReady() {
		d.handleInternal(w, r, canonical)
		return true
	}
	if !d.enabledFlag || !d.configured() {
		if demoFeaturePath(canonical) || potentialDemoOriginHost(host) {
			d.fail(w, r, http.StatusServiceUnavailable, "demo_unavailable")
			return true
		}
		return false
	}
	if strings.HasPrefix(canonical, demoInternalPrefix) {
		d.handleInternal(w, r, canonical)
		return true
	}
	if strings.HasPrefix(canonical, "/users/") {
		d.handleStable(w, r, canonical)
		return true
	}
	if potentialDemoOriginHost(host) {
		if id, ok := d.get(originKey(host)); ok {
			d.handleOrigin(w, r, canonical, id)
			return true
		}
		demoError(w, http.StatusServiceUnavailable, "session_unavailable")
		return true
	}
	return false
}

func demoFeaturePath(path string) bool {
	return strings.HasPrefix(path, "/internal/demo/") || strings.HasPrefix(path, "/api/demo/") || strings.HasPrefix(path, "/users/")
}

func potentialDemoOriginHost(host string) bool {
	return demoOriginHostPattern.MatchString(strings.ToLower(stripHostPort(host)))
}

func stripHostPort(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return host
}

func (d *demoRelay) handleInternal(w http.ResponseWriter, r *http.Request, canonical string) {
	sessionID := strings.TrimPrefix(canonical, demoInternalPrefix)
	if !demoSessionIDPattern.MatchString(sessionID) || strings.Contains(sessionID, "/") {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !demoSecretEqual(presentedControlSecret(r), d.controlSecret) {
		demoError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method == http.MethodDelete {
		d.mu.Lock()
		d.deleteSession(sessionID)
		d.mu.Unlock()
		demoNoContent(w)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT, DELETE")
		demoError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	payload, err := readDemoJSON(r)
	if err != nil {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	incoming, ok := d.parseIncoming(payload, sessionID)
	if !ok {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if raw, exists := d.get(sessionKey(sessionID)); exists {
		stored, parsed := parseStoredSession(raw)
		if !parsed || !d.sameSession(stored, incoming) {
			demoError(w, http.StatusConflict, "session_conflict")
			return
		}
		if !d.repairIndexes(stored) {
			demoError(w, http.StatusConflict, "session_conflict")
			return
		}
		demoNoContent(w)
		return
	}
	if replaced, exists := d.get(ownerKey(incoming.OwnerUserID)); exists && replaced != sessionID {
		d.deleteSession(replaced)
	}
	encoded, err := json.Marshal(incoming)
	if err != nil {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expiry, _ := time.Parse(time.RFC3339Nano, incoming.ExpiresAt)
	expiration := kvExpiration(expiry, d.now())
	host := originHostname(incoming.OriginURL)
	d.put(sessionKey(sessionID), string(encoded), expiration)
	d.put(spaceKey(incoming.SpaceID), sessionID, expiration)
	d.put(ownerKey(incoming.OwnerUserID), sessionID, expiration)
	d.put(originKey(host), sessionID, expiration)
	if current, exists := d.get(ownerKey(incoming.OwnerUserID)); !exists || current != sessionID {
		demoError(w, http.StatusConflict, "session_conflict")
		return
	}
	demoNoContent(w)
}

func presentedControlSecret(r *http.Request) string {
	values := r.Header.Values(demoControlHeader)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func (d *demoRelay) repairIndexes(session storedSession) bool {
	expiration := kvExpiration(mustParseDemoTime(session.ExpiresAt), d.now())
	indexes := d.sessionIndexes(session)
	for _, index := range indexes {
		current, exists := d.get(index[0])
		if exists && current != index[1] {
			return false
		}
	}
	for _, index := range indexes {
		if _, exists := d.get(index[0]); !exists {
			d.put(index[0], index[1], expiration)
		}
	}
	return true
}

func (d *demoRelay) sessionIndexes(session storedSession) [][2]string {
	return [][2]string{
		{spaceKey(session.SpaceID), session.SessionID},
		{ownerKey(session.OwnerUserID), session.SessionID},
		{originKey(originHostname(session.OriginURL)), session.SessionID},
	}
}

func (d *demoRelay) deleteSession(sessionID string) {
	raw, ok := d.get(sessionKey(sessionID))
	if !ok {
		return
	}
	session, parsed := parseStoredSession(raw)
	d.del(sessionKey(sessionID))
	if !parsed {
		return
	}
	for _, index := range d.sessionIndexes(session) {
		if current, exists := d.get(index[0]); exists && current == sessionID {
			d.del(index[0])
		}
	}
}

func (d *demoRelay) handleStable(w http.ResponseWriter, r *http.Request, canonical string) {
	owner, suffix, ok := parseStablePath(canonical)
	if !ok {
		d.fail(w, r, http.StatusNotFound, "not_found")
		return
	}
	sessionID, found := d.get(ownerKey(owner))
	if !found {
		d.fail(w, r, http.StatusNotFound, "session_unavailable")
		return
	}
	session, ok := d.authorize(w, r, sessionID)
	if !ok {
		return
	}
	if session.OwnerUserID != owner || session.DatabaseID != demoStableDatabaseID {
		d.fail(w, r, http.StatusNotFound, "not_found")
		return
	}
	d.proxy(w, r, session, suffix)
}

func (d *demoRelay) handleOrigin(w http.ResponseWriter, r *http.Request, canonical, sessionID string) {
	if !demoNativeRecord(canonical) {
		d.fail(w, r, http.StatusNotFound, "not_found")
		return
	}
	session, ok := d.authorize(w, r, sessionID)
	if !ok {
		return
	}
	d.proxy(w, r, session, canonical)
}

func (d *demoRelay) authorize(w http.ResponseWriter, r *http.Request, sessionID string) (storedSession, bool) {
	raw, ok := d.get(sessionKey(sessionID))
	session, parsed := parseStoredSession(raw)
	if !ok || !parsed {
		d.fail(w, r, http.StatusNotFound, "session_unavailable")
		return storedSession{}, false
	}
	expiry, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	if err != nil {
		d.fail(w, r, http.StatusNotFound, "session_unavailable")
		return storedSession{}, false
	}
	if !d.now().Before(expiry) {
		d.fail(w, r, http.StatusGone, "session_expired")
		return storedSession{}, false
	}
	if d.cors(w, r) {
		return storedSession{}, false
	}
	if !demoAllowedMethod(r.Method) {
		w.Header().Set("Allow", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		d.fail(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return storedSession{}, false
	}
	if !demoRequestBounded(r) {
		d.fail(w, r, http.StatusBadRequest, "invalid_request")
		return storedSession{}, false
	}
	subject, ok := d.verifyFirebase(w, r)
	if !ok {
		return storedSession{}, false
	}
	if subject != session.OwnerUserID {
		d.fail(w, r, http.StatusForbidden, "forbidden")
		return storedSession{}, false
	}
	return session, true
}

func (d *demoRelay) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin != "" && origin != d.corsOrigin {
		d.fail(w, r, http.StatusForbidden, "cors_forbidden")
		return true
	}
	if r.Method != http.MethodOptions {
		return false
	}
	d.applyCORS(w.Header(), r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func (d *demoRelay) proxy(w http.ResponseWriter, r *http.Request, session storedSession, suffix string) {
	token, ok := d.decrypt(session.EncryptedOriginToken)
	if !ok {
		d.fail(w, r, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	origin, err := url.Parse(session.OriginURL)
	if err != nil {
		d.fail(w, r, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	decoded, err := url.PathUnescape(suffix)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	origin.Path = decoded
	origin.RawPath = suffix
	origin.RawQuery = r.URL.RawQuery
	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodDelete && r.Method != http.MethodHead {
		if demoBodyTooLarge(r, demoMaxJSON) {
			d.fail(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		buf, readErr := readLimited(r.Body, demoMaxJSON)
		if readErr != nil {
			d.fail(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		body = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(r.Context(), demoOriginTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, origin.String(), body)
	if err != nil {
		d.fail(w, r, http.StatusServiceUnavailable, "origin_unavailable")
		return
	}
	req.URL = origin
	req.Host = origin.Host
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		if contentType := r.Header.Get("Content-Type"); contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	resp, err := d.origin.RoundTrip(req)
	if err != nil {
		if demoTimedOut(err) {
			d.fail(w, r, http.StatusGatewayTimeout, "origin_timeout")
			return
		}
		d.fail(w, r, http.StatusServiceUnavailable, "origin_unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		d.fail(w, r, http.StatusBadGateway, "origin_rejected")
		return
	}
	if resp.ContentLength > demoMaxResponse {
		d.fail(w, r, http.StatusBadGateway, "response_too_large")
		return
	}
	d.applyCORS(w.Header(), r)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusResetContent || resp.StatusCode == http.StatusNotModified {
		w.WriteHeader(resp.StatusCode)
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		d.fail(w, r, http.StatusBadGateway, "origin_rejected")
		return
	}
	buf, err := readLimited(resp.Body, demoMaxResponse)
	if err != nil {
		if errors.Is(err, errDemoTooLarge) {
			d.fail(w, r, http.StatusBadGateway, "response_too_large")
			return
		}
		d.fail(w, r, http.StatusServiceUnavailable, "origin_unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(buf)
}

func (d *demoRelay) parseIncoming(value any, sessionID string) (storedSession, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return storedSession{}, false
	}
	owner, ok1 := demoStringField(object, "ownerUserId")
	space, ok2 := demoStringField(object, "spaceId")
	database, ok3 := demoStringField(object, "databaseId")
	expiresAt, ok4 := demoStringField(object, "expiresAt")
	originURL, ok5 := demoStringField(object, "originUrl")
	token, ok6 := demoStringField(object, "originToken")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return storedSession{}, false
	}
	if !demoSafeIDPattern.MatchString(owner) || !demoSafeIDPattern.MatchString(space) || database != demoStableDatabaseID {
		return storedSession{}, false
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return storedSession{}, false
	}
	now := d.now()
	if !expiry.After(now) || expiry.After(now.Add(demoSessionLeaseLimit)) {
		return storedSession{}, false
	}
	origin, err := url.Parse(originURL)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Port() != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" || strings.ContainsAny(originURL, "?#") {
		return storedSession{}, false
	}
	if origin.Path != "" && origin.Path != "/" {
		return storedSession{}, false
	}
	expected := "ovdb-demo-" + sessionID + "." + d.originSuffix
	if !strings.EqualFold(origin.Hostname(), expected) {
		return storedSession{}, false
	}
	origin.Path = "/"
	origin.RawPath = ""
	encrypted, err := d.encrypt(token)
	if err != nil {
		return storedSession{}, false
	}
	return storedSession{
		SessionID: sessionID, OwnerUserID: owner, SpaceID: space, DatabaseID: database,
		ExpiresAt: expiry.UTC().Format("2006-01-02T15:04:05.000Z"), OriginURL: origin.String(),
		EncryptedOriginToken: encrypted,
	}, true
}

func parseStoredSession(raw string) (storedSession, bool) {
	var session storedSession
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return storedSession{}, false
	}
	if !demoSessionIDPattern.MatchString(session.SessionID) || !demoSafeIDPattern.MatchString(session.OwnerUserID) || !demoSafeIDPattern.MatchString(session.SpaceID) || session.DatabaseID != demoStableDatabaseID || session.ExpiresAt == "" || session.OriginURL == "" || session.EncryptedOriginToken.IV == "" || session.EncryptedOriginToken.Ciphertext == "" {
		return storedSession{}, false
	}
	return session, true
}

func (d *demoRelay) sameSession(left, right storedSession) bool {
	if left.SessionID != right.SessionID || left.OwnerUserID != right.OwnerUserID || left.SpaceID != right.SpaceID || left.DatabaseID != right.DatabaseID || left.ExpiresAt != right.ExpiresAt || left.OriginURL != right.OriginURL {
		return false
	}
	leftToken, okL := d.decrypt(left.EncryptedOriginToken)
	rightToken, okR := d.decrypt(right.EncryptedOriginToken)
	return okL && okR && subtle.ConstantTimeCompare([]byte(leftToken), []byte(rightToken)) == 1
}

func (d *demoRelay) verifyFirebase(w http.ResponseWriter, r *http.Request) (string, bool) {
	unauthorized := func() (string, bool) {
		d.fail(w, r, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	token, ok := demoBearer(r.Header.Get("Authorization"))
	if !ok {
		return unauthorized()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return unauthorized()
	}
	header, claims, err := decodeFirebase(parts[0], parts[1])
	if err != nil || header.Alg != "RS256" || header.Kid == "" {
		return unauthorized()
	}
	now := d.now().Unix()
	project := d.firebaseProject
	if claims.Aud != project || claims.Iss != "https://securetoken.google.com/"+project || !demoSafeIDPattern.MatchString(claims.Sub) || !validFirebaseTimes(claims, now) {
		return unauthorized()
	}
	keys, err := d.loadJWKS(r, false)
	if err != nil {
		return unauthorized()
	}
	jwk := findDemoJWK(keys, header.Kid)
	if jwk == nil && d.cache.claimKid(header.Kid, d.now()) {
		keys, err = d.loadJWKS(r, true)
		if err != nil {
			return unauthorized()
		}
		jwk = findDemoJWK(keys, header.Kid)
		if jwk != nil {
			d.cache.clearKid(header.Kid)
		}
	}
	if jwk == nil {
		return unauthorized()
	}
	publicKey, err := jwk.publicKey()
	if err != nil {
		return unauthorized()
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return unauthorized()
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, sum[:], signature) != nil {
		return unauthorized()
	}
	return claims.Sub, true
}

func (d *demoRelay) loadJWKS(r *http.Request, refresh bool) (demoJWKSet, error) {
	if !refresh {
		if set, ok := d.cache.get(d.now()); ok {
			return set, nil
		}
	}
	var resp *http.Response
	var err error
	if d.fetchJWKS != nil {
		resp, err = d.fetchJWKS(r)
	} else {
		req, reqErr := http.NewRequestWithContext(r.Context(), http.MethodGet, demoJWKSURL, nil)
		if reqErr != nil {
			return demoJWKSet{}, reqErr
		}
		resp, err = d.jwksClient.Do(req)
	}
	if err != nil {
		return demoJWKSet{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.ContentLength > demoMaxJWKS {
		return demoJWKSet{}, errors.New("keys unavailable")
	}
	body, err := readLimited(resp.Body, demoMaxJWKS)
	if err != nil {
		return demoJWKSet{}, err
	}
	var set demoJWKSet
	if json.Unmarshal(body, &set) != nil || set.Keys == nil || len(set.Keys) > 32 {
		return demoJWKSet{}, errors.New("invalid keys")
	}
	if age := cacheMaxAge(resp.Header.Get("Cache-Control")); age > 0 {
		d.cache.put(set, d.now().Add(time.Duration(age)*time.Second))
	}
	return set, nil
}

func findDemoJWK(set demoJWKSet, kid string) *demoJWK {
	for i := range set.Keys {
		key := &set.Keys[i]
		if key.Kid == kid && key.Kty == "RSA" && key.Alg == "RS256" && key.Use == "sig" {
			return key
		}
	}
	return nil
}

func (k demoJWK) publicKey() (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(modulus) == 0 {
		return nil, errors.New("modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return nil, errors.New("exponent")
	}
	exponent := 0
	for _, b := range exponentBytes {
		exponent = exponent<<8 | int(b)
	}
	if exponent < 2 {
		return nil, errors.New("exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}, nil
}

func decodeFirebase(headerPart, payloadPart string) (struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}, firebaseClaims, error) {
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(headerPart)
	if err != nil {
		return header, firebaseClaims{}, err
	}
	if json.Unmarshal(headerJSON, &header) != nil {
		return header, firebaseClaims{}, errors.New("header")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return header, firebaseClaims{}, err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(payloadJSON, &raw) != nil {
		return header, firebaseClaims{}, errors.New("payload")
	}
	var claims firebaseClaims
	if json.Unmarshal(raw["sub"], &claims.Sub) != nil || json.Unmarshal(raw["aud"], &claims.Aud) != nil || json.Unmarshal(raw["iss"], &claims.Iss) != nil {
		return header, firebaseClaims{}, errors.New("claims")
	}
	claims.Exp, err = numericDate(raw["exp"])
	if err != nil {
		return header, firebaseClaims{}, err
	}
	claims.Iat, err = numericDate(raw["iat"])
	if err != nil {
		return header, firebaseClaims{}, err
	}
	claims.AuthTime, err = numericDate(raw["auth_time"])
	if err != nil {
		return header, firebaseClaims{}, err
	}
	return header, claims, nil
}

func numericDate(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return 0, err
	}
	if strings.ContainsAny(num.String(), ".eE") {
		return 0, errors.New("fraction")
	}
	value, err := num.Int64()
	if err != nil || value <= 0 {
		return 0, errors.New("range")
	}
	return value, nil
}

func validFirebaseTimes(claims firebaseClaims, now int64) bool {
	if claims.Exp <= 0 || claims.Iat <= 0 || claims.AuthTime <= 0 {
		return false
	}
	if claims.Exp <= now || claims.Exp <= claims.Iat || claims.Exp-claims.Iat > 3600 {
		return false
	}
	if claims.Iat > now+60 || claims.AuthTime > now+60 {
		return false
	}
	return true
}

func (d *demoRelay) encrypt(plaintext string) (encryptedValue, error) {
	block, err := aes.NewCipher(d.encryptionKey)
	if err != nil {
		return encryptedValue{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedValue{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return encryptedValue{}, err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(plaintext), nil)
	return encryptedValue{IV: base64.RawURLEncoding.EncodeToString(nonce), Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext)}, nil
}

func (d *demoRelay) decrypt(value encryptedValue) (string, bool) {
	block, err := aes.NewCipher(d.encryptionKey)
	if err != nil {
		return "", false
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(value.IV)
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", false
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(value.Ciphertext)
	if err != nil {
		return "", false
	}
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

func (d *demoRelay) applyCORS(h http.Header, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin == d.corsOrigin {
		h.Set("Access-Control-Allow-Origin", d.corsOrigin)
		h.Set("Vary", "Origin")
	}
}

func (d *demoRelay) fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	d.applyCORS(w.Header(), r)
	demoError(w, status, code)
}

func demoError(w http.ResponseWriter, status int, code string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Referrer-Policy") == "" {
		h.Set("Referrer-Policy", "no-referrer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func demoNoContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func demoSecretEqual(left, right string) bool {
	sumLeft := sha256.Sum256([]byte(left))
	sumRight := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(sumLeft[:], sumRight[:]) == 1
}

func demoStringField(input map[string]any, field string) (string, bool) {
	value, ok := input[field].(string)
	return value, ok && value != "" && len(value) <= 4096
}

func demoAllowedMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func demoRequestBounded(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return true
	}
	return mediaType(r.Header.Get("Content-Type")) == "application/json" && !demoBodyTooLarge(r, demoMaxJSON)
}

func mediaType(value string) string {
	if i := strings.Index(value, ";"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

func demoBodyTooLarge(r *http.Request, limit int64) bool {
	if r.ContentLength > limit {
		return true
	}
	raw := strings.TrimSpace(r.Header.Get("Content-Length"))
	if raw == "" {
		return false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && n > limit
}

func readDemoJSON(r *http.Request) (any, error) {
	if mediaType(r.Header.Get("Content-Type")) != "application/json" || demoBodyTooLarge(r, demoMaxJSON) {
		return nil, errors.New("invalid")
	}
	buf, err := readLimited(r.Body, demoMaxJSON)
	if err != nil {
		return nil, err
	}
	var value any
	if err = json.Unmarshal(buf, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func readLimited(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, errDemoTooLarge
	}
	return buf, nil
}

func demoBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, prefix)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func parseStablePath(pathname string) (string, string, bool) {
	const prefix = "/users/"
	const marker = "/demo-sneat-space/ovdb"
	if !strings.HasPrefix(pathname, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(pathname, prefix)
	index := strings.Index(rest, marker)
	if index < 0 {
		return "", "", false
	}
	owner := rest[:index]
	suffix := rest[index+len(marker):]
	if !demoSafeIDPattern.MatchString(owner) || !demoNativeRecord(suffix) || !demoSafeSuffix(suffix) {
		return "", "", false
	}
	return owner, suffix, true
}

func demoNativeRecord(pathname string) bool {
	return strings.HasPrefix(pathname, "/v1/databases/"+demoStableDatabaseID+"/records/")
}

func demoSafeSuffix(value string) bool {
	return demoSafeSuffixPattern.MatchString(value) && !demoBadEscapePattern.MatchString(value) && !strings.Contains(value, "//")
}

func cacheMaxAge(header string) int {
	match := demoMaxAgePattern.FindStringSubmatch(header)
	if match == nil {
		return 0
	}
	age, err := strconv.Atoi(match[1])
	if err != nil || age <= 0 {
		return 0
	}
	if age > 3600 {
		return 3600
	}
	return age
}

func demoTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func kvExpiration(expiresAt, now time.Time) int64 {
	exp := expiresAt.Unix()
	min := now.Unix() + 60
	if exp < min {
		return min
	}
	return exp
}

func mustParseDemoTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func originHostname(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func sessionKey(id string) string { return demoSessionPrefix + id }
func spaceKey(id string) string   { return demoSpacePrefix + id }
func ownerKey(id string) string   { return demoOwnerPrefix + id }
func originKey(host string) string {
	return demoOriginPrefix + host
}

func (d *demoRelay) get(key string) (string, bool) {
	if d.store == nil {
		return "", false
	}
	return d.store.get(key, d.now())
}

func (d *demoRelay) put(key, value string, expiration int64) {
	d.store.put(key, value, expiration, d.now())
}

func (d *demoRelay) del(key string) { d.store.delete(key) }

type demoRecord struct {
	value      string
	expiration int64
}

type memoryDemoStore struct {
	mu     sync.Mutex
	values map[string]demoRecord
}

func newMemoryDemoStore() *memoryDemoStore {
	return &memoryDemoStore{values: map[string]demoRecord{}}
}

func (s *memoryDemoStore) get(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.values[key]
	if !ok {
		return "", false
	}
	if rec.expiration > 0 && now.Unix() >= rec.expiration {
		delete(s.values, key)
		return "", false
	}
	return rec.value, true
}

func (s *memoryDemoStore) put(key, value string, expiration int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, rec := range s.values {
		if rec.expiration > 0 && now.Unix() >= rec.expiration {
			delete(s.values, existing)
		}
	}
	s.values[key] = demoRecord{value: value, expiration: expiration}
}

func (s *memoryDemoStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
}

func (s *memoryDemoStore) expiration(key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.values[key]
	if !ok {
		return 0, false
	}
	return rec.expiration, true
}

func (s *memoryDemoStore) count(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rec := range s.values {
		if rec.expiration == 0 || now.Unix() < rec.expiration {
			n++
		}
	}
	return n
}

type demoJWKSCache struct {
	mu      sync.Mutex
	set     demoJWKSet
	expires time.Time
	has     bool
	kids    map[string]time.Time
}

func (c *demoJWKSCache) get(now time.Time) (demoJWKSet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.has || !now.Before(c.expires) {
		return demoJWKSet{}, false
	}
	return c.set, true
}

func (c *demoJWKSCache) put(set demoJWKSet, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = set
	c.expires = expires
	c.has = true
}

func (c *demoJWKSCache) claimKid(kid string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.kids[kid]; ok && now.Before(exp) {
		return false
	}
	c.kids[kid] = now.Add(demoUnknownKidWait)
	return true
}

func (c *demoJWKSCache) clearKid(kid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.kids, kid)
}

func demoAPIPath(path string) bool {
	return path == "/api/demo/sessions" || path == "/api/demo/session" || path == "/api/demo/session/browser" || strings.HasPrefix(path, "/api/demo/sessions/")
}

func (g *gateway) serveDemoAPI(w http.ResponseWriter, r *http.Request, path string) {
	if g.listus == nil || !g.listus.enabledFlag || !g.listus.configured() {
		demoError(w, http.StatusServiceUnavailable, "demo_unavailable")
		return
	}
	switch path {
	case "/api/demo/session/browser":
		g.serveDemoBrowser(w, r)
	case "/api/demo/sessions":
		g.serveDemoCreate(w, r)
	case "/api/demo/session":
		g.serveDemoMetadata(w, r)
	default:
		g.serveDemoEnd(w, r, path)
	}
}

func (g *gateway) serveDemoBrowser(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && origin != g.listus.corsOrigin {
		demoError(w, http.StatusForbidden, "cors_forbidden")
		return
	}
	if r.Method == http.MethodOptions {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", g.listus.corsOrigin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		h.Set("Cache-Control", "no-store")
		h.Set("Vary", "Origin")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		gatewayError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	search, ok := demoSessionSearch(r)
	if !ok {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	g.proxyDemo(w, r, http.MethodGet, "/v0/ovdb/demo/session/browser", search, true)
}

func (g *gateway) serveDemoMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		gatewayError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	search, ok := demoSessionSearch(r)
	if !ok {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	g.proxyDemo(w, r, http.MethodGet, "/v0/ovdb/demo/session", search, false)
}

func (g *gateway) serveDemoCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		gatewayError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !g.admit(r, "device:/v0/ovdb/demo/sessions", 60) {
		demoRateLimited(w)
		return
	}
	g.proxyDemo(w, r, http.MethodPost, "/v0/ovdb/demo/sessions", r.URL.RawQuery, true)
}

func (g *gateway) serveDemoEnd(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		gatewayError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	sessionID := strings.TrimPrefix(path, "/api/demo/sessions/")
	if !gatewayDatabaseID.MatchString(sessionID) {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	payload, err := json.Marshal(map[string]string{"sessionId": sessionID})
	if err != nil {
		demoError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !gatewayReadBody(w, r) {
		return
	}
	r.Method = http.MethodPost
	r.Body = io.NopCloser(bytes.NewReader(payload))
	r.ContentLength = int64(len(payload))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	r.Header.Set("Content-Type", "application/json")
	g.proxyDemoPrepared(w, r, "/v0/ovdb/demo/session/end", r.URL.RawQuery, true)
}

func demoSessionSearch(r *http.Request) (string, bool) {
	values := r.URL.Query()["spaceId"]
	if len(values) != 1 || !gatewayDatabaseID.MatchString(values[0]) {
		return "", false
	}
	return url.Values{"spaceId": {values[0]}}.Encode(), true
}

func (g *gateway) proxyDemo(w http.ResponseWriter, r *http.Request, method, upstream, rawQuery string, cors bool) {
	if !gatewayReadBody(w, r) {
		return
	}
	r.Method = method
	if method == http.MethodGet || method == http.MethodHead {
		r.Body = http.NoBody
		r.ContentLength = 0
		r.Header.Del("Content-Type")
	}
	if r.Header.Get("Accept") == "" {
		r.Header.Set("Accept", "application/json")
	}
	g.proxyDemoPrepared(w, r, upstream, rawQuery, cors)
}

func (g *gateway) proxyDemoPrepared(w http.ResponseWriter, r *http.Request, upstream, rawQuery string, cors bool) {
	if g.proxy == nil {
		gatewayError(w, http.StatusServiceUnavailable, "Authorization is not configured")
		return
	}
	if r.Header.Get("Accept") == "" {
		r.Header.Set("Accept", "application/json")
	}
	r.URL.Path = upstream
	r.URL.RawPath = ""
	r.URL.RawQuery = rawQuery
	if cors {
		w = &demoControlCORSWriter{ResponseWriter: w, origin: r.Header.Get("Origin"), allowed: g.listus.corsOrigin}
	}
	g.proxy.ServeHTTP(w, r)
}

func demoRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many attempts. Try again in one minute."})
}

type demoControlCORSWriter struct {
	http.ResponseWriter
	origin  string
	allowed string
	wrote   bool
}

func (w *demoControlCORSWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if w.origin != "" && w.origin == w.allowed {
		w.Header().Set("Access-Control-Allow-Origin", w.allowed)
		if w.Header().Get("Vary") == "" {
			w.Header().Set("Vary", "Origin")
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *demoControlCORSWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *demoControlCORSWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
