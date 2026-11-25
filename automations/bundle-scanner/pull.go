package bundlescanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	titleAnnotation = "org.opencontainers.image.title"
	maxBundleBytes  = 512 << 20
)

// pull downloads a bundle version from the Massdriver OCI registry into dir. `mass bundle publish` stores each file
// as its own layer, named by the title annotation, so the layers are files and not archives.
func (s *Scanner) pull(ctx context.Context, repo, version, dir string) error {
	base := fmt.Sprintf("%s/v2/%s/%s", s.registry, s.api.OrganizationID, repo)

	var manifest struct {
		Layers []struct {
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	body, err := s.registryGet(ctx, base+"/manifests/"+version, ociManifestType, 4<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	var total int64
	for _, layer := range manifest.Layers {
		name := layer.Annotations[titleAnnotation]
		// A path from the registry must not write outside the scan directory.
		if name == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("layer %s has an unsafe file name %q", layer.Digest, name)
		}
		total += layer.Size
		if total > maxBundleBytes {
			return fmt.Errorf("bundle is larger than %d bytes", maxBundleBytes)
		}

		data, err := s.registryGet(ctx, base+"/blobs/"+layer.Digest, "", layer.Size)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != layer.Digest {
			return fmt.Errorf("%s does not match digest %s", name, layer.Digest)
		}

		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) registryGet(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+s.api.APIKey)
	if accept != "" {
		req.Header.Set("accept", accept)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", strings.TrimPrefix(url, s.registry), resp.StatusCode, b)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", strings.TrimPrefix(url, s.registry), limit)
	}
	return b, nil
}
