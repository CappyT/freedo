package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	bridgeFirmwareManifestSuffix = ".bridge-firmware.json"
	bridgeFirmwareImageSuffix    = ".bridge-firmware.bin"
	bridgeFirmwareMaxBytes       = 8 << 20
	bridgeFirmwareHTTPTimeout    = 45 * time.Second
	bridgeFirmwareFlashTimeout   = 5 * time.Minute
	bridgeFirmwareOutputLimit    = 16 << 10
)

var bridgeVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)

type bridgeFirmwareStatus struct {
	State     string    `json:"state"`
	Version   string    `json:"version,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type bridgeFirmwareAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type bridgeRelease struct {
	Tag    string                `json:"tag_name"`
	Assets []bridgeFirmwareAsset `json:"assets"`
}

type latestBridgeFirmware struct {
	version string
	image   []byte
	sum     string
}

var bridgeFirmwareHTTP = &http.Client{Timeout: bridgeFirmwareHTTPTimeout}
var bridgeReleaseAPI = "https://api.github.com/repos/CappyT/freedo/releases/latest"

func bridgeFirmwareManifest(path string) string { return path + bridgeFirmwareManifestSuffix }
func bridgeFirmwareImage(path string) string    { return path + bridgeFirmwareImageSuffix }

func (d *daemon) getBridgeFirmware(w http.ResponseWriter, r *http.Request) {
	status := bridgeFirmwareStatus{State: "idle"}
	supported, reason := d.bridgeFirmwareSupported()
	if d.state != nil {
		stored, err := readBridgeFirmwareStatus(bridgeFirmwareManifest(d.state.path))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "read bridge firmware status: "+err.Error())
			return
		}
		if stored.State != "" {
			status = stored
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supported": supported,
		"reason":    reason,
		"state":     status.State,
		"version":   status.Version,
		"sha256":    status.SHA256,
		"error":     status.Error,
		"updatedAt": status.UpdatedAt,
	})
}

func (d *daemon) bridgeFirmwareSupported() (bool, string) {
	if d.mode != "live" || d.bridgePort == "" {
		return false, "no_live_bridge"
	}
	if d.state == nil || d.state.path == "" {
		return false, "no_state_database"
	}
	if _, err := exec.LookPath("esptool"); err != nil {
		return false, "esptool_missing"
	}
	return true, ""
}

func (d *daemon) postBridgeFirmwareUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeBodyLimit(w, r, &body, 1024) {
		return
	}
	if !body.Confirm {
		writeErr(w, http.StatusBadRequest, `firmware update needs {"confirm":true}`)
		return
	}
	if ok, reason := d.bridgeFirmwareSupported(); !ok {
		writeErr(w, http.StatusConflict, reason)
		return
	}
	d.maintenanceMu.Lock()
	defer d.maintenanceMu.Unlock()

	manifestPath := bridgeFirmwareManifest(d.state.path)
	current, err := readBridgeFirmwareStatus(manifestPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read bridge firmware status: "+err.Error())
		return
	}
	if current.State == "pending" || current.State == "flashing" {
		writeErr(w, http.StatusConflict, "a bridge firmware update is already in progress")
		return
	}
	if len(d.restart) > 0 {
		writeErr(w, http.StatusConflict, "freedo is already restarting")
		return
	}

	firmware, err := fetchLatestBridgeFirmware(r.Context(), bridgeFirmwareHTTP, bridgeReleaseAPI)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download the latest bridge firmware: "+err.Error())
		return
	}
	status := bridgeFirmwareStatus{
		State:     "pending",
		Version:   firmware.version,
		SHA256:    firmware.sum,
		UpdatedAt: time.Now().UTC(),
	}
	imagePath := bridgeFirmwareImage(d.state.path)
	if err := writePrivateFile(imagePath, firmware.image); err != nil {
		writeErr(w, http.StatusInternalServerError, "stage bridge firmware: "+err.Error())
		return
	}
	if err := writeBridgeFirmwareStatus(manifestPath, status); err != nil {
		_ = os.Remove(imagePath)
		writeErr(w, http.StatusInternalServerError, "stage bridge firmware status: "+err.Error())
		return
	}

	log.Printf("bridge firmware: update to %s accepted; restarting to release %s", status.Version, d.bridgePort)
	writeJSON(w, http.StatusAccepted, status)
	d.restart <- struct{}{}
}

func readBridgeFirmwareStatus(name string) (bridgeFirmwareStatus, error) {
	var status bridgeFirmwareStatus
	raw, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return status, err
	}
	return status, nil
}

func writeBridgeFirmwareStatus(name string, status bridgeFirmwareStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return writePrivateFile(name, raw)
}

func writePrivateFile(name string, data []byte) error {
	dir := filepath.Dir(name)
	file, err := os.CreateTemp(dir, ".bridge-firmware-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, name)
}

func fetchLatestBridgeFirmware(ctx context.Context, client *http.Client, apiURL string) (latestBridgeFirmware, error) {
	var out latestBridgeFirmware
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "freedo-bridge-firmware-updater")
	res, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	raw, err := readBridgeResponse(res, 1<<20)
	if err != nil {
		return out, err
	}
	var release bridgeRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return out, fmt.Errorf("read latest release: %w", err)
	}
	version := strings.TrimPrefix(release.Tag, "v")
	if !bridgeVersionPattern.MatchString(version) {
		return out, fmt.Errorf("invalid release tag %q", release.Tag)
	}
	imageName := "freedo-cc1101-bridge-" + version + "-merged.bin"
	imageURL, sumsURL := "", ""
	for _, asset := range release.Assets {
		switch asset.Name {
		case imageName:
			imageURL = asset.URL
		case "SHA256SUMS":
			sumsURL = asset.URL
		}
	}
	if imageURL == "" || sumsURL == "" {
		return out, fmt.Errorf("release %s has no merged firmware and SHA256SUMS", release.Tag)
	}
	expectedHost := "github.com"
	if api, parseErr := url.Parse(apiURL); parseErr == nil && api.Hostname() != "api.github.com" {
		expectedHost = api.Hostname()
	}
	if err := validateBridgeAssetURL(imageURL, expectedHost, release.Tag, imageName); err != nil {
		return out, err
	}
	if err := validateBridgeAssetURL(sumsURL, expectedHost, release.Tag, "SHA256SUMS"); err != nil {
		return out, err
	}
	sums, err := fetchBridgeAsset(ctx, client, sumsURL, 32<<10)
	if err != nil {
		return out, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	expected, err := bridgeChecksum(string(sums), imageName)
	if err != nil {
		return out, err
	}
	image, err := fetchBridgeAsset(ctx, client, imageURL, bridgeFirmwareMaxBytes)
	if err != nil {
		return out, fmt.Errorf("download merged firmware: %w", err)
	}
	actual := sha256.Sum256(image)
	sum := hex.EncodeToString(actual[:])
	if sum != expected {
		return out, errors.New("merged firmware checksum does not match SHA256SUMS")
	}
	return latestBridgeFirmware{version: version, image: image, sum: sum}, nil
}

func readBridgeResponse(res *http.Response, limit int64) ([]byte, error) {
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %s bytes", strconv.FormatInt(limit, 10))
	}
	return raw, nil
}

func fetchBridgeAsset(ctx context.Context, client *http.Client, assetURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "freedo-bridge-firmware-updater")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return readBridgeResponse(res, limit)
}

func validateBridgeAssetURL(raw, host, tag, name string) error {
	u, err := url.Parse(raw)
	wantScheme := "https"
	if host != "github.com" {
		wantScheme = "http"
	}
	expectedPath := "/CappyT/freedo/releases/download/" + tag + "/" + name
	if err != nil || u.Scheme != wantScheme || u.Hostname() != host ||
		u.Port() != "" || u.User != nil || u.Path != expectedPath || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("release asset URL is not a trusted GitHub release URL")
	}
	return nil
}

func bridgeChecksum(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			return "", errors.New("SHA256SUMS has an invalid checksum for the merged firmware")
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", errors.New("SHA256SUMS has an invalid checksum for the merged firmware")
		}
		return sum, nil
	}
	return "", errors.New("SHA256SUMS has no checksum for the merged firmware")
}

func applyPendingBridgeFirmware(databasePath, port string, run func(context.Context, string, string, string) ([]byte, error)) {
	if port == "" {
		return
	}
	manifestPath := bridgeFirmwareManifest(databasePath)
	status, err := readBridgeFirmwareStatus(manifestPath)
	if err != nil {
		log.Printf("bridge firmware: read staged update: %v", err)
		return
	}
	if status.State == "flashing" {
		status.State = "failed"
		status.Error = "the previous flash was interrupted"
		status.UpdatedAt = time.Now().UTC()
		if err := writeBridgeFirmwareStatus(manifestPath, status); err != nil {
			log.Printf("bridge firmware: write interrupted status: %v", err)
		}
		_ = os.Remove(bridgeFirmwareImage(databasePath))
		return
	}
	if status.State != "pending" {
		return
	}
	imagePath := bridgeFirmwareImage(databasePath)
	image, err := os.ReadFile(imagePath)
	if err == nil {
		sum := sha256.Sum256(image)
		if hex.EncodeToString(sum[:]) != status.SHA256 {
			err = errors.New("staged firmware checksum does not match")
		}
	}
	if err == nil {
		status.State = "flashing"
		status.Error = ""
		status.UpdatedAt = time.Now().UTC()
		err = writeBridgeFirmwareStatus(manifestPath, status)
	}
	var output []byte
	if err == nil {
		output, err = run(context.Background(), port, imagePath, "0x0")
	}
	if err != nil {
		status.State = "failed"
		status.Error = conciseBridgeError(err, output)
		log.Printf("bridge firmware: flash %s failed: %s", status.Version, status.Error)
	} else {
		status.State = "succeeded"
		status.Error = ""
		log.Printf("bridge firmware: flashed %s to %s", status.Version, port)
	}
	status.UpdatedAt = time.Now().UTC()
	if saveErr := writeBridgeFirmwareStatus(manifestPath, status); saveErr != nil {
		log.Printf("bridge firmware: write final status: %v", saveErr)
	}
	if removeErr := os.Remove(imagePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		log.Printf("bridge firmware: remove staged image: %v", removeErr)
	}
}

func runEsptool(ctx context.Context, port, imagePath, offset string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeFirmwareFlashTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "esptool", "--chip", "esp32", "--port", port, "write-flash", offset, imagePath)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func conciseBridgeError(err error, output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) > bridgeFirmwareOutputLimit {
		text = text[len(text)-bridgeFirmwareOutputLimit:]
	}
	if text == "" {
		return err.Error()
	}
	return err.Error() + ": " + text
}
