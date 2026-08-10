// Package cli implements command dispatch for pku-drive.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/output"
	"github.com/Findddx/pku-drive-cli/internal/version"
)

var commandTimeouts = map[string]time.Duration{
	"login": 5 * time.Minute, "status": 2 * time.Minute, "ls": 2 * time.Minute,
	"mkdir": 2 * time.Minute, "put": 24 * time.Hour, "get": 24 * time.Hour,
	"rm": 2 * time.Minute, "logout": 2 * time.Minute,
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps Dependencies, info version.Info) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		return renderError(output.NewRenderer(stdout, stderr, false), usageError("command required"))
	}
	command := args[0]
	if _, ok := map[string]bool{"login": true, "status": true, "ls": true, "mkdir": true, "put": true, "get": true, "rm": true, "logout": true, "version": true}[command]; !ok {
		return renderError(output.NewRenderer(stdout, stderr, false), usageError("unknown command"))
	}

	allowed := map[string]bool{"json": true}
	if command == "login" {
		allowed["paste-callback"] = true
	}
	if command == "mkdir" {
		allowed["parents"] = true
	}
	if command == "put" {
		allowed["overwrite"], allowed["quiet"] = true, true
	}
	if command == "get" {
		allowed["overwrite"], allowed["quiet"] = true, true
	}
	if command == "rm" {
		allowed["recursive"] = true
	}
	if command == "logout" {
		allowed["local-only"] = true
	}
	reordered, err := reorderBoolFlags(args[1:], allowed)
	if err != nil {
		return renderError(output.NewRenderer(stdout, stderr, requestedJSON(args[1:])), err)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	jsonMode := flags.Bool("json", false, "emit JSON")
	var parents, overwrite, quiet, recursive, localOnly, pasteCallback *bool
	if command == "login" {
		pasteCallback = flags.Bool("paste-callback", false, "paste the browser callback URL")
	}
	if command == "mkdir" {
		parents = flags.Bool("parents", false, "create missing parents")
	}
	if command == "put" {
		overwrite = flags.Bool("overwrite", false, "overwrite existing file")
		quiet = flags.Bool("quiet", false, "suppress progress")
	}
	if command == "get" {
		overwrite = flags.Bool("overwrite", false, "overwrite existing file")
		quiet = flags.Bool("quiet", false, "suppress progress")
	}
	if command == "rm" {
		recursive = flags.Bool("recursive", false, "delete a directory tree")
	}
	if command == "logout" {
		localOnly = flags.Bool("local-only", false, "delete only local credentials")
	}
	if err := flags.Parse(reordered); err != nil {
		if _, writeErr := io.WriteString(stderr, output.RedactMessage(flagOutput.String())); writeErr != nil {
			return apperr.ExitCode(localError("write output"))
		}
		return renderError(output.NewRenderer(stdout, stderr, requestedJSON(args[1:])), usageError("invalid flags"))
	}
	renderer := output.NewRenderer(stdout, stderr, *jsonMode)
	positionals := flags.Args()
	if err := validateArity(command, positionals); err != nil {
		return renderError(renderer, err)
	}

	if command == "version" {
		if *jsonMode {
			return renderSuccess(renderer, "version", map[string]any{"version": info.Version, "commit": info.Commit, "build_date": info.BuildDate}, "")
		}
		_, err := fmt.Fprintf(stdout, "pku-drive %s (%s) %s\n", info.Version, info.Commit, info.BuildDate)
		if err != nil {
			return renderError(renderer, localError("write output"))
		}
		return 0
	}

	commandCtx, cancel := context.WithTimeout(ctx, commandTimeouts[command])
	defer cancel()
	switch command {
	case "login":
		if deps.Login == nil {
			err = localError("login dependency unavailable")
		} else {
			var callbackInput io.Reader
			if *pasteCallback {
				callbackInput = stdin
			}
			err = deps.Login(commandCtx, callbackInput, stderr, *pasteCallback)
		}
		if err == nil {
			return renderSuccess(renderer, command, nil, "Login successful.")
		}
	case "status":
		var result StatusResult
		if deps.Status == nil {
			err = localError("status dependency unavailable")
		} else {
			result, err = deps.Status(commandCtx)
		}
		if err == nil {
			fields := structFields(result)
			human := "Not logged in."
			if result.LoggedIn {
				human = fmt.Sprintf("Logged in as %s on %s.", result.Account, result.Server)
			}
			return renderSuccess(renderer, command, fields, human)
		}
	case "ls":
		path := "/"
		if len(positionals) == 1 {
			path = positionals[0]
		}
		var entries []ItemResult
		if deps.List == nil {
			err = localError("list dependency unavailable")
		} else {
			entries, err = deps.List(commandCtx, path)
		}
		if err == nil {
			if entries == nil {
				entries = make([]ItemResult, 0)
			}
			sort.SliceStable(entries, func(i, j int) bool {
				if entries[i].RemotePath == entries[j].RemotePath {
					return entries[i].Name < entries[j].Name
				}
				return entries[i].RemotePath < entries[j].RemotePath
			})
			lines := make([]string, len(entries))
			for i, entry := range entries {
				lines[i] = fmt.Sprintf("%s\t%s\t%d", entry.Type, entry.RemotePath, entry.Size)
			}
			return renderSuccess(renderer, command, map[string]any{"entries": entries}, strings.Join(lines, "\n"))
		}
	case "mkdir":
		var result ItemResult
		if deps.Mkdir == nil {
			err = localError("mkdir dependency unavailable")
		} else {
			result, err = deps.Mkdir(commandCtx, positionals[0], *parents)
		}
		if err == nil {
			return renderSuccess(renderer, command, structFields(result), "Directory ready: "+result.RemotePath)
		}
	case "put":
		var result PutResult
		progress := output.NewProgress(stderr, *quiet, interactiveWriter(stderr), time.Now)
		if deps.Put == nil {
			err = localError("put dependency unavailable")
		} else {
			result, err = deps.Put(commandCtx, positionals[0], positionals[1], *overwrite, progress)
		}
		if err == nil {
			progress.Finished()
			return renderSuccess(renderer, command, structFields(result), "Uploaded: "+result.RemotePath)
		}
		output.AbortProgress(progress)
	case "get":
		var result GetResult
		progress := output.NewDownloadProgress(stderr, *quiet, interactiveWriter(stderr), time.Now)
		if deps.Get == nil {
			err = localError("download dependency unavailable")
		} else {
			result, err = deps.Get(commandCtx, positionals[0], positionals[1], *overwrite, progress)
		}
		if err == nil {
			progress.Finished()
			return renderSuccess(renderer, command, structFields(result), "Downloaded: "+result.LocalPath)
		}
		output.AbortProgress(progress)
	case "rm":
		var result DeleteResult
		if deps.Delete == nil {
			err = localError("delete dependency unavailable")
		} else {
			result, err = deps.Delete(commandCtx, positionals[0], *recursive)
		}
		if err == nil {
			human := "Moved to recycle bin: " + result.RemotePath
			if result.PendingReview {
				human = "Deletion submitted for review: " + result.RemotePath
			}
			return renderSuccess(renderer, command, structFields(result), human)
		}
	case "logout":
		if deps.Logout == nil {
			err = localError("logout dependency unavailable")
		} else {
			err = deps.Logout(commandCtx, *localOnly)
		}
		if err == nil {
			return renderSuccess(renderer, command, nil, "Logged out.")
		}
	}
	if contextErr := commandCtx.Err(); contextErr != nil {
		err = apperr.Wrap(apperr.Interrupted, command, "interrupted", contextErr)
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		err = apperr.Wrap(apperr.Interrupted, command, "interrupted", err)
	}
	return renderError(renderer, err)
}

func reorderBoolFlags(args []string, allowed map[string]bool) ([]string, error) {
	flags, positional := make([]string, 0), make([]string, 0)
	seen := make(map[string]bool)
	afterDoubleDash := false
	for index, arg := range args {
		if afterDoubleDash {
			positional = append(positional, arg)
			continue
		}
		if arg == "--" {
			afterDoubleDash = true
			positional = append(positional, args[index+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if !strings.HasPrefix(arg, "--") {
				return nil, usageError("unknown flag: " + arg)
			}
			name := strings.TrimPrefix(strings.SplitN(arg, "=", 2)[0], "--")
			if !allowed[name] {
				return nil, usageError("unknown flag: --" + name)
			}
			if seen[name] {
				return nil, usageError("duplicate flag: --" + name)
			}
			seen[name] = true
			flags = append(flags, arg)
			continue
		}
		positional = append(positional, arg)
	}
	if afterDoubleDash {
		return append(append(flags, "--"), positional...), nil
	}
	return append(flags, positional...), nil
}

func requestedJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--json" || arg == "--json=true" {
			return true
		}
	}
	return false
}

func validateArity(command string, args []string) error {
	valid := false
	switch command {
	case "login", "status", "logout", "version":
		valid = len(args) == 0
	case "ls":
		valid = len(args) <= 1
	case "mkdir":
		valid = len(args) == 1
	case "put":
		valid = len(args) == 2
	case "get":
		valid = len(args) == 2
	case "rm":
		valid = len(args) == 1
	}
	if !valid {
		return usageError("invalid arguments for " + command)
	}
	return nil
}

func structFields(value any) map[string]any {
	// Round-tripping honors the public JSON field names and omitempty contract.
	var result map[string]any
	encoded, _ := jsonMarshal(value)
	_ = jsonUnmarshal(encoded, &result)
	return result
}

var jsonMarshal = func(value any) ([]byte, error) { return json.Marshal(value) }
var jsonUnmarshal = func(data []byte, value any) error { return json.Unmarshal(data, value) }

func interactiveWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func renderSuccess(renderer *output.Renderer, operation string, fields map[string]any, human string) int {
	if err := renderer.Success(operation, fields, human); err != nil {
		return apperr.ExitCode(localError("write output"))
	}
	return 0
}

func renderError(renderer *output.Renderer, err error) int {
	category, message := errorDetails(err)
	if renderErr := renderer.Failure(string(category), message); renderErr != nil {
		return apperr.ExitCode(localError("write output"))
	}
	return apperr.ExitCode(err)
}

func errorDetails(err error) (apperr.Category, string) {
	if err == nil {
		return apperr.Network, "operation failed"
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		message := appErr.Message
		if message == "" {
			message = string(appErr.Category) + " operation failed"
		}
		return appErr.Category, message
	}
	return apperr.Network, "operation failed"
}

func usageError(message string) error {
	return apperr.Wrap(apperr.Usage, "command", message, errors.New("invalid command line"))
}
func localError(message string) error {
	return apperr.Wrap(apperr.Local, "command", message, errors.New("local dependency unavailable"))
}
