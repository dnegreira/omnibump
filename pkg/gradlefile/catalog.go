/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package gradlefile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// CatalogFile is a parsed gradle/libs.versions.toml version catalog. The
// TOML structure is read with a real TOML parser; edits are line-targeted
// splices on the raw text so formatting, comments and key alignment survive.
type CatalogFile struct {
	path      string
	buf       editBuffer
	versions  []CatalogVersion
	libraries []CatalogLibrary
}

// ParseCatalog parses a libs.versions.toml version catalog.
func ParseCatalog(path string, content []byte) (*CatalogFile, error) {
	var catalog struct {
		Versions  map[string]any `toml:"versions"`
		Libraries map[string]any `toml:"libraries"`
	}
	if err := toml.Unmarshal(content, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse TOML catalog %s: %w", path, err)
	}

	f := &CatalogFile{
		path: path,
		buf:  editBuffer{original: content},
	}
	f.collectVersions(catalog.Versions)
	f.collectLibraries(catalog.Libraries)
	return f, nil
}

// Path returns the file path the catalog was parsed from.
func (f *CatalogFile) Path() string { return f.path }

// Versions returns the [versions] entries with string values.
func (f *CatalogFile) Versions() []CatalogVersion { return f.versions }

// Libraries returns the [libraries] entries with resolvable coordinates.
func (f *CatalogFile) Libraries() []CatalogLibrary { return f.libraries }

// SetVersion queues a rewrite of the [versions] entry named key.
func (f *CatalogFile) SetVersion(key, value string) error {
	if err := ValidateVersion(value); err != nil {
		return err
	}
	for _, v := range f.versions {
		if v.Key != key {
			continue
		}
		if !v.valueSpan.valid() {
			return fmt.Errorf("%w: catalog version %s in %s", ErrNotEditable, key, f.path)
		}
		return f.buf.add(v.valueSpan, value)
	}
	return fmt.Errorf("%w: catalog version %s in %s", ErrNotEditable, key, f.path)
}

// SetLibraryVersion queues a rewrite of lib's inline version literal.
// Libraries that use version.ref are updated through SetVersion on the
// referenced key instead.
func (f *CatalogFile) SetLibraryVersion(lib CatalogLibrary, value string) error {
	if err := ValidateVersion(value); err != nil {
		return err
	}
	if lib.VersionRef != "" || !lib.versionSpan.valid() {
		return fmt.Errorf("%w: catalog library %s in %s has no inline version", ErrNotEditable, lib.Alias, f.path)
	}
	return f.buf.add(lib.versionSpan, value)
}

// Content renders the catalog with all queued edits applied.
func (f *CatalogFile) Content() []byte { return f.buf.render() }

// Changed reports whether any queued edit modifies the original content.
func (f *CatalogFile) Changed() bool { return f.buf.changed() }

// ChangeCount returns the number of queued edits that modify the content.
func (f *CatalogFile) ChangeCount() int { return f.buf.changeCount() }

// collectVersions records [versions] entries with string values, locating
// each entry's value span in the raw text.
func (f *CatalogFile) collectVersions(versions map[string]any) {
	section, ok := tomlSectionSpan(f.buf.original, "versions")
	if !ok {
		return
	}
	keys := make([]string, 0, len(versions))
	for key := range versions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, isString := versions[key].(string)
		if !isString {
			// Rich versions ({ strictly = "..." }) are recorded without an
			// editable span; resolution can still route through other tiers.
			continue
		}
		f.versions = append(f.versions, CatalogVersion{
			Key:       key,
			Value:     value,
			valueSpan: tomlValueSpan(f.buf.original, section, key, value),
		})
	}
}

// collectLibraries records [libraries] entries that declare coordinates via
// module = "g:a" or group/name pairs, with either version.ref or an inline
// version string.
func (f *CatalogFile) collectLibraries(libraries map[string]any) {
	section, _ := tomlSectionSpan(f.buf.original, "libraries")
	aliases := make([]string, 0, len(libraries))
	for alias := range libraries {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		entry, isTable := libraries[alias].(map[string]any)
		if !isTable {
			continue
		}
		lib, ok := parseLibraryEntry(alias, entry)
		if !ok {
			continue
		}
		if lib.Version != "" {
			lib.versionSpan = tomlInlineLibraryVersionSpan(f.buf.original, section, alias, lib.Version)
		}
		f.libraries = append(f.libraries, lib)
	}
}

// parseLibraryEntry extracts coordinates and version information from one
// [libraries] table entry.
func parseLibraryEntry(alias string, entry map[string]any) (CatalogLibrary, bool) {
	lib := CatalogLibrary{Alias: alias, versionSpan: span{-1, -1}}

	if module, ok := entry["module"].(string); ok {
		parts := strings.SplitN(module, ":", 2)
		if len(parts) != 2 {
			return lib, false
		}
		lib.Group, lib.Artifact = parts[0], parts[1]
	} else {
		group, hasGroup := entry["group"].(string)
		name, hasName := entry["name"].(string)
		if !hasGroup || !hasName {
			return lib, false
		}
		lib.Group, lib.Artifact = group, name
	}

	switch version := entry["version"].(type) {
	case string:
		lib.Version = version
	case map[string]any:
		if ref, ok := version["ref"].(string); ok {
			lib.VersionRef = ref
		}
	}
	return lib, true
}

// tomlSectionSpan returns the span of a top-level TOML section body (from
// the line after the [name] header to the next section header or EOF).
func tomlSectionSpan(content []byte, name string) (span, bool) {
	header := regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(name) + `\]\s*$`)
	m := header.FindIndex(content)
	if m == nil {
		return span{-1, -1}, false
	}
	next := regexp.MustCompile(`(?m)^\[`)
	rest := content[m[1]:]
	if n := next.FindIndex(rest); n != nil {
		return span{m[1], m[1] + n[0]}, true
	}
	return span{m[1], len(content)}, true
}

// tomlValueSpan locates the quoted value of `key = "value"` within section.
func tomlValueSpan(content []byte, section span, key, value string) span {
	if !section.valid() {
		return span{-1, -1}
	}
	pattern := regexp.MustCompile(`(?m)^\s*["']?` + regexp.QuoteMeta(key) + `["']?\s*=\s*["']` + regexp.QuoteMeta(value) + `["']`)
	m := pattern.FindIndex(content[section.start:section.end])
	if m == nil {
		return span{-1, -1}
	}
	line := content[section.start+m[0] : section.start+m[1]]
	offset := strings.LastIndex(string(line), value)
	if offset < 0 {
		return span{-1, -1}
	}
	start := section.start + m[0] + offset
	return span{start, start + len(value)}
}

// tomlInlineLibraryVersionSpan locates the inline version literal of a
// [libraries] entry declared as a single-line inline table, e.g.
//
//	okio = { module = "com.squareup.okio:okio", version = "3.4.0" }
func tomlInlineLibraryVersionSpan(content []byte, section span, alias, version string) span {
	if !section.valid() {
		return span{-1, -1}
	}
	pattern := regexp.MustCompile(`(?m)^\s*["']?` + regexp.QuoteMeta(alias) + `["']?\s*=\s*\{[^\n]*version\s*=\s*["'](` + regexp.QuoteMeta(version) + `)["']`)
	m := pattern.FindSubmatchIndex(content[section.start:section.end])
	if m == nil {
		return span{-1, -1}
	}
	return span{section.start + m[2], section.start + m[3]}
}
