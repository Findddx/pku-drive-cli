package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/cli"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/sharelink"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

type wireOptions struct {
	defaultPaths func() (config.Paths, error)
	transport    http.RoundTripper
	browser      oauth.Browser
}

type wiredRuntime struct {
	dependencies cli.Dependencies
	httpClient   *httpx.Client
	uploader     *upload.Uploader
}

type authOperations interface {
	Login(context.Context, io.Reader, io.Writer) (auth.Status, error)
	Status(context.Context) (auth.Status, error)
	Logout(context.Context, bool) error
}

type identityOperations interface {
	CurrentUser(context.Context) (anyshare.User, error)
}

type remoteOperations interface {
	List(context.Context, string) ([]anyshare.Item, error)
	Mkdir(context.Context, string, bool) (anyshare.Item, error)
	Resolve(context.Context, string) (anyshare.Item, error)
	ResolveUploadTarget(context.Context, string, string) (remote.UploadTarget, error)
	Delete(context.Context, string, bool) (remote.DeleteResult, error)
}

type uploaderOperations interface {
	Put(context.Context, string, remote.UploadTarget, bool, upload.Progress) (upload.Result, error)
}

type downloaderOperations interface {
	Get(context.Context, anyshare.Item, string, bool, download.Progress) (download.Result, error)
}

type shareOperations interface {
	Open(context.Context, string) (*sharelink.Session, error)
}

type dependencyServices struct {
	manager    authOperations
	identity   identityOperations
	remote     remoteOperations
	uploader   uploaderOperations
	downloader downloaderOperations
	shares     shareOperations
}

func wireDependencies() cli.Dependencies {
	return wireDependenciesWith(wireOptions{
		defaultPaths: config.DefaultPaths,
		browser:      oauth.NewSystemBrowser(),
	})
}

func wireDependenciesWith(options wireOptions) cli.Dependencies {
	runtime, err := newWiredRuntime(options)
	if err != nil {
		return unavailableDependencies(err)
	}
	return runtime.dependencies
}

func newWiredRuntime(options wireOptions) (*wiredRuntime, error) {
	paths := options.defaultPaths
	if paths == nil {
		paths = config.DefaultPaths
	}
	storagePaths, err := paths()
	if err != nil {
		return nil, err
	}
	store := config.NewStore(storagePaths)
	configuration, err := store.LoadConfig()
	if err != nil {
		return nil, err
	}
	server := configuration.Server
	if server == "" {
		server = config.DefaultServer
	}

	httpClient := httpx.New(options.transport, httpx.Policy{MaxAttempts: 5})
	oauthClient := oauth.NewClient(server, httpClient)
	authorizer := oauth.NewBrowserAuthorizer(oauthClient, options.browser)
	manager := &auth.Manager{Store: store, OAuth: oauthClient, Authorizer: authorizer}
	api := anyshare.NewClient(server, httpClient, manager)
	remoteService := remote.NewService(api)
	uploader := upload.NewUploader(server, api, remoteService, upload.NewStateStore(storagePaths.UploadStateDir), 4)
	downloader := download.NewDownloader(api)
	shares, err := sharelink.NewOpener(server, httpClient, api)
	if err != nil {
		return nil, err
	}
	dependencies := dependenciesFromServices(dependencyServices{
		manager: manager, identity: api, remote: remoteService, uploader: uploader, downloader: downloader, shares: shares,
	})
	return &wiredRuntime{dependencies: dependencies, httpClient: httpClient, uploader: uploader}, nil
}

func dependenciesFromServices(services dependencyServices) cli.Dependencies {
	return cli.Dependencies{
		Login: func(ctx context.Context, callbackInput io.Reader, notice io.Writer, pasteCallback bool) error {
			if !pasteCallback {
				callbackInput = nil
			}
			_, err := services.manager.Login(ctx, callbackInput, notice)
			return err
		},
		Status: func(ctx context.Context) (cli.StatusResult, error) {
			status, err := services.manager.Status(ctx)
			if err != nil {
				return cli.StatusResult{}, err
			}
			if !status.LoggedIn {
				return cli.StatusResult{}, apperr.Wrap(apperr.Auth, "status", "login required", errors.New("credentials are missing"))
			}
			user, err := services.identity.CurrentUser(ctx)
			if err != nil {
				return cli.StatusResult{}, err
			}
			refreshed, err := services.manager.Status(ctx)
			if err != nil {
				return cli.StatusResult{}, err
			}
			expires := refreshed.ExpiresAt
			return cli.StatusResult{LoggedIn: refreshed.LoggedIn, Server: refreshed.Server, Account: user.Account, ExpiresAt: &expires}, nil
		},
		List: func(ctx context.Context, path string) ([]cli.ItemResult, error) {
			items, err := services.remote.List(ctx, path)
			if err != nil {
				return nil, err
			}
			results := make([]cli.ItemResult, len(items))
			for index, item := range items {
				results[index] = itemResult(item)
			}
			return results, nil
		},
		Mkdir: func(ctx context.Context, path string, parents bool) (cli.ItemResult, error) {
			item, err := services.remote.Mkdir(ctx, path, parents)
			if err != nil {
				return cli.ItemResult{}, err
			}
			return itemResult(item), nil
		},
		Put: func(ctx context.Context, localPath, remotePath string, overwrite bool, progress upload.Progress) (cli.PutResult, error) {
			target, err := services.remote.ResolveUploadTarget(ctx, filepath.Base(localPath), remotePath)
			if err != nil {
				return cli.PutResult{}, err
			}
			result, err := services.uploader.Put(ctx, localPath, target, overwrite, progress)
			if err != nil {
				return cli.PutResult{}, err
			}
			return cli.PutResult{
				RemotePath: result.RemotePath, RemoteID: result.RemoteID, Revision: result.Revision, Size: result.Size,
				Resumed: result.Resumed, Instant: result.Instant, Multipart: result.Multipart, PartsTotal: result.PartsTotal,
				PartsResumed: result.PartsResumed, PartsUploaded: result.PartsUploaded,
			}, nil
		},
		Get: func(ctx context.Context, remotePath, localPath string, overwrite bool, progress upload.Progress) (cli.GetResult, error) {
			item, err := services.remote.Resolve(ctx, remotePath)
			if err != nil {
				return cli.GetResult{}, err
			}
			destination, err := downloadDestination(localPath, item.Name)
			if err != nil {
				return cli.GetResult{}, err
			}
			result, err := services.downloader.Get(ctx, item, destination, overwrite, progress)
			if err != nil {
				return cli.GetResult{}, err
			}
			return cli.GetResult{RemotePath: item.Path, RemoteID: result.RemoteID, Revision: result.Revision, LocalPath: result.LocalPath, Size: result.Size}, nil
		},
		OpenShare: func(ctx context.Context, rawLink string) (cli.ShareSession, error) {
			if services.shares == nil {
				return nil, apperr.Wrap(apperr.Local, "share link", "share dependency unavailable", errors.New("missing share dependency"))
			}
			session, err := services.shares.Open(ctx, rawLink)
			if err != nil {
				return nil, err
			}
			return &cliShareSession{session: session}, nil
		},
		Delete: func(ctx context.Context, remotePath string, recursive bool) (cli.DeleteResult, error) {
			result, err := services.remote.Delete(ctx, remotePath, recursive)
			if err != nil {
				return cli.DeleteResult{}, err
			}
			remoteID := result.Item.ID
			if remoteID == "" {
				remoteID = result.Item.DocID
			}
			return cli.DeleteResult{
				RemotePath: result.Item.Path, RemoteID: remoteID, Type: result.Item.Type, Status: string(result.Status),
				PendingReview: result.Status == anyshare.DeleteStatusPendingReview,
			}, nil
		},
		Logout: func(ctx context.Context, localOnly bool) error {
			return services.manager.Logout(ctx, localOnly)
		},
	}
}

func downloadDestination(localPath, remoteName string) (string, error) {
	if localPath == "" || strings.IndexByte(localPath, 0) >= 0 {
		return "", apperr.Wrap(apperr.Local, "download", "invalid local destination", errors.New("local path is empty or invalid"))
	}
	info, err := os.Lstat(localPath)
	if err == nil && info.IsDir() {
		if remoteName == "" || remoteName == "." || remoteName == ".." || filepath.Base(remoteName) != remoteName || strings.IndexByte(remoteName, 0) >= 0 {
			return "", apperr.Wrap(apperr.Local, "download", "invalid remote file name", errors.New("remote name cannot be used locally"))
		}
		return filepath.Join(localPath, remoteName), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", apperr.Wrap(apperr.Local, "download", "inspect local destination", err)
	}
	if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(localPath, string(filepath.Separator)) {
		return "", apperr.Wrap(apperr.Local, "download", "local destination directory does not exist", err)
	}
	return localPath, nil
}

func itemResult(item anyshare.Item) cli.ItemResult {
	remoteID := item.ID
	if remoteID == "" {
		remoteID = item.DocID
	}
	return cli.ItemResult{Name: item.Name, RemotePath: item.Path, Type: item.Type, RemoteID: remoteID, Size: item.Size, Modified: item.Modified}
}

func unavailableDependencies(cause error) cli.Dependencies {
	failure := func(ctx context.Context, operation string) error {
		if err := ctx.Err(); err != nil {
			return apperr.Wrap(apperr.Interrupted, operation, "interrupted", err)
		}
		return apperr.Wrap(apperr.Local, operation, "local dependency unavailable", cause)
	}
	return cli.Dependencies{
		Login:  func(ctx context.Context, _ io.Reader, _ io.Writer, _ bool) error { return failure(ctx, "login") },
		Status: func(ctx context.Context) (cli.StatusResult, error) { return cli.StatusResult{}, failure(ctx, "status") },
		List:   func(ctx context.Context, _ string) ([]cli.ItemResult, error) { return nil, failure(ctx, "list") },
		Mkdir: func(ctx context.Context, _ string, _ bool) (cli.ItemResult, error) {
			return cli.ItemResult{}, failure(ctx, "mkdir")
		},
		Put: func(ctx context.Context, _, _ string, _ bool, _ upload.Progress) (cli.PutResult, error) {
			return cli.PutResult{}, failure(ctx, "put")
		},
		Get: func(ctx context.Context, _, _ string, _ bool, _ upload.Progress) (cli.GetResult, error) {
			return cli.GetResult{}, failure(ctx, "get")
		},
		OpenShare: func(ctx context.Context, _ string) (cli.ShareSession, error) {
			return nil, failure(ctx, "share link")
		},
		Delete: func(ctx context.Context, _ string, _ bool) (cli.DeleteResult, error) {
			return cli.DeleteResult{}, failure(ctx, "rm")
		},
		Logout: func(ctx context.Context, _ bool) error { return failure(ctx, "logout") },
	}
}

type cliShareSession struct{ session *sharelink.Session }

func (session *cliShareSession) Root() cli.ShareEntry {
	if session == nil || session.session == nil {
		return cli.ShareEntry{}
	}
	return shareEntryResult(session.session.Root())
}

func (session *cliShareSession) List(ctx context.Context, relativeDir string) ([]cli.ShareEntry, error) {
	if session == nil || session.session == nil {
		return nil, apperr.Wrap(apperr.Local, "share link", "share dependency unavailable", errors.New("missing share session"))
	}
	entries, err := session.session.List(ctx, relativeDir)
	if err != nil {
		return nil, err
	}
	results := make([]cli.ShareEntry, len(entries))
	for index, entry := range entries {
		results[index] = shareEntryResult(entry)
	}
	return results, nil
}

func (session *cliShareSession) Download(ctx context.Context, relativeFiles []string, localDir string, overwrite bool, progress upload.Progress) ([]cli.ShareGetResult, error) {
	if session == nil || session.session == nil {
		return nil, apperr.Wrap(apperr.Local, "share link", "share dependency unavailable", errors.New("missing share session"))
	}
	downloaded, err := session.session.Download(ctx, relativeFiles, localDir, overwrite, progress)
	results := make([]cli.ShareGetResult, len(downloaded))
	for index, item := range downloaded {
		results[index] = cli.ShareGetResult{
			SharePath: item.Entry.Path,
			Revision:  item.Result.Revision,
			LocalPath: item.Result.LocalPath,
			Size:      item.Result.Size,
		}
	}
	return results, err
}

func shareEntryResult(entry sharelink.Entry) cli.ShareEntry {
	return cli.ShareEntry{Name: entry.Name, SharePath: entry.Path, Type: entry.Type, Size: entry.Size}
}
