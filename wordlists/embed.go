// Package wordlists embeds the default dictionaries into the binary, so the
// server runs from a single file with no external dependencies.
//
// To add a language, drop a "<code>.txt" file in this directory (one word or
// expression per line) and rebuild. To use lists from another directory
// without rebuilding, set WORDLIST_DIR.
package wordlists

import "embed"

// FS holds every *.txt file of this directory.
//
//go:embed *.txt
var FS embed.FS
