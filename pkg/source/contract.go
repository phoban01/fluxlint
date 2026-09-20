package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// A contract travels with the image it describes, as an OCI referrer: an
// artifact whose subject is the image. Whoever deploys the image — from a
// chart, a Git repository or plain manifests — gets the contract with it, and
// it cannot belong to the wrong version.
const (
	ContractArtifactType = "application/vnd.fluxlint.contract.v1+yaml"
	contractLayerType    = "application/vnd.fluxlint.contract.layer.v1+yaml"
	maxContractBytes     = 1 << 20
)

func remoteOptions(ctx context.Context) []remote.Option {
	transport := remote.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	transport.TLSHandshakeTimeout = connectTimeout
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithTransport(transport)}
}

// PushContract attaches a contract to an image and returns the artifact's
// reference. The image must already be in the registry. Registries without the
// referrers API are handled by go-containerregistry's tag fallback.
func PushContract(ctx context.Context, image string, contract []byte) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", err
	}
	opts := remoteOptions(ctx)
	subject, err := remote.Head(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("%s: the image must be pushed before its contract: %w", image, err)
	}
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, ContractArtifactType)
	img, err = mutate.AppendLayers(img, static.NewLayer(contract, contractLayerType))
	if err != nil {
		return "", err
	}
	img = mutate.Annotations(img, map[string]string{
		"org.opencontainers.image.title": "fluxlint-contract.yaml",
	}).(v1.Image)
	attached, ok := mutate.Subject(img, *subject).(v1.Image)
	if !ok {
		return "", errors.New("cannot set the artifact's subject")
	}
	digest, err := attached.Digest()
	if err != nil {
		return "", err
	}
	target := ref.Context().Digest(digest.String())
	if err := remote.Write(target, attached, opts...); err != nil {
		return "", err
	}
	return target.String(), nil
}

// FetchContract returns the contract attached to image, or nil when it has
// none. When several are attached the newest push wins.
func FetchContract(ctx context.Context, image string) ([]byte, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, err
	}
	opts := remoteOptions(ctx)
	subject, err := remote.Head(ref, opts...)
	if err != nil {
		return nil, err
	}
	index, err := remote.Referrers(ref.Context().Digest(subject.Digest.String()), opts...)
	if err != nil {
		return nil, err
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}
	for i := len(manifest.Manifests) - 1; i >= 0; i-- {
		d := manifest.Manifests[i]
		if d.ArtifactType != ContractArtifactType {
			continue
		}
		img, err := remote.Image(ref.Context().Digest(d.Digest.String()), opts...)
		if err != nil {
			return nil, err
		}
		layers, err := img.Layers()
		if err != nil || len(layers) == 0 {
			return nil, fmt.Errorf("contract artifact %s has no layer", d.Digest)
		}
		rc, err := layers[0].Uncompressed()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxContractBytes))
	}
	return nil, nil
}

type contractCall struct {
	once     sync.Once
	contract []byte
	err      error
}

type contractMeta struct {
	Image   string    `json:"image"`
	Found   bool      `json:"found"`
	Fetched time.Time `json:"fetched"`
}

// ImageContract is FetchContract behind the cache. An image referenced by
// digest never changes, so neither does the answer. For a tag the cached answer
// is used until --refresh, like every other floating reference.
func (r *Resolver) ImageContract(ctx context.Context, image string) ([]byte, error) {
	r.mu.Lock()
	if r.contracts == nil {
		r.contracts = map[string]*contractCall{}
	}
	c := r.contracts[image]
	if c == nil {
		c = &contractCall{}
		r.contracts[image] = c
	}
	r.mu.Unlock()
	c.once.Do(func() { c.contract, c.err = r.imageContract(ctx, image) })
	return c.contract, c.err
}

func (r *Resolver) imageContract(ctx context.Context, image string) ([]byte, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, err
	}
	slot := filepath.Join(r.opts.CacheDir, "contracts", hashOf(ref.Name()))
	_, pinned := ref.(name.Digest)
	if b, err := os.ReadFile(slot + ".json"); err == nil && (pinned || r.opts.Mode != Refresh) {
		var m contractMeta
		if json.Unmarshal(b, &m) == nil {
			if !m.Found {
				return nil, nil
			}
			if contract, err := os.ReadFile(slot + ".yaml"); err == nil {
				return contract, nil
			}
		}
	}
	if r.opts.Mode == Offline {
		return nil, fmt.Errorf("%s: %w", image, ErrNotCached)
	}
	host := ref.Context().RegistryStr()
	if err := r.hostDown(host); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	contract, err := FetchContract(ctx, image)
	if err != nil {
		r.markDown(host, err)
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return nil, err
	}
	if contract != nil {
		if err := os.WriteFile(slot+".yaml", bytes.TrimSpace(contract), 0o644); err != nil {
			return nil, err
		}
	}
	meta, _ := json.MarshalIndent(contractMeta{Image: ref.Name(), Found: contract != nil, Fetched: time.Now().UTC()}, "", "  ")
	return contract, os.WriteFile(slot+".json", meta, 0o644)
}
