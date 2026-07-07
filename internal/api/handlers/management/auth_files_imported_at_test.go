package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestEnsureImportedAt_StampsOnFirstImport(t *testing.T) {
	auth := &coreauth.Auth{ID: "a"}

	before := time.Now().UTC()
	ensureImportedAt(auth)
	after := time.Now().UTC()

	raw, ok := auth.Metadata["imported_at"]
	if !ok {
		t.Fatalf("imported_at not set")
	}
	str, ok := raw.(string)
	if !ok {
		t.Fatalf("imported_at = %T, want string", raw)
	}
	ts, err := time.Parse(time.RFC3339, str)
	if err != nil {
		t.Fatalf("imported_at not RFC3339: %v", err)
	}
	if ts.Before(before.Truncate(time.Second)) || ts.After(after.Add(time.Second)) {
		t.Fatalf("imported_at = %v, want within [%v, %v]", ts, before, after)
	}
}

func TestEnsureImportedAt_DoesNotOverwriteExisting(t *testing.T) {
	original := "2020-01-02T03:04:05Z"
	auth := &coreauth.Auth{
		ID:       "a",
		Metadata: map[string]any{"imported_at": original},
	}

	ensureImportedAt(auth)

	if got := auth.Metadata["imported_at"]; got != original {
		t.Fatalf("imported_at = %v, want preserved %q", got, original)
	}
}

func TestEnsureImportedAt_ReplacesInvalidValue(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "a",
		Metadata: map[string]any{"imported_at": "not-a-date"},
	}

	ensureImportedAt(auth)

	raw := auth.Metadata["imported_at"]
	str, ok := raw.(string)
	if !ok {
		t.Fatalf("imported_at = %T, want string", raw)
	}
	if _, err := time.Parse(time.RFC3339, str); err != nil {
		t.Fatalf("invalid value not replaced with RFC3339: %q (%v)", str, err)
	}
}

func TestEnsureImportedAt_NilMetadataInitialized(t *testing.T) {
	auth := &coreauth.Auth{ID: "a"}
	if auth.Metadata != nil {
		t.Fatalf("precondition: Metadata should be nil")
	}
	ensureImportedAt(auth)
	if auth.Metadata == nil {
		t.Fatalf("Metadata not initialized")
	}
}

func TestSurvivalDaysFromImportedAt(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		imported any
		wantDays int
		wantOK   bool
	}{
		{"rfc3339_zero_days_same_day", "2026-06-27T00:00:00Z", 0, true},
		{"rfc3339_one_day", "2026-06-26T11:00:00Z", 1, true},
		{"rfc3339_ten_days", "2026-06-17T12:00:00Z", 10, true},
		{"datetime_layout", "2026-06-20 12:00:00", 7, true},
		{"unix_string", "1718841600", int(now.Sub(time.Unix(1718841600, 0).UTC()) / (24 * time.Hour)), true},
		{"unix_float", float64(1718841600), int(now.Sub(time.Unix(1718841600, 0).UTC()) / (24 * time.Hour)), true},
		{"future_clamped_to_zero", "2026-07-01T00:00:00Z", 0, true},
		{"invalid", "garbage", 0, false},
		{"empty", "", 0, false},
		{"nil", nil, 0, false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gotDays, gotOK := survivalDaysFromImportedAt(tt.imported, now)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotDays != tt.wantDays {
				t.Fatalf("days = %d, want %d", gotDays, tt.wantDays)
			}
		})
	}
}

// TestImportedAt_RoundTripSurvivalDays verifies the end-to-end logic: stamp an
// import, then compute survival days N days later.
func TestImportedAt_RoundTripSurvivalDays(t *testing.T) {
	auth := &coreauth.Auth{ID: "a"}
	ensureImportedAt(auth)

	imported := auth.Metadata["imported_at"]
	importedTS, _ := time.Parse(time.RFC3339, imported.(string))

	future := importedTS.Add(15*24*time.Hour + time.Hour)
	days, ok := survivalDaysFromImportedAt(imported, future)
	if !ok {
		t.Fatalf("survivalDaysFromImportedAt ok = false")
	}
	if days != 15 {
		t.Fatalf("survival_days = %d, want 15", days)
	}
}

func TestListAuthFiles_IncludesSurvivalDaysFromManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-user@example.com.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex","email":"user@example.com"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	importedAt := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type":        "codex",
			"imported_at": importedAt,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if got := entry["imported_at"]; got != importedAt {
		t.Fatalf("imported_at = %#v, want %q", got, importedAt)
	}
	if got, ok := entry["survival_days"].(float64); !ok || got != 10 {
		t.Fatalf("survival_days = %#v, want 10", entry["survival_days"])
	}
}

func TestListAuthFilesFromDisk_IncludesSurvivalDays(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	importedAt := time.Now().UTC().Add(-5 * 24 * time.Hour).Format(time.RFC3339)
	filePath := filepath.Join(authDir, "codex-user@example.com.json")
	content := `{"type":"codex","email":"user@example.com","imported_at":"` + importedAt + `"}`
	if errWrite := os.WriteFile(filePath, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	// nil manager forces the disk fallback path.
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	entry := firstAuthFileEntry(t, h)
	if got := entry["imported_at"]; got != importedAt {
		t.Fatalf("imported_at = %#v, want %q", got, importedAt)
	}
	if got, ok := entry["survival_days"].(float64); !ok || got != 5 {
		t.Fatalf("survival_days = %#v, want 5", entry["survival_days"])
	}
}
