package license

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// maxLicenseFile bounds licence files read; real ones are well under this.
const maxLicenseFile = 256 << 10

// textRules recognise licence texts by distinctive phrases, most specific
// first: the AGPL contains the GPL's wording and LGPL-3.0 refers to GPL-3.0.
var textRules = []struct {
	id      string
	phrases []string
}{
	{"AGPL-3.0", []string{"GNU AFFERO GENERAL PUBLIC LICENSE"}},
	{"LGPL-3.0", []string{"GNU LESSER GENERAL PUBLIC LICENSE", "Version 3"}},
	{"LGPL-2.1", []string{"GNU LESSER GENERAL PUBLIC LICENSE", "Version 2.1"}},
	{"LGPL-2.0", []string{"GNU LIBRARY GENERAL PUBLIC LICENSE"}},
	{"GPL-3.0", []string{"GNU GENERAL PUBLIC LICENSE", "Version 3"}},
	{"GPL-2.0", []string{"GNU GENERAL PUBLIC LICENSE", "Version 2"}},
	{"SSPL-1.0", []string{"Server Side Public License"}},
	{"BUSL-1.1", []string{"Business Source License 1.1"}},
	{"Elastic-2.0", []string{"Elastic License 2.0"}},
	{"MPL-2.0", []string{"Mozilla Public License Version 2.0"}},
	{"MPL-2.0", []string{"Mozilla Public License, version 2.0"}},
	{"EPL-2.0", []string{"Eclipse Public License - v 2.0"}},
	{"EPL-1.0", []string{"Eclipse Public License - v 1.0"}},
	{"Apache-2.0", []string{"Apache License", "Version 2.0"}},
	{"BSL-1.0", []string{"Boost Software License - Version 1.0"}},
	{"Unlicense", []string{"This is free and unencumbered software released into the public domain"}},
	{"CC0-1.0", []string{"CC0 1.0 Universal"}},
	{"BlueOak-1.0.0", []string{"Blue Oak Model License"}},
	{"0BSD", []string{"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted.", "THE SOFTWARE IS PROVIDED \"AS IS\" AND THE AUTHOR DISCLAIMS"}},
	{"ISC", []string{"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted, provided that the above copyright notice"}},
	{"ISC", []string{"Permission to use, copy, modify, and distribute this software for any purpose with or without fee is hereby granted, provided that the above copyright notice"}},
	{"MIT", []string{"Permission is hereby granted, free of charge, to any person obtaining a copy"}},
	{"BSD-3-Clause", []string{"Redistribution and use in source and binary forms", "Neither the name"}},
	{"BSD-3-Clause", []string{"Redistribution and use in source and binary forms", "names of its contributors may"}},
	{"BSD-2-Clause", []string{"Redistribution and use in source and binary forms"}},
	{"Zlib", []string{"This software is provided 'as-is', without any express or implied warranty", "Altered source versions must be plainly marked"}},
}

var spaces = regexp.MustCompile(`\s+`)

// Detect returns the SPDX identifier of a licence text, or "" when it is not
// recognised. A leading SPDX-License-Identifier line wins.
func Detect(text []byte) string {
	if i := bytes.Index(text, []byte("SPDX-License-Identifier:")); i >= 0 && i < 2048 {
		line, _, _ := bytes.Cut(text[i+len("SPDX-License-Identifier:"):], []byte("\n"))
		if id := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(line)), "*/")); id != "" {
			return id
		}
	}
	norm := spaces.ReplaceAllString(string(text), " ")
	for _, r := range textRules {
		all := true
		for _, p := range r.phrases {
			if !strings.Contains(norm, p) {
				all = false
				break
			}
		}
		if all {
			return r.id
		}
	}
	return ""
}

// licenseFile matches the usual licence file names.
var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|unlicense)([.-][a-z0-9.-]*)?$`)

// FromDir detects the licence from the licence files at the top of dir and
// returns the SPDX expression and the file names it used. Several permissive
// files (LICENSE-MIT plus LICENSE-APACHE) are a choice and joined with OR;
// any other mix applies together and is joined with AND.
func FromDir(dir string) (string, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil
	}
	var found, files []string
	for _, e := range entries {
		if !e.Type().IsRegular() || !licenseFile.MatchString(e.Name()) {
			continue
		}
		id := detectFile(filepath.Join(dir, e.Name()))
		if id == "" {
			continue
		}
		files = append(files, e.Name())
		if !slices.Contains(found, id) {
			found = append(found, id)
		}
	}
	switch len(found) {
	case 0:
		return "", files
	case 1:
		return found[0], files
	}
	op := " AND "
	allPermissive := true
	for _, id := range found {
		if Of(id) != Permissive {
			allPermissive = false
		}
	}
	if allPermissive {
		op = " OR " // LICENSE-MIT + LICENSE-APACHE is a choice
	}
	return strings.Join(found, op), files
}

func detectFile(p string) string {
	fi, err := os.Lstat(p) //nolint:gosec // p is a directory entry of a dir we chose; Lstat rejects symlinks below
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxLicenseFile {
		return ""
	}
	b, err := os.ReadFile(p) //nolint:gosec // licence file found in a directory listing, symlinks rejected above
	if err != nil {
		return ""
	}
	return Detect(b)
}

var pyprojectLicense = regexp.MustCompile(`(?m)^\s*license\s*=\s*(?:"([^"]+)"|\{\s*text\s*=\s*"([^"]+)"\s*\})`)

// Project returns the licence of the repository at root: licence files at the
// top, then the "license" field of package.json, then pyproject.toml.
func Project(root string) Found {
	if id, files := FromDir(root); id != "" {
		return Found{License: id, Source: strings.Join(files, ", ")}
	}
	if b, err := readSmall(filepath.Join(root, "package.json")); err == nil {
		if id := NPMField(b); id != "" {
			return Found{License: id, Source: "package.json"}
		}
	}
	if b, err := readSmall(filepath.Join(root, "pyproject.toml")); err == nil {
		if m := pyprojectLicense.FindSubmatch(b); m != nil {
			if id := Normalize(string(m[1]) + string(m[2])); Classify(id) != Unknown {
				return Found{License: id, Source: "pyproject.toml"}
			}
		}
	}
	return Found{}
}
