package license

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		expr string
		want Category
	}{
		{"MIT", Permissive},
		{"Apache 2.0", Permissive},
		{"apache-2.0", Permissive},
		{"BSD", Permissive},
		{"CC0-1.0", PublicDomain},
		{"MPL-2.0", WeakCopyleft},
		{"LGPL-2.1-or-later", WeakCopyleft},
		{"GPL-3.0-only", StrongCopyleft},
		{"GPL-2.0+", StrongCopyleft},
		{"AGPL-3.0-or-later", NetworkCopyleft},
		{"BUSL-1.1", Restricted},
		{"UNLICENSED", Restricted},
		{"MIT OR GPL-3.0", Permissive},
		{"(BSD-3-Clause OR GPL-2.0)", Permissive},
		{"MIT AND GPL-3.0", StrongCopyleft},
		{"(MIT OR Apache-2.0) AND MPL-2.0", WeakCopyleft},
		{"GPL-2.0 WITH Classpath-exception-2.0", WeakCopyleft},
		{"Some Custom License", Unknown},
		{"", Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			assert.Equal(t, Classify(tt.expr), tt.want)
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name, project, dep string
		dev                bool
		want               string
		reason             string
	}{
		{"permissive in apache", "Apache-2.0", "MIT", false, OK, "permissive"},
		{"gpl in apache", "Apache-2.0", "GPL-3.0", false, Conflict, "strong copyleft"},
		{"gpl dev dependency", "Apache-2.0", "GPL-3.0", true, Review, "development dependency"},
		{"agpl in mit", "MIT", "AGPL-3.0", false, Conflict, "network copyleft"},
		{"mpl in apache", "Apache-2.0", "MPL-2.0", false, Review, "weak copyleft"},
		{"dual licence picks mit", "Apache-2.0", "MIT OR GPL-3.0", false, OK, ""},
		{"busl anywhere", "GPL-3.0", "BUSL-1.1", false, Conflict, "not an open-source licence"},
		{"no dep licence", "Apache-2.0", "", false, Review, "No licence found"},
		{"unknown dep licence", "MIT", "Custom", false, Review, "not a licence CodeSynapse recognises"},
		{"apache in gpl2-only", "GPL-2.0-only", "Apache-2.0", false, Conflict, "incompatible with GPL-2.0-only"},
		{"apache or mit in gpl2-only", "GPL-2.0-only", "Apache-2.0 OR MIT", false, OK, ""},
		{"gpl3 in gpl2-only", "GPL-2.0-only", "GPL-3.0", false, Conflict, "cannot be combined"},
		{"apache in gpl3", "GPL-3.0", "Apache-2.0", false, OK, ""},
		{"agpl in gpl3", "GPL-3.0", "AGPL-3.0", false, Review, "network service"},
		{"agpl in agpl", "AGPL-3.0", "AGPL-3.0", false, OK, ""},
		{"gpl in unlicensed project", "", "GPL-3.0", false, Conflict, "no licence found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := Check(tt.project, tt.dep, tt.dev)
			assert.Equal(t, v.Level, tt.want, v.Reason)
			assert.True(t, strings.Contains(v.Reason, tt.reason), v.Reason)
		})
	}
}

func TestDetect(t *testing.T) {
	tests := map[string]string{
		"Apache-2.0":   "                                 Apache License\n                           Version 2.0, January 2004\n",
		"MIT":          "MIT License\n\nPermission is hereby granted, free of charge, to any person obtaining a copy\nof this software",
		"BSD-3-Clause": "Redistribution and use in source and binary forms, with or without\nmodification... * Neither the name of Google Inc. nor",
		"BSD-2-Clause": "Redistribution and use in source and binary forms, with or without modification, are permitted",
		"GPL-3.0":      "GNU GENERAL PUBLIC LICENSE\n Version 3, 29 June 2007",
		"LGPL-2.1":     "GNU LESSER GENERAL PUBLIC LICENSE\n Version 2.1, February 1999 ... GNU GENERAL PUBLIC LICENSE",
		"AGPL-3.0":     "GNU AFFERO GENERAL PUBLIC LICENSE\n Version 3, 19 November 2007 GNU GENERAL PUBLIC LICENSE",
		"MPL-2.0":      "Mozilla Public License Version 2.0\n==================================",
		"ISC":          "Permission to use, copy, modify, and/or distribute this software for any\npurpose with or without fee is hereby granted, provided that the above\ncopyright notice",
		"BUSL-1.1":     "License text copyright (c) 2020 MariaDB Corporation Ab\nBusiness Source License 1.1",
		"MIT OR X":     "// SPDX-License-Identifier: MIT OR X\npackage main",
		"":             "All rights reserved.",
	}
	for want, text := range tests {
		t.Run(want, func(t *testing.T) {
			assert.Equal(t, Detect([]byte(text)), want)
		})
	}
}

func TestFromDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600))
	}
	write("LICENSE-MIT", "Permission is hereby granted, free of charge, to any person obtaining a copy")
	write("LICENSE-APACHE", "Apache License\nVersion 2.0, January 2004")
	write("README.md", "GNU GENERAL PUBLIC LICENSE Version 3")
	got, files := FromDir(dir)
	assert.Equal(t, got, "Apache-2.0 OR MIT")
	assert.Equal(t, files, []string{"LICENSE-APACHE", "LICENSE-MIT"})

	// A symlinked licence file is not followed.
	other := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(dir, "LICENSE-MIT"), filepath.Join(other, "LICENSE")))
	got, _ = FromDir(other)
	assert.Equal(t, got, "")
}

func FuzzClassify(f *testing.F) {
	for _, s := range []string{"MIT", "(MIT OR Apache-2.0) AND GPL-2.0 WITH Classpath-exception-2.0", "((", "OR OR", ")("} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, s string) {
		_ = Classify(s)
		_ = Check(s, s, false)
	})
}

func TestProject(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, Project(dir), Found{})
	writeFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"x\"\nlicense = {text = \"Apache 2.0\"}\n")
	assert.Equal(t, Project(dir), Found{"Apache-2.0", "pyproject.toml"})
	writeFile(t, filepath.Join(dir, "package.json"), `{"license":"MIT"}`)
	assert.Equal(t, Project(dir), Found{"MIT", "package.json"})
	writeFile(t, filepath.Join(dir, "LICENSE"), "GNU AFFERO GENERAL PUBLIC LICENSE Version 3")
	assert.Equal(t, Project(dir), Found{"AGPL-3.0", "LICENSE"})
}
