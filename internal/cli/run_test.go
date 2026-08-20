package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/cli"
	"github.com/Findddx/pku-drive-cli/internal/upload"
	"github.com/Findddx/pku-drive-cli/internal/version"
)

type calls struct {
	loginNotice  string
	listPath     string
	mkdirPath    string
	parents      bool
	localPath    string
	remotePath   string
	overwrite    bool
	getLocal     string
	getRemote    string
	getOverwrite bool
	deletePath   string
	recursive    bool
	localOnly    bool
	deadline     time.Duration
	progressSet  bool
	pasteLogin   bool
	loginInput   bool
	shareLink    string
	shareListDir string
	sharePaths   []string
	shareLocal   string
}

type shareSessionFake struct {
	root      cli.ShareEntry
	entries   []cli.ShareEntry
	downloads []cli.ShareGetResult
	calls     *calls
}

func (s *shareSessionFake) Root() cli.ShareEntry { return s.root }

func (s *shareSessionFake) List(_ context.Context, relativeDir string) ([]cli.ShareEntry, error) {
	s.calls.shareListDir = relativeDir
	return append([]cli.ShareEntry(nil), s.entries...), nil
}

func (s *shareSessionFake) Download(_ context.Context, paths []string, localDir string, overwrite bool, progress upload.Progress) ([]cli.ShareGetResult, error) {
	s.calls.sharePaths = append([]string(nil), paths...)
	s.calls.shareLocal = localDir
	s.calls.overwrite = overwrite
	if progress != nil {
		progress.Started(9)
		progress.Advanced(9)
		progress.Finished()
	}
	return append([]cli.ShareGetResult(nil), s.downloads...), nil
}

func dependencies(c *calls) cli.Dependencies {
	deps := cli.Dependencies{
		Login: func(ctx context.Context, input io.Reader, notice io.Writer, paste bool) error {
			_, _ = io.WriteString(notice, "Authorize at https://disk.example.invalid/oauth\n")
			c.loginNotice = "written"
			c.pasteLogin = paste
			c.loginInput = input != nil
			c.deadline = remaining(ctx)
			return nil
		},
		Status: func(ctx context.Context) (cli.StatusResult, error) {
			c.deadline = remaining(ctx)
			expires := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			return cli.StatusResult{LoggedIn: true, Server: "https://disk.pku.edu.cn", Account: "alice", ExpiresAt: &expires}, nil
		},
		List: func(ctx context.Context, path string) ([]cli.ItemResult, error) {
			c.listPath, c.deadline = path, remaining(ctx)
			return []cli.ItemResult{{Name: "z.txt", RemotePath: strings.TrimRight(path, "/") + "/z.txt", Type: "file", RemoteID: "doc-z", Size: 7, Modified: 99}}, nil
		},
		Mkdir: func(ctx context.Context, path string, parents bool) (cli.ItemResult, error) {
			c.mkdirPath, c.parents, c.deadline = path, parents, remaining(ctx)
			return cli.ItemResult{Name: "new", RemotePath: path, Type: "directory", RemoteID: "dir-1", Size: 0, Modified: 12}, nil
		},
		Put: func(ctx context.Context, local, remote string, overwrite bool, progress upload.Progress) (cli.PutResult, error) {
			c.localPath, c.remotePath, c.overwrite, c.deadline = local, remote, overwrite, remaining(ctx)
			c.progressSet = progress != nil
			if progress != nil {
				progress.Started(42)
				progress.Advanced(21)
				progress.Finished()
			}
			return cli.PutResult{RemotePath: remote, RemoteID: "doc-1", Revision: "r2", Size: 42, Resumed: true, Instant: false, Multipart: true, PartsTotal: 3, PartsResumed: 1, PartsUploaded: 2}, nil
		},
		Get: func(ctx context.Context, remote, local string, overwrite bool, progress upload.Progress) (cli.GetResult, error) {
			c.getRemote, c.getLocal, c.getOverwrite, c.deadline = remote, local, overwrite, remaining(ctx)
			c.progressSet = progress != nil
			if progress != nil {
				progress.Started(42)
				progress.Advanced(42)
			}
			return cli.GetResult{RemotePath: remote, RemoteID: "doc-get", Revision: "r3", LocalPath: local, Size: 42}, nil
		},
		Delete: func(ctx context.Context, path string, recursive bool) (cli.DeleteResult, error) {
			c.deletePath, c.recursive, c.deadline = path, recursive, remaining(ctx)
			return cli.DeleteResult{RemotePath: path, RemoteID: "doc-delete", Type: "directory", Status: "pending_review", PendingReview: true}, nil
		},
		Logout: func(ctx context.Context, localOnly bool) error {
			c.localOnly, c.deadline = localOnly, remaining(ctx)
			return nil
		},
	}
	deps.OpenShare = func(_ context.Context, link string) (cli.ShareSession, error) {
		c.shareLink = link
		return &shareSessionFake{
			root:      cli.ShareEntry{Name: "shared-root", SharePath: "", Type: "directory", Size: -1},
			entries:   []cli.ShareEntry{{Name: "a.tsv", SharePath: "数据/a.tsv", Type: "file", Size: 9}},
			downloads: []cli.ShareGetResult{{SharePath: "数据/a.tsv", Revision: "r1", LocalPath: "/tmp/out/数据/a.tsv", Size: 9}},
			calls:     c,
		}, nil
	}
	return deps
}

func TestRunShareListAndExplicitDownloadJSON(t *testing.T) {
	const link = "https://disk.pku.edu.cn/link/AA-ShareCapabilityMarker"

	t.Run("list share directory", func(t *testing.T) {
		c := new(calls)
		code, stdout, stderr := run(t, []string{"ls", "--share", link, "数据", "--json"}, dependencies(c))
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		got := decodeOne(t, stdout)
		entries, _ := got["entries"].([]any)
		if c.shareLink != link || c.shareListDir != "数据" || len(entries) != 1 {
			t.Fatalf("calls=%+v envelope=%v", c, got)
		}
		entry := entries[0].(map[string]any)
		if entry["share_path"] != "数据/a.tsv" || entry["type"] != "file" || entry["size"] != float64(9) {
			t.Fatalf("entry=%v", entry)
		}
		if strings.Contains(stdout, "ShareCapabilityMarker") {
			t.Fatalf("share capability leaked to stdout: %q", stdout)
		}
	})

	t.Run("download explicit shared files", func(t *testing.T) {
		c := new(calls)
		code, stdout, stderr := run(t, []string{"get", "--share", link, "/tmp/out", "数据/a.tsv", "--overwrite", "--quiet", "--json"}, dependencies(c))
		if code != 0 || stderr != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		got := decodeOne(t, stdout)
		downloads, _ := got["downloads"].([]any)
		if c.shareLink != link || c.shareLocal != "/tmp/out" || !c.overwrite || !reflect.DeepEqual(c.sharePaths, []string{"数据/a.tsv"}) || len(downloads) != 1 {
			t.Fatalf("calls=%+v envelope=%v", c, got)
		}
		result := downloads[0].(map[string]any)
		if result["share_path"] != "数据/a.tsv" || result["local_path"] != "/tmp/out/数据/a.tsv" || result["size"] != float64(9) {
			t.Fatalf("download=%v", result)
		}
		if strings.Contains(stdout, "ShareCapabilityMarker") {
			t.Fatalf("share capability leaked to stdout: %q", stdout)
		}
	})
}

func TestRunShareGetNonInteractiveSelectionRules(t *testing.T) {
	const link = "https://disk.pku.edu.cn/link/AA-ShareCapabilityMarker"

	t.Run("directory share with JSON requires paths", func(t *testing.T) {
		code, stdout, stderr := run(t, []string{"get", "--share", link, "/tmp/out", "--json"}, dependencies(new(calls)))
		if code != 2 || stderr != "" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		got := decodeOne(t, stdout)
		if got["category"] != "usage" || strings.Contains(stdout, "ShareCapabilityMarker") {
			t.Fatalf("envelope=%v", got)
		}
	})

	t.Run("single file share downloads without picker", func(t *testing.T) {
		c := new(calls)
		deps := dependencies(c)
		deps.OpenShare = func(_ context.Context, got string) (cli.ShareSession, error) {
			c.shareLink = got
			return &shareSessionFake{
				root:      cli.ShareEntry{Name: "single.tsv", SharePath: "single.tsv", Type: "file", Size: 9},
				downloads: []cli.ShareGetResult{{SharePath: "single.tsv", LocalPath: "/tmp/out/single.tsv", Size: 9}},
				calls:     c,
			}, nil
		}
		code, stdout, stderr := run(t, []string{"get", "--share", link, "/tmp/out", "--quiet", "--json"}, deps)
		if code != 0 || stderr != "" || !reflect.DeepEqual(c.sharePaths, []string{"single.tsv"}) {
			t.Fatalf("code=%d calls=%+v stdout=%q stderr=%q", code, c, stdout, stderr)
		}
	})
}

func TestRunShareHumanOutputEscapesUntrustedNames(t *testing.T) {
	const link = "https://disk.pku.edu.cn/link/AA-ShareCapabilityMarker"
	c := new(calls)
	deps := dependencies(c)
	deps.OpenShare = func(context.Context, string) (cli.ShareSession, error) {
		return &shareSessionFake{
			root:    cli.ShareEntry{Name: "root", Type: "directory", Size: -1},
			entries: []cli.ShareEntry{{Name: "unsafe", SharePath: "报告\x1b]8;;https://evil.invalid\a.tsv\nnext", Type: "file", Size: 9}},
			calls:   c,
		}, nil
	}
	code, stdout, stderr := run(t, []string{"ls", "--share", link}, deps)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.ContainsAny(stdout, "\x1b\a") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("unsafe terminal output: %q", stdout)
	}
	for _, want := range []string{`\x1B`, `\x07`, `\x0A`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout=%q missing %q", stdout, want)
		}
	}
}

func remaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline)
}

func run(t *testing.T, args []string, deps cli.Dependencies) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, deps, version.Info{Version: "0.1.0", Commit: "abc1234", BuildDate: "2026-08-09T00:00:00Z"})
	return code, stdout.String(), stderr.String()
}

func decodeOne(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode first JSON object: %v; output=%q", err, raw)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout has more than one JSON value: %q", raw)
	}
	return value
}

func TestRunAllCommandsJSONSuccess(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		operation string
		check     func(*testing.T, *calls, map[string]any, string)
	}{
		{"login", []string{"login", "--json"}, "login", func(t *testing.T, c *calls, _ map[string]any, stderr string) {
			if c.loginNotice == "" || !strings.Contains(stderr, "Authorize at https://disk.example.invalid/oauth") {
				t.Fatalf("notice not on stderr: calls=%+v stderr=%q", c, stderr)
			}
		}},
		{"status", []string{"status", "--json"}, "status", func(t *testing.T, _ *calls, got map[string]any, _ string) {
			if got["account"] != "alice" || got["logged_in"] != true || got["server"] != "https://disk.pku.edu.cn" || got["expires_at"] != "2026-08-09T12:00:00Z" {
				t.Fatalf("status=%v", got)
			}
		}},
		{"ls default", []string{"ls", "--json"}, "ls", func(t *testing.T, c *calls, got map[string]any, _ string) {
			if c.listPath != "/" {
				t.Fatalf("path=%q", c.listPath)
			}
			entries := got["entries"].([]any)
			if len(entries) != 1 {
				t.Fatalf("entries=%v", got["entries"])
			}
			entry := entries[0].(map[string]any)
			if entry["name"] != "z.txt" || entry["remote_path"] != "/z.txt" || entry["type"] != "file" || entry["remote_id"] != "doc-z" || entry["size"] != float64(7) || entry["modified"] != float64(99) {
				t.Fatalf("entries=%v", got["entries"])
			}
		}},
		{"mkdir", []string{"mkdir", "/docs/new", "--parents", "--json"}, "mkdir", func(t *testing.T, c *calls, got map[string]any, _ string) {
			if c.mkdirPath != "/docs/new" || !c.parents || got["name"] != "new" || got["remote_path"] != "/docs/new" || got["type"] != "directory" || got["remote_id"] != "dir-1" || got["size"] != float64(0) || got["modified"] != float64(12) {
				t.Fatalf("calls=%+v got=%v", c, got)
			}
		}},
		{"put", []string{"put", "local.bin", "/docs/file.bin", "--overwrite", "--quiet", "--json"}, "put", func(t *testing.T, c *calls, got map[string]any, _ string) {
			if c.localPath != "local.bin" || c.remotePath != "/docs/file.bin" || !c.overwrite || !c.progressSet || got["remote_path"] != "/docs/file.bin" || got["remote_id"] != "doc-1" || got["revision"] != "r2" || got["size"] != float64(42) || got["resumed"] != true || got["instant"] != false || got["parts_total"] != float64(3) || got["parts_resumed"] != float64(1) || got["parts_uploaded"] != float64(2) || got["multipart"] != true {
				t.Fatalf("calls=%+v got=%v", c, got)
			}
		}},
		{"get", []string{"get", "/docs/file.bin", "local.bin", "--overwrite", "--quiet", "--json"}, "get", func(t *testing.T, c *calls, got map[string]any, _ string) {
			if c.getRemote != "/docs/file.bin" || c.getLocal != "local.bin" || !c.getOverwrite || !c.progressSet || got["remote_path"] != "/docs/file.bin" || got["remote_id"] != "doc-get" || got["revision"] != "r3" || got["local_path"] != "local.bin" || got["size"] != float64(42) {
				t.Fatalf("calls=%+v got=%v", c, got)
			}
		}},
		{"rm", []string{"rm", "/docs/folder", "--recursive", "--json"}, "rm", func(t *testing.T, c *calls, got map[string]any, _ string) {
			if c.deletePath != "/docs/folder" || !c.recursive || got["remote_path"] != "/docs/folder" || got["remote_id"] != "doc-delete" || got["type"] != "directory" || got["status"] != "pending_review" || got["pending_review"] != true {
				t.Fatalf("calls=%+v got=%v", c, got)
			}
		}},
		{"logout", []string{"logout", "--local-only", "--json"}, "logout", func(t *testing.T, c *calls, _ map[string]any, _ string) {
			if !c.localOnly {
				t.Fatal("local-only not forwarded")
			}
		}},
		{"version", []string{"version", "--json"}, "version", func(t *testing.T, _ *calls, got map[string]any, _ string) {
			if got["version"] != "0.1.0" || got["commit"] != "abc1234" || got["build_date"] != "2026-08-09T00:00:00Z" {
				t.Fatalf("version=%v", got)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: dispatching to the wrong command, dropping a command argument/result field, or writing the final JSON payload to stderr.
			c := new(calls)
			code, stdout, stderr := run(t, test.args, dependencies(c))
			if code != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			got := decodeOne(t, stdout)
			if got["ok"] != true || got["operation"] != test.operation {
				t.Fatalf("envelope=%v", got)
			}
			test.check(t, c, got, stderr)
			if strings.Contains(stderr, `"ok"`) {
				t.Fatalf("JSON payload leaked to stderr: %q", stderr)
			}
		})
	}
}

func TestRunVersionHumanCompatibility(t *testing.T) {
	// Mutation caught: changing the established no-flag human version bytes.
	code, stdout, stderr := run(t, []string{"version"}, cli.Dependencies{})
	if code != 0 || stdout != "pku-drive 0.1.0 (abc1234) 2026-08-09T00:00:00Z\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunLoginReadsStdinOnlyInExplicitPasteMode(t *testing.T) {
	t.Run("default does not expose stdin", func(t *testing.T) {
		deps := dependencies(new(calls))
		deps.Login = func(_ context.Context, input io.Reader, _ io.Writer, paste bool) error {
			if input != nil || paste {
				t.Fatalf("default login input=%T paste=%v", input, paste)
			}
			return nil
		}
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"login"}, panicCallbackReader{}, &stdout, &stderr, deps, version.Info{})
		if code != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("paste flag forwards stdin", func(t *testing.T) {
		deps := dependencies(new(calls))
		deps.Login = func(_ context.Context, input io.Reader, _ io.Writer, paste bool) error {
			if input == nil || !paste {
				t.Fatalf("paste login input=%T paste=%v", input, paste)
			}
			got, err := io.ReadAll(input)
			if err != nil || string(got) != "fake-callback-input\n" {
				t.Fatalf("input=%q err=%v", got, err)
			}
			return nil
		}
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"login", "--paste-callback"}, strings.NewReader("fake-callback-input\n"), &stdout, &stderr, deps, version.Info{})
		if code != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

type panicCallbackReader struct{}

func (panicCallbackReader) Read([]byte) (int, error) { panic("default login read stdin") }

func TestRunBooleanFlagsAnywhere(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(*calls) bool
	}{
		{"mkdir before", []string{"mkdir", "--parents", "/a"}, func(c *calls) bool { return c.parents && c.mkdirPath == "/a" }},
		{"mkdir after", []string{"mkdir", "/a", "--parents"}, func(c *calls) bool { return c.parents && c.mkdirPath == "/a" }},
		{"mkdir explicit false", []string{"mkdir", "/a", "--parents=false"}, func(c *calls) bool { return !c.parents && c.mkdirPath == "/a" }},
		{"put before", []string{"put", "--overwrite", "local", "/a"}, func(c *calls) bool { return c.overwrite && c.localPath == "local" && c.remotePath == "/a" }},
		{"put between", []string{"put", "local", "--overwrite=true", "/a"}, func(c *calls) bool { return c.overwrite && c.remotePath == "/a" }},
		{"put after", []string{"put", "local", "/a", "--overwrite"}, func(c *calls) bool { return c.overwrite && c.remotePath == "/a" }},
		{"overwrite explicit false", []string{"put", "local", "/a", "--overwrite=false"}, func(c *calls) bool { return !c.overwrite && c.remotePath == "/a" }},
		{"quiet before", []string{"put", "--quiet", "local", "/a"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"quiet between", []string{"put", "local", "--quiet=true", "/a"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"quiet after", []string{"put", "local", "/a", "--quiet=false"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"json before", []string{"put", "--json", "local", "/a"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"json between", []string{"put", "local", "--json=true", "/a"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"json after", []string{"put", "local", "/a", "--json=false"}, func(c *calls) bool { return c.remotePath == "/a" }},
		{"get overwrite before", []string{"get", "--overwrite", "/a", "local"}, func(c *calls) bool { return c.getOverwrite && c.getRemote == "/a" && c.getLocal == "local" }},
		{"get quiet after", []string{"get", "/a", "local", "--quiet"}, func(c *calls) bool { return c.getRemote == "/a" && c.getLocal == "local" }},
		{"rm recursive after", []string{"rm", "/a", "--recursive"}, func(c *calls) bool { return c.recursive && c.deletePath == "/a" }},
		{"logout explicit false", []string{"logout", "--local-only=false"}, func(c *calls) bool { return !c.localOnly }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: letting flag.FlagSet stop at the first positional or mishandling --flag=false.
			c := new(calls)
			code, _, stderr := run(t, test.args, dependencies(c))
			if code != 0 || !test.check(c) {
				t.Fatalf("code=%d calls=%+v stderr=%q", code, c, stderr)
			}
		})
	}
}

func TestRunDoubleDashEndsFlagRecognition(t *testing.T) {
	// Mutation caught: treating a flag-looking positional after -- as an option.
	c := new(calls)
	code, _, stderr := run(t, []string{"ls", "--", "--json"}, dependencies(c))
	if code != 0 || c.listPath != "--json" {
		t.Fatalf("code=%d path=%q stderr=%q", code, c.listPath, stderr)
	}
}

func TestRunDoubleDashPreservesEarlierAndFlagLookingPositionals(t *testing.T) {
	// Mutation caught: dropping/reordering operands around -- or recognizing a following overwrite flag.
	c := new(calls)
	code, _, stderr := run(t, []string{"put", "local", "--", "--overwrite"}, dependencies(c))
	if code != 0 || c.localPath != "local" || c.remotePath != "--overwrite" || c.overwrite {
		t.Fatalf("code=%d calls=%+v stderr=%q", code, c, stderr)
	}
}

func TestRunSortsListEntriesForDeterministicJSON(t *testing.T) {
	// Mutation caught: forwarding dependency iteration order into the public entries array.
	deps := dependencies(new(calls))
	deps.List = func(context.Context, string) ([]cli.ItemResult, error) {
		return []cli.ItemResult{{Name: "z", RemotePath: "/z"}, {Name: "a", RemotePath: "/a"}}, nil
	}
	code, stdout, stderr := run(t, []string{"ls", "--json"}, deps)
	got := decodeOne(t, stdout)
	entries := got["entries"].([]any)
	if code != 0 || entries[0].(map[string]any)["name"] != "a" {
		t.Fatalf("code=%d entries=%v stderr=%q", code, entries, stderr)
	}
}

func TestRunEmptyListIsJSONArray(t *testing.T) {
	// Mutation caught: exposing a nil dependency slice as JSON null instead of an iterable entries array.
	deps := dependencies(new(calls))
	deps.List = func(context.Context, string) ([]cli.ItemResult, error) { return nil, nil }
	code, stdout, stderr := run(t, []string{"ls", "--json"}, deps)
	got := decodeOne(t, stdout)
	entries, ok := got["entries"].([]any)
	if code != 0 || !ok || len(entries) != 0 {
		t.Fatalf("code=%d entries=%#v stderr=%q", code, got["entries"], stderr)
	}
}

func TestRunUsageErrors(t *testing.T) {
	tests := [][]string{
		{}, {"download"}, {"login", "extra"}, {"status", "extra"}, {"ls", "a", "b"}, {"mkdir"}, {"mkdir", "a", "b"},
		{"put", "local"}, {"put", "local", "remote", "extra"}, {"get", "remote"}, {"get", "remote", "local", "extra"}, {"rm"}, {"rm", "a", "b"}, {"logout", "extra"}, {"version", "extra"},
		{"ls", "--quiet"}, {"status", "--paste-callback"}, {"mkdir", "--overwrite", "/a"}, {"put", "--parents", "a", "b"}, {"get", "--recursive", "a", "b"}, {"rm", "--overwrite", "/a"},
		{"mkdir", "--parents", "--parents", "/a"}, {"status", "--json", "--json"}, {"put", "--quiet=maybe", "a", "b"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// Mutation caught: accepting missing/extra operands, unknown flags, duplicate flags, or malformed boolean values.
			code, stdout, stderr := run(t, args, dependencies(new(calls)))
			jsonRequested := false
			for _, arg := range args {
				jsonRequested = jsonRequested || arg == "--json" || arg == "--json=true"
			}
			if jsonRequested {
				if code != 2 || decodeOne(t, stdout)["category"] != "usage" || strings.Contains(stderr, `"ok"`) {
					t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
				}
				return
			}
			if code != 2 || stdout != "" || stderr == "" {
				t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
			}
		})
	}
}

func TestRunRedactsMalformedFlagDiagnostics(t *testing.T) {
	tests := []struct {
		name, value string
		forbidden   []string
	}{
		{"token", "access_token=fake-flag-token", []string{"fake-flag-token"}},
		{"authorization parameters", "Authorization=Digest username=fake-flag-user, response=fake-flag-authorization", []string{"fake-flag-user", "fake-flag-authorization"}},
		{"multiple cookies", "Cookie=session=fake-flag-cookie-one; refresh=fake-flag-cookie-two", []string{"fake-flag-cookie-one", "fake-flag-cookie-two"}},
		{"signed URL", "https://objects.invalid/a?X-Amz-Signature=fake-flag-signature", []string{"fake-flag-signature", "objects.invalid"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: allowing flag.FlagSet to echo a secret-bearing invalid boolean value directly to stderr.
			code, stdout, stderr := run(t, []string{"put", "local", "/remote", "--json", "--quiet=" + test.value}, dependencies(new(calls)))
			if code != 2 || decodeOne(t, stdout)["category"] != "usage" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(stderr, forbidden) {
					t.Fatalf("secret %q leaked in stderr=%q", forbidden, stderr)
				}
			}
		})
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRunFailureOutputWriteErrorsAreLocal(t *testing.T) {
	tests := []struct {
		name string
		json bool
	}{{"human stderr", false}, {"JSON stdout", true}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: discarding a failed final error render and returning the service error's exit code.
			deps := dependencies(new(calls))
			deps.Status = func(context.Context) (cli.StatusResult, error) {
				return cli.StatusResult{}, apperr.Wrap(apperr.Auth, "status", "login required", errors.New("missing"))
			}
			var good bytes.Buffer
			stdout, stderr := io.Writer(&good), io.Writer(&good)
			args := []string{"status"}
			if test.json {
				args = append(args, "--json")
				stdout = errorWriter{}
			} else {
				stderr = errorWriter{}
			}
			code := cli.Run(context.Background(), args, strings.NewReader(""), stdout, stderr, deps, version.Info{})
			if code != 6 {
				t.Fatalf("code=%d, want Local/6", code)
			}
		})
	}
}

func TestRunFailuresHaveStableExitAndMinimalJSON(t *testing.T) {
	categories := []struct {
		category apperr.Category
		code     int
	}{{apperr.Usage, 2}, {apperr.Auth, 3}, {apperr.Remote, 4}, {apperr.Network, 5}, {apperr.Local, 6}, {apperr.Integrity, 7}, {apperr.Interrupted, 130}}
	for _, test := range categories {
		t.Run(string(test.category), func(t *testing.T) {
			// Mutation caught: mapping an application category to the wrong exit code or exposing operation/raw error fields in JSON.
			deps := dependencies(new(calls))
			deps.Status = func(context.Context) (cli.StatusResult, error) {
				return cli.StatusResult{}, apperr.Wrap(test.category, "status", "safe message", errors.New("raw nested body"))
			}
			code, stdout, stderr := run(t, []string{"status", "--json"}, deps)
			if code != test.code || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			got := decodeOne(t, stdout)
			if len(got) != 3 || got["ok"] != false || got["category"] != string(test.category) || got["message"] != "safe message" {
				t.Fatalf("failure=%v", got)
			}
			if strings.Contains(stdout, "raw nested body") || strings.Contains(stdout, "status") {
				t.Fatalf("unsafe failure=%q", stdout)
			}
		})
	}
}

func TestRunHumanSuccessAndErrorStreams(t *testing.T) {
	// Mutation caught: sending human success to stderr or human errors to stdout.
	code, stdout, stderr := run(t, []string{"status"}, dependencies(new(calls)))
	if code != 0 || stdout == "" || stderr != "" {
		t.Fatalf("success code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	deps := dependencies(new(calls))
	deps.Status = func(context.Context) (cli.StatusResult, error) {
		return cli.StatusResult{}, apperr.Wrap(apperr.Auth, "status", "login required", errors.New("secret"))
	}
	code, stdout, stderr = run(t, []string{"status"}, deps)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "login required") || strings.Contains(stderr, "secret") {
		t.Fatalf("failure code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunCommandDeadlines(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want time.Duration
	}{
		{"login", []string{"login"}, 5 * time.Minute}, {"status", []string{"status"}, 2 * time.Minute}, {"list", []string{"ls"}, 2 * time.Minute},
		{"mkdir", []string{"mkdir", "/a"}, 2 * time.Minute}, {"put", []string{"put", "a", "/a"}, 24 * time.Hour},
		{"get", []string{"get", "/a", "a"}, 24 * time.Hour}, {"rm", []string{"rm", "/a"}, 2 * time.Minute}, {"logout", []string{"logout"}, 2 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: applying the wrong command deadline or omitting the deadline child context.
			c := new(calls)
			code, _, stderr := run(t, test.args, dependencies(c))
			if code != 0 || c.deadline < test.want-time.Second || c.deadline > test.want {
				t.Fatalf("deadline=%s want approximately %s; stderr=%q", c.deadline, test.want, stderr)
			}
		})
	}
}

func TestRunPreservesParentCancellation(t *testing.T) {
	// Mutation caught: replacing rather than deriving from the incoming context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps := dependencies(new(calls))
	deps.Status = func(ctx context.Context) (cli.StatusResult, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("child context err=%v", ctx.Err())
		}
		return cli.StatusResult{}, ctx.Err()
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, []string{"status"}, strings.NewReader(""), &stdout, &stderr, deps, version.Info{})
	if code != 130 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(syscall.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatalf("unlock ptmx: %v", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(syscall.TIOCGPTN), uintptr(unsafe.Pointer(&number))); errno != 0 {
		t.Fatalf("get pts number: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func readPTY(t *testing.T, master, slave *os.File) string {
	t.Helper()
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	buffer := make([]byte, 1024)
	for {
		n, err := master.Read(buffer)
		captured.Write(buffer[:n])
		if err == nil {
			continue
		}
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EIO) || errors.Is(err, io.EOF) {
			break
		}
		t.Fatal(err)
	}
	return captured.String()
}

func TestRunPutSuppressionPreservesFinalSuccessAndFailure(t *testing.T) {
	tests := []struct {
		name        string
		quiet       bool
		interactive bool
		jsonMode    bool
		fail        bool
	}{
		{name: "quiet interactive human success", quiet: true, interactive: true},
		{name: "quiet interactive human failure", quiet: true, interactive: true, fail: true},
		{name: "noninteractive JSON success", jsonMode: true},
		{name: "noninteractive JSON failure", jsonMode: true, fail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: dropping the Progress object, ignoring quiet/noninteractive suppression, or suppressing the final command result/error with progress.
			c := new(calls)
			deps := dependencies(c)
			if test.fail {
				deps.Put = func(_ context.Context, _, remotePath string, _ bool, progress upload.Progress) (cli.PutResult, error) {
					c.progressSet = progress != nil
					if progress != nil {
						progress.Started(42)
						progress.Advanced(21)
						progress.Finished()
					}
					return cli.PutResult{}, apperr.Wrap(apperr.Remote, "put", "upload failed", errors.New("fake remote failure"))
				}
			}
			args := []string{"put", "local.bin", "/remote.bin"}
			if test.quiet {
				args = append(args, "--quiet")
			}
			if test.jsonMode {
				args = append(args, "--json")
			}
			var stdout, pipeStderr bytes.Buffer
			var stderr io.Writer = &pipeStderr
			var master, slave *os.File
			if test.interactive {
				master, slave = openPTY(t)
				stderr = slave
			}
			code := cli.Run(context.Background(), args, strings.NewReader(""), &stdout, stderr, deps, version.Info{})
			capturedStderr := pipeStderr.String()
			if test.interactive {
				capturedStderr = readPTY(t, master, slave)
			}
			if !c.progressSet {
				t.Fatal("Put did not receive a Progress object")
			}
			if strings.Contains(capturedStderr, "Uploaded ") || strings.Contains(capturedStderr, "bytes") {
				t.Fatalf("periodic progress leaked to stderr=%q", capturedStderr)
			}
			if test.fail {
				if code != 4 {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), capturedStderr)
				}
				if test.jsonMode {
					failure := decodeOne(t, stdout.String())
					if failure["ok"] != false || failure["category"] != "remote" || capturedStderr != "" {
						t.Fatalf("failure=%v stderr=%q", failure, capturedStderr)
					}
				} else if stdout.Len() != 0 || !strings.Contains(capturedStderr, "upload failed") {
					t.Fatalf("stdout=%q stderr=%q", stdout.String(), capturedStderr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), capturedStderr)
			}
			if test.jsonMode {
				success := decodeOne(t, stdout.String())
				if success["ok"] != true || success["operation"] != "put" || capturedStderr != "" {
					t.Fatalf("success=%v stderr=%q", success, capturedStderr)
				}
			} else if stdout.String() != "Uploaded: /remote.bin\n" || capturedStderr != "" {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), capturedStderr)
			}
		})
	}
}

func TestRunGetFailureDoesNotReportCompleteProgress(t *testing.T) {
	deps := dependencies(new(calls))
	deps.Get = func(_ context.Context, _, _ string, _ bool, progress upload.Progress) (cli.GetResult, error) {
		progress.Started(10)
		progress.Advanced(4)
		return cli.GetResult{}, apperr.Wrap(apperr.Network, "get", "download failed", errors.New("fixture failure"))
	}
	master, slave := openPTY(t)
	defer master.Close()
	var stdout bytes.Buffer
	code := cli.Run(context.Background(), []string{"get", "/remote.bin", "local.bin"}, strings.NewReader(""), &stdout, slave, deps, version.Info{})
	stderr := readPTY(t, master, slave)
	if code != 5 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr)
	}
	if !strings.Contains(stderr, "Downloaded 4/10 bytes") || strings.Contains(stderr, "Downloaded 10/10 bytes") {
		t.Fatalf("false completion in stderr=%q", stderr)
	}
}
