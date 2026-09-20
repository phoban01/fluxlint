package source

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const testContract = "requires:\n  crds:\n    - {group: platform.example.test, kind: Widget}\n"

func pushImage(t *testing.T, host, repo string) string {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(host + "/" + repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return ref.String()
}

// Registries with the referrers API and registries without it (where the
// client keeps an index under a fallback tag) must both work.
func TestContractTravelsWithTheImage(t *testing.T) {
	for name, opts := range map[string][]registry.Option{
		"referrers API": {registry.WithReferrersSupport(true)},
		"tag fallback":  {registry.WithReferrersSupport(false)},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(registry.New(opts...))
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "http://")
			ctx := context.Background()

			image := pushImage(t, host, "org/operator:v1.2.3")
			if got, err := FetchContract(ctx, image); err != nil || got != nil {
				t.Fatalf("an image without a contract: %q, %v", got, err)
			}
			if _, err := PushContract(ctx, image, []byte(testContract)); err != nil {
				t.Fatal(err)
			}
			got, err := FetchContract(ctx, image)
			if err != nil || string(got) != testContract {
				t.Fatalf("got %q, %v", got, err)
			}

			// a newer contract for the same image replaces the older one
			newer := testContract + "    - {group: platform.example.test, kind: Gadget}\n"
			if _, err := PushContract(ctx, image, []byte(newer)); err != nil {
				t.Fatal(err)
			}
			if got, _ := FetchContract(ctx, image); string(got) != newer {
				t.Errorf("want the newest contract, got %q", got)
			}

			// another image in the same repository has none
			other := pushImage(t, host, "org/operator:v1.2.4")
			if got, err := FetchContract(ctx, other); err != nil || got != nil {
				t.Errorf("v1.2.4 has no contract: %q, %v", got, err)
			}
			if _, err := PushContract(ctx, host+"/org/operator:v9.9.9", []byte(testContract)); err == nil {
				t.Error("attaching to an image that does not exist must fail")
			}
		})
	}
}

func TestImageContractIsCached(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	with := pushImage(t, host, "org/operator:v1")
	without := pushImage(t, host, "org/plain:v1")
	if _, err := PushContract(ctx, with, []byte(testContract)); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	online, err := New(Options{CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := online.ImageContract(ctx, with); err != nil || !strings.Contains(string(got), "Widget") {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := online.ImageContract(ctx, without); err != nil || got != nil {
		t.Fatalf("got %q, %v", got, err)
	}

	// with the registry gone, both answers come from the cache
	srv.Close()
	offline, err := New(Options{CacheDir: cache, Mode: Offline})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := offline.ImageContract(ctx, with); err != nil || !strings.Contains(string(got), "Widget") {
		t.Errorf("cached contract: %q, %v", got, err)
	}
	if got, err := offline.ImageContract(ctx, without); err != nil || got != nil {
		t.Errorf("cached absence: %q, %v", got, err)
	}
	if _, err := offline.ImageContract(ctx, host+"/org/unseen:v1"); !errors.Is(err, ErrNotCached) {
		t.Errorf("an image never looked up is not cached: %v", err)
	}
}
