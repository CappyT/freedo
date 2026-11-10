package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchLatestBridgeFirmwareVerifiesMergedReleaseAsset(t *testing.T) {
	image := []byte("synthetic merged firmware")
	sum := sha256.Sum256(image)
	checksum := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(bridgeRelease{
				Tag: "v1.2.3",
				Assets: []bridgeFirmwareAsset{
					{Name: "freedo-cc1101-bridge-1.2.3-merged.bin", URL: serverURL(r) + "/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin"},
					{Name: "SHA256SUMS", URL: serverURL(r) + "/CappyT/freedo/releases/download/v1.2.3/SHA256SUMS"},
				},
			})
		case "/CappyT/freedo/releases/download/v1.2.3/SHA256SUMS":
			_, _ = w.Write([]byte(checksum + "  freedo-cc1101-bridge-1.2.3-merged.bin\n"))
		case "/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin":
			_, _ = w.Write(image)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := fetchLatestBridgeFirmware(context.Background(), server.Client(), server.URL+"/latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.version != "1.2.3" || got.sum != checksum || string(got.image) != string(image) {
		t.Fatalf("got version=%q sha256=%q image=%q", got.version, got.sum, got.image)
	}
}

func TestFetchLatestBridgeFirmwareRejectsChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(bridgeRelease{
				Tag: "v1.2.3",
				Assets: []bridgeFirmwareAsset{
					{Name: "freedo-cc1101-bridge-1.2.3-merged.bin", URL: serverURL(r) + "/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin"},
					{Name: "SHA256SUMS", URL: serverURL(r) + "/CappyT/freedo/releases/download/v1.2.3/SHA256SUMS"},
				},
			})
		case "/CappyT/freedo/releases/download/v1.2.3/SHA256SUMS":
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  freedo-cc1101-bridge-1.2.3-merged.bin\n"))
		case "/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin":
			_, _ = w.Write([]byte("unexpected"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := fetchLatestBridgeFirmware(context.Background(), server.Client(), server.URL+"/latest")
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got error %v, want checksum mismatch", err)
	}
}

func TestValidateBridgeAssetURLRequiresTheExpectedReleasePath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"expected release asset", "https://github.com/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin", true},
		{"wrong repository", "https://github.com/other/repo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin", false},
		{"wrong tag", "https://github.com/CappyT/freedo/releases/download/v9.9.9/freedo-cc1101-bridge-1.2.3-merged.bin", false},
		{"wrong asset", "https://github.com/CappyT/freedo/releases/download/v1.2.3/image.bin", false},
		{"unexpected port", "https://github.com:8443/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin", false},
		{"query string", "https://github.com/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin?redirect=elsewhere", false},
		{"wrong host", "https://github.example/CappyT/freedo/releases/download/v1.2.3/freedo-cc1101-bridge-1.2.3-merged.bin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBridgeAssetURL(tt.url, "github.com", "v1.2.3", "freedo-cc1101-bridge-1.2.3-merged.bin")
			if (err == nil) != tt.want {
				t.Fatalf("validateBridgeAssetURL() error = %v, want valid=%v", err, tt.want)
			}
		})
	}
}

func TestApplyPendingBridgeFirmwareRunsAndRecordsSuccess(t *testing.T) {
	database := filepath.Join(t.TempDir(), "state.db")
	image := []byte("synthetic merged firmware")
	sum := sha256.Sum256(image)
	if err := os.WriteFile(bridgeFirmwareImage(database), image, 0o600); err != nil {
		t.Fatal(err)
	}
	status := bridgeFirmwareStatus{
		State:   "pending",
		Version: "1.2.3",
		SHA256:  hex.EncodeToString(sum[:]),
	}
	if err := writeBridgeFirmwareStatus(bridgeFirmwareManifest(database), status); err != nil {
		t.Fatal(err)
	}

	called := false
	applyPendingBridgeFirmware(database, "COM7", func(_ context.Context, port, imagePath, offset string) ([]byte, error) {
		called = true
		if port != "COM7" || imagePath != bridgeFirmwareImage(database) || offset != "0x0" {
			t.Fatalf("got port=%q image=%q offset=%q", port, imagePath, offset)
		}
		return []byte("write complete"), nil
	})
	got, err := readBridgeFirmwareStatus(bridgeFirmwareManifest(database))
	if err != nil {
		t.Fatal(err)
	}
	if !called || got.State != "succeeded" || got.Version != status.Version || got.Error != "" {
		t.Fatalf("called=%v status=%+v", called, got)
	}
	if _, err := os.Stat(bridgeFirmwareImage(database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged image still exists: %v", err)
	}
}

func TestApplyPendingBridgeFirmwareRecordsFailure(t *testing.T) {
	database := filepath.Join(t.TempDir(), "state.db")
	image := []byte("synthetic merged firmware")
	sum := sha256.Sum256(image)
	if err := os.WriteFile(bridgeFirmwareImage(database), image, 0o600); err != nil {
		t.Fatal(err)
	}
	status := bridgeFirmwareStatus{State: "pending", Version: "1.2.3", SHA256: hex.EncodeToString(sum[:])}
	if err := writeBridgeFirmwareStatus(bridgeFirmwareManifest(database), status); err != nil {
		t.Fatal(err)
	}

	applyPendingBridgeFirmware(database, "COM7", func(context.Context, string, string, string) ([]byte, error) {
		return []byte("serial port unavailable"), errors.New("esptool failed")
	})
	got, err := readBridgeFirmwareStatus(bridgeFirmwareManifest(database))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" || !strings.Contains(got.Error, "serial port unavailable") {
		t.Fatalf("got status %+v", got)
	}
}

func TestApplyPendingBridgeFirmwareRejectsChangedStagedImage(t *testing.T) {
	database := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(bridgeFirmwareImage(database), []byte("unexpected image"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := bridgeFirmwareStatus{State: "pending", Version: "1.2.3", SHA256: strings.Repeat("0", 64)}
	if err := writeBridgeFirmwareStatus(bridgeFirmwareManifest(database), status); err != nil {
		t.Fatal(err)
	}

	called := false
	applyPendingBridgeFirmware(database, "COM7", func(context.Context, string, string, string) ([]byte, error) {
		called = true
		return nil, nil
	})
	got, err := readBridgeFirmwareStatus(bridgeFirmwareManifest(database))
	if err != nil {
		t.Fatal(err)
	}
	if called || got.State != "failed" || !strings.Contains(got.Error, "checksum") {
		t.Fatalf("called=%v status=%+v", called, got)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
