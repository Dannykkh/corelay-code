package capabilityprofile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func verifiedProfile(t *testing.T, target TargetIdentity, ttl time.Duration) CapabilityProfile {
	t.Helper()
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, &fakeExecutor{}, ttl).Run(contextBackground(), target, testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Verified() {
		t.Fatalf("test profile not verified: %v", profile.QuarantineReasons())
	}
	return profile
}

// contextBackground is kept local so store tests do not need fixture-specific
// context cancellation behavior.
func contextBackground() context.Context { return context.Background() }

func quarantinedProfile(t *testing.T, target TargetIdentity, ttl time.Duration) CapabilityProfile {
	t.Helper()
	executor := &fakeExecutor{mutate: func(execution ProbeExecution, observation *ProbeObservation) error {
		if execution.Case.Stage == StageHoldout {
			observation.Success = false
		}
		return nil
	}}
	profile, err := testProfiler(t, &fakeWorkspaceFactory{}, executor, ttl).Run(contextBackground(), target, testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestStoreSaveLoadAndExactTargetAutoSelection(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 48*time.Hour)
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Save(profile)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ProfileID != profile.ID() || ref.TargetDigest != target.Digest() {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	loaded, err := store.Load(target, profile.ID())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.AutoSelect(target, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID() != profile.ID() || selected.ID() != profile.ID() {
		t.Fatalf("loaded=%s selected=%s want=%s", loaded.ID(), selected.ID(), profile.ID())
	}

	profilePath := filepath.Join(store.Root(), target.Digest(), profile.ID()+".json")
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode=%#o want=0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"https://private-endpoint.invalid", "sk-never-persist-this-key", "never-persist-this-parameter"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("stored JSON leaked %q", forbidden)
		}
	}
}

func TestStoreConcurrentSaveIsExclusiveAndDoesNotOverwrite(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 48*time.Hour)
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, saveErr := store.Save(profile)
			errorsOut <- saveErr
		}()
	}
	close(start)
	wait.Wait()
	close(errorsOut)
	successes, conflicts := 0, 0
	for saveErr := range errorsOut {
		switch {
		case saveErr == nil:
			successes++
		case errors.Is(saveErr, ErrProfileConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent save error: %v", saveErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	before, err := os.ReadFile(filepath.Join(store.Root(), target.Digest(), profile.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(profile); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("repeat save error=%v want conflict", err)
	}
	after, err := os.ReadFile(filepath.Join(store.Root(), target.Digest(), profile.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("conflicting save overwrote immutable content")
	}
}

func TestStoreParameterDriftAndExpiryNeverAutoSelect(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 2*time.Hour)
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AutoSelect(target, fixedNow.Add(3*time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("expired auto select error=%v", err)
	}
	var selectionErr *SelectionError
	if _, err := store.AutoSelect(target, fixedNow.Add(3*time.Hour)); !errors.As(err, &selectionErr) || !reasonSliceContains(selectionErr.Reasons, QuarantineExpired) {
		t.Fatalf("expired selection reasons=%v err=%v", selectionErr, err)
	}

	drifted, err := NewTargetIdentity(TargetSpec{
		Provider: "test-provider", Model: "small-model/v1", Endpoint: "https://private-endpoint.invalid/v1?tenant=hidden",
		ServingParameters: map[string]any{"temperature": 0.3, "max_tokens": 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Digest() == target.Digest() {
		t.Fatal("test target did not drift")
	}
	if _, err := store.AutoSelect(drifted, fixedNow.Add(time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("parameter drift auto selected profile: %v", err)
	}
	if _, err := store.Load(drifted, profile.ID()); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("drifted explicit load error=%v want not found", err)
	}
}

func TestManualOverrideIsExplicitOnlyAndUsesEarlierExpiry(t *testing.T) {
	target := testTarget(t)
	base := quarantinedProfile(t, target, 2*time.Hour)
	overridden, err := base.WithManualOverride(ManualOverrideSpec{
		ID: "override-001", Actor: "operator-one",
		Reason:    "accept holdout risk for an isolated offline reproduction",
		ExpiresAt: fixedNow.Add(3 * time.Hour),
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if overridden.ID() == base.ID() {
		t.Fatal("manual provenance did not create new immutable content ID")
	}
	if overridden.Snapshot().Recommendations != base.Snapshot().Recommendations {
		t.Fatal("manual override unexpectedly patched recommendations")
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(overridden); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AutoSelect(target, fixedNow.Add(time.Hour)); !errors.Is(err, ErrNoSelectableProfile) {
		t.Fatalf("manual profile was automatically selected: %v", err)
	}
	if _, err := store.SelectManual(target, overridden.ID(), "override-001", fixedNow.Add(time.Hour)); err != nil {
		t.Fatalf("explicit valid override was rejected: %v", err)
	}
	if _, err := store.SelectManual(target, overridden.ID(), "wrong-id", fixedNow.Add(time.Hour)); !errors.Is(err, ErrManualOverride) {
		t.Fatalf("wrong override ID error=%v", err)
	}
	// Base profile expiry is earlier than override expiry, so it is the
	// effective deadline and stale measurements cannot be revived manually.
	if _, err := store.SelectManual(target, overridden.ID(), "override-001", fixedNow.Add(150*time.Minute)); !errors.Is(err, ErrManualOverride) {
		t.Fatalf("manual selection revived expired base: %v", err)
	}
}

func TestVerifiedProfileWithManualRecordIsNeverAutomatic(t *testing.T) {
	target := testTarget(t)
	base := verifiedProfile(t, target, 48*time.Hour)
	overridden, err := base.WithManualOverride(ManualOverrideSpec{
		ID: "override-verified", Actor: "operator-one", Reason: "force explicit selection in a replay test",
		ExpiresAt: fixedNow.Add(time.Hour),
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(overridden); err != nil {
		t.Fatal(err)
	}
	_, err = store.AutoSelect(target, fixedNow.Add(30*time.Minute))
	var selectionErr *SelectionError
	if !errors.As(err, &selectionErr) || !reasonSliceContains(selectionErr.Reasons, QuarantineManualOnly) {
		t.Fatalf("manual-only auto selection error=%v", err)
	}
	if _, err := store.SelectManual(target, overridden.ID(), "override-verified", fixedNow.Add(2*time.Hour)); !errors.Is(err, ErrManualOverride) {
		t.Fatalf("expired override remained selectable: %v", err)
	}
}

func TestStoreStrictLoadRejectsCorruptUnknownAndSchemaMismatch(t *testing.T) {
	target := testTarget(t)
	profile := verifiedProfile(t, target, 48*time.Hour)
	tests := []struct {
		name string
		data func(ProfileSnapshot) []byte
		want error
	}{
		{
			name: "corrupt",
			data: func(ProfileSnapshot) []byte { return []byte(`{"schemaVersion":`) },
			want: ErrInvalidProfile,
		},
		{
			name: "unknown-field",
			data: func(snapshot ProfileSnapshot) []byte {
				encoded, _ := json.Marshal(snapshot)
				return append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
			},
			want: ErrInvalidProfile,
		},
		{
			name: "schema-mismatch",
			data: func(snapshot ProfileSnapshot) []byte {
				snapshot.SchemaVersion++
				encoded, _ := json.Marshal(snapshot)
				return encoded
			},
			want: ErrSchemaMismatch,
		},
		{
			name: "duplicate-key",
			data: func(snapshot ProfileSnapshot) []byte {
				encoded, _ := json.Marshal(snapshot)
				return append(encoded[:len(encoded)-1], []byte(`,"schemaVersion":1}`)...)
			},
			want: ErrInvalidProfile,
		},
		{
			name: "excessive-depth",
			data: func(snapshot ProfileSnapshot) []byte {
				encoded, _ := json.Marshal(snapshot)
				nesting := strings.Repeat("[", maxProfileJSONDepth+2) + "0" + strings.Repeat("]", maxProfileJSONDepth+2)
				return append(encoded[:len(encoded)-1], []byte(`,"unknown":`+nesting+`}`)...)
			},
			want: ErrInvalidProfile,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(profile); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Root(), target.Digest(), profile.ID()+".json")
			if err := os.WriteFile(path, test.data(profile.Snapshot()), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(target, profile.ID()); !errors.Is(err, test.want) {
				t.Fatalf("Load error=%v want=%v", err, test.want)
			}
			_, err = store.AutoSelect(target, fixedNow.Add(time.Hour))
			var selectionErr *SelectionError
			if !errors.As(err, &selectionErr) {
				t.Fatalf("AutoSelect error=%v", err)
			}
			wantedReason := QuarantineCorrupt
			if errors.Is(test.want, ErrSchemaMismatch) {
				wantedReason = QuarantineSchemaMismatch
			}
			if !reasonSliceContains(selectionErr.Reasons, wantedReason) {
				t.Fatalf("selection reasons=%v want=%v", selectionErr.Reasons, wantedReason)
			}
		})
	}
}

func TestFutureCreatedProfileNeverAutoSelects(t *testing.T) {
	target := testTarget(t)
	base := verifiedProfile(t, target, 48*time.Hour)
	snapshot := base.Snapshot()
	snapshot.CreatedAt = fixedNow.Add(24 * time.Hour)
	snapshot.ExpiresAt = snapshot.CreatedAt.Add(2 * time.Hour)
	future, err := profileFromSnapshot(snapshot, true)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(future); err != nil {
		t.Fatal(err)
	}
	_, err = store.AutoSelect(target, fixedNow)
	var selectionErr *SelectionError
	if !errors.As(err, &selectionErr) || !reasonSliceContains(selectionErr.Reasons, QuarantineCorrupt) {
		t.Fatalf("future profile selection error=%v", err)
	}
}

func TestStoreRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := NewStore(linkRoot); !errors.Is(err, ErrUnsafeStorePath) {
		t.Fatalf("symlink store root error=%v", err)
	}
}

func reasonSliceContains(reasons []QuarantineReason, wanted QuarantineReason) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}
