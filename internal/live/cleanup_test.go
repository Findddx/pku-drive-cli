//go:build live

package live

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/securefs"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

func newLiveServicesFromSecureStore(t *testing.T) (*anyshare.Client, *remote.Service) {
	t.Helper()
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(paths)
	configuration, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Server != config.DefaultServer {
		t.Fatalf("live acceptance requires server %q", config.DefaultServer)
	}
	credentials, err := store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Server != configuration.Server {
		t.Fatal("live credentials and configured server do not match")
	}
	hx := httpx.New(nil, httpx.Policy{
		MaxAttempts: 5,
		BaseDelay:   200 * time.Millisecond,
		MaxDelay:    5 * time.Second,
	})
	oauthClient := oauth.NewClient(configuration.Server, hx)
	manager := &auth.Manager{Store: store, OAuth: oauthClient, Now: time.Now}
	client := anyshare.NewClient(configuration.Server, hx, manager)
	return client, remote.NewService(client)
}

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("PKU_DRIVE_LIVE") != "1" {
		t.Skip("PKU_DRIVE_LIVE=1 is required")
	}
}

func TestServerPlansMultipartFor128MiB(t *testing.T) {
	requireLive(t)
	client, _ := newLiveServicesFromSecureStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	options, err := client.StorageOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := upload.BuildPlan(128<<20, upload.Limits{
		PartMinSize: options.PartMinSize,
		PartMaxSize: options.PartMaxSize,
		PartMaxNum:  options.PartMaxNum,
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storage options=%+v plan=%+v", options, plan)
	if !plan.Multipart || plan.PartCount < 2 {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestCleanupExactDirectory(t *testing.T) {
	requireLive(t)
	manifestPath := os.Getenv("PKU_DRIVE_LIVE_MANIFEST")
	if manifestPath == "" {
		t.Fatal("PKU_DRIVE_LIVE_MANIFEST is required")
	}
	var manifest Manifest
	if err := securefs.ReadJSON0600(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}

	client, paths := newLiveServicesFromSecureStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	library, err := paths.Resolve(ctx, manifest.LibraryPath)
	if err != nil {
		t.Fatal("resolve live library failed")
	}
	if library.ID != manifest.LibraryID || library.Type != "user_doc_lib" || library.Path != manifest.LibraryPath {
		t.Fatal("refusing cleanup: resolved library identity changed")
	}
	item, err := paths.Resolve(ctx, manifest.TestPath)
	if err != nil {
		libraryEntries, listErr := paths.List(ctx, manifest.LibraryPath)
		if listErr != nil {
			t.Fatal("resolve cleanup target and prior-cleanup verification both failed")
		}
		if absentErr := ValidateDeleted(manifest, libraryEntries); absentErr != nil {
			t.Fatal("cleanup target is not safely absent")
		}
		t.Log("exact cleanup target was already absent")
		return
	}
	if item.ID != manifest.TestID || item.Type != "directory" || item.Path != manifest.TestPath {
		t.Fatal("refusing cleanup: resolved test-directory identity changed")
	}
	entries, err := paths.List(ctx, manifest.TestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCleanupEntries(manifest, entries); err != nil {
		t.Fatal(err)
	}
	deleteResult, deleteErr := client.DeleteDir(ctx, manifest.TestID)
	if deleteErr == nil && deleteResult.Status == anyshare.DeleteStatusPendingReview {
		t.Fatal("cleanup deletion is pending review; exact directory was not deleted")
	}
	var verifyErr error
	for attempt := 0; attempt < 5; attempt++ {
		libraryEntries, listErr := paths.List(ctx, manifest.LibraryPath)
		if listErr != nil {
			verifyErr = listErr
		} else {
			verifyErr = ValidateDeleted(manifest, libraryEntries)
			if verifyErr == nil {
				break
			}
		}
		if attempt < 4 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if verifyErr != nil {
		if deleteErr != nil {
			t.Fatal("delete result unknown and exact absence could not be verified")
		}
		t.Fatal("exact cleanup absence verification failed")
	}
	if deleteErr != nil {
		t.Log("delete response was inconclusive but exact absence was verified")
	}
}
