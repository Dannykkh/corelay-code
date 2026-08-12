package capabilityprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	CurrentTargetSchemaVersion = 1
	maxIdentityFieldBytes      = 256
	maxEndpointBytes           = 8 << 10
	maxServingParametersBytes  = 64 << 10
)

// TargetSpec is an ephemeral input. Endpoint, APIKey, and serving parameter
// values are never retained by TargetIdentity. APIKey exists so callers can
// pass one registration object without accidentally making credentials part
// of profile identity.
type TargetSpec struct {
	Provider          string
	Model             string
	Endpoint          string
	APIKey            string
	ServingParameters map[string]any
}

// TargetSnapshot is safe to persist. It contains only the public provider and
// model labels plus one-way digests of endpoint and serving parameters.
type TargetSnapshot struct {
	SchemaVersion           int    `json:"schemaVersion"`
	Provider                string `json:"provider"`
	Model                   string `json:"model"`
	EndpointDigest          string `json:"endpointDigest"`
	ServingParametersDigest string `json:"servingParametersDigest"`
	TargetDigest            string `json:"targetDigest"`
}

// TargetIdentity is immutable after construction; no raw endpoint,
// credential, or serving parameter is stored in it.
type TargetIdentity struct {
	provider                string
	model                   string
	endpointDigest          string
	servingParametersDigest string
	targetDigest            string
}

func NewTargetIdentity(spec TargetSpec) (TargetIdentity, error) {
	provider, err := normalizeIdentityField("provider", spec.Provider)
	if err != nil {
		return TargetIdentity{}, err
	}
	model, err := normalizeIdentityField("model", spec.Model)
	if err != nil {
		return TargetIdentity{}, err
	}

	endpoint := strings.TrimSpace(spec.Endpoint)
	if len(endpoint) > maxEndpointBytes || !utf8.ValidString(endpoint) || containsControl(endpoint) {
		return TargetIdentity{}, fmt.Errorf("%w: endpoint is not bounded printable text", ErrInvalidTarget)
	}
	parameters, err := canonicalServingParameters(spec.ServingParameters)
	if err != nil {
		return TargetIdentity{}, err
	}
	endpointSum := sha256.Sum256([]byte(endpoint))
	parameterSum := sha256.Sum256(parameters)

	target := TargetIdentity{
		provider:                provider,
		model:                   model,
		endpointDigest:          hex.EncodeToString(endpointSum[:]),
		servingParametersDigest: hex.EncodeToString(parameterSum[:]),
	}
	target.targetDigest = digestTarget(target)
	return target, nil
}

func targetFromSnapshot(snapshot TargetSnapshot) (TargetIdentity, error) {
	if snapshot.SchemaVersion != CurrentTargetSchemaVersion {
		return TargetIdentity{}, ErrSchemaMismatch
	}
	provider, err := normalizeIdentityField("provider", snapshot.Provider)
	if err != nil {
		return TargetIdentity{}, err
	}
	model, err := normalizeIdentityField("model", snapshot.Model)
	if err != nil {
		return TargetIdentity{}, err
	}
	if !validDigest(snapshot.EndpointDigest) || !validDigest(snapshot.ServingParametersDigest) || !validDigest(snapshot.TargetDigest) {
		return TargetIdentity{}, fmt.Errorf("%w: malformed target digest", ErrInvalidTarget)
	}
	target := TargetIdentity{
		provider:                provider,
		model:                   model,
		endpointDigest:          snapshot.EndpointDigest,
		servingParametersDigest: snapshot.ServingParametersDigest,
		targetDigest:            snapshot.TargetDigest,
	}
	if digestTarget(target) != snapshot.TargetDigest {
		return TargetIdentity{}, ErrTargetMismatch
	}
	return target, nil
}

func (t TargetIdentity) Valid() bool {
	if t.provider == "" || t.model == "" || !validDigest(t.endpointDigest) || !validDigest(t.servingParametersDigest) || !validDigest(t.targetDigest) {
		return false
	}
	return digestTarget(t) == t.targetDigest
}

func (t TargetIdentity) Provider() string                { return t.provider }
func (t TargetIdentity) Model() string                   { return t.model }
func (t TargetIdentity) EndpointDigest() string          { return t.endpointDigest }
func (t TargetIdentity) ServingParametersDigest() string { return t.servingParametersDigest }
func (t TargetIdentity) Digest() string                  { return t.targetDigest }

func (t TargetIdentity) Snapshot() TargetSnapshot {
	return TargetSnapshot{
		SchemaVersion:           CurrentTargetSchemaVersion,
		Provider:                t.provider,
		Model:                   t.model,
		EndpointDigest:          t.endpointDigest,
		ServingParametersDigest: t.servingParametersDigest,
		TargetDigest:            t.targetDigest,
	}
}

func digestTarget(t TargetIdentity) string {
	payload := struct {
		SchemaVersion           int    `json:"schemaVersion"`
		Provider                string `json:"provider"`
		Model                   string `json:"model"`
		EndpointDigest          string `json:"endpointDigest"`
		ServingParametersDigest string `json:"servingParametersDigest"`
	}{
		SchemaVersion:           CurrentTargetSchemaVersion,
		Provider:                t.provider,
		Model:                   t.model,
		EndpointDigest:          t.endpointDigest,
		ServingParametersDigest: t.servingParametersDigest,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func normalizeIdentityField(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxIdentityFieldBytes || !utf8.ValidString(value) || containsControl(value) || credentialLikeIdentityLabel(value) {
		return "", fmt.Errorf("%w: %s must be a bounded printable value", ErrInvalidTarget, name)
	}
	return value, nil
}

func canonicalServingParameters(parameters map[string]any) ([]byte, error) {
	if parameters == nil {
		return []byte("{}"), nil
	}
	if err := validateParameterValue(reflect.ValueOf(parameters), 0); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return nil, fmt.Errorf("%w: serving parameters are not canonical JSON", ErrInvalidTarget)
	}
	if len(encoded) > maxServingParametersBytes {
		return nil, fmt.Errorf("%w: serving parameters exceed maximum size", ErrInvalidTarget)
	}
	return encoded, nil
}

func validateParameterValue(value reflect.Value, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%w: serving parameters exceed maximum depth", ErrInvalidTarget)
	}
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return validateParameterValue(value.Elem(), depth+1)
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%w: serving parameter keys must be strings", ErrInvalidTarget)
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, key := range keys {
			if key.String() == "" || len(key.String()) > maxIdentityFieldBytes || !utf8.ValidString(key.String()) || containsControl(key.String()) {
				return fmt.Errorf("%w: serving parameter keys must be bounded printable strings", ErrInvalidTarget)
			}
			if isSensitiveParameterKey(key.String()) {
				return fmt.Errorf("%w: credential-like serving parameter key is not allowed", ErrInvalidTarget)
			}
			if err := validateParameterValue(value.MapIndex(key), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := validateParameterValue(value.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// Strict JSON primitive values are allowed.
	case reflect.Float32, reflect.Float64:
		floating := value.Float()
		if math.IsNaN(floating) || math.IsInf(floating, 0) {
			return fmt.Errorf("%w: serving parameters contain a non-finite number", ErrInvalidTarget)
		}
	default:
		return fmt.Errorf("%w: unsupported serving parameter type", ErrInvalidTarget)
	}
	return nil
}

func isSensitiveParameterKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.TrimSpace(key)))
	switch normalized {
	case "apikey", "authorization", "credential", "credentials", "password", "passwd",
		"privatekey", "secret", "clientsecret", "token", "apitoken", "authtoken",
		"accesstoken", "refreshtoken", "bearertoken", "idtoken":
		return true
	}
	return false
}

var endpointLikeLabel = regexp.MustCompile(`(?i)(://|[?#=]|\b(?:localhost|127\.0\.0\.1|\d{1,3}(?:\.\d{1,3}){3})[:/])`)

func credentialLikeIdentityLabel(value string) bool {
	return endpointLikeLabel.MatchString(value) || secretLikeText.MatchString(value) ||
		strings.HasPrefix(strings.ToLower(value), "sk-") || looksLikeJWT(value)
}

func looksLikeJWT(value string) bool {
	parts := strings.Split(value, ".")
	return len(parts) == 3 && len(parts[0]) >= 8 && len(parts[1]) >= 8 && len(parts[2]) >= 8
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}
