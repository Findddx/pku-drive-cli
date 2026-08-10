// Package version defines build metadata shown by the CLI.
package version

// Info holds build metadata for the pku-drive command.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}
